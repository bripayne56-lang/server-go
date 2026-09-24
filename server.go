package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	validationTime    = 1 * time.Second
	validClickLimit   = 10 
	maxConcurrentWaits = 50
	duplicateWindow   = 1500 * time.Millisecond
)

var (
	validClicks  int64
	reservedClicks int64
	requestID    int64

	waitSlots = make(chan struct{}, maxConcurrentWaits)

	page []byte

	clientsMu sync.Mutex
	clients   = make(map[string]*clientState)
)

type clientState struct {
	mu         sync.Mutex
	inFlight   bool
	lastFailed time.Time
}

// ---------- Client cookie ----------

func newClientID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely, but don't continue with a predictable ID.
		panic(err)
	}

	return fmt.Sprintf("%x", b)
}

func getClientState(clientID string) *clientState {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	state, ok := clients[clientID]
	if !ok {
		state = &clientState{}
		clients[clientID] = state
	}

	return state
}

// ---------- Click reservation ----------

func reserveClick() bool {
	for {
		completed := atomic.LoadInt64(&validClicks)
		reserved := atomic.LoadInt64(&reservedClicks)

		if completed+reserved >= validClickLimit {
			return false
		}

		if atomic.CompareAndSwapInt64(
			&reservedClicks,
			reserved,
			reserved+1,
		) {
			return true
		}
	}
}

func releaseReservation() {
	atomic.AddInt64(&reservedClicks, -1)
}

func commitClick() (int64, bool) {
	atomic.AddInt64(&reservedClicks, -1)

	for {
		current := atomic.LoadInt64(&validClicks)

		if current >= validClickLimit {
			return current, false
		}

		if atomic.CompareAndSwapInt64(
			&validClicks,
			current,
			current+1,
		) {
			return current + 1, true
		}
	}
}

// ---------- Main handler ----------

func handler(w http.ResponseWriter, r *http.Request) {
	reqID := atomic.AddInt64(&requestID, 1)

	log.Printf(
		"REQUEST req=%d method=%s path=%s",
		reqID,
		r.Method,
		r.URL.Path,
	)

	// Only GET /
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// 1. COOKIE BOOTSTRAP
	//
	// The very first GET / receives a cookie and a 307.
	// It does NOT receive index.html.
	// ------------------------------------------------------------

	cookie, err := r.Cookie("click_client")

	if err != nil || cookie.Value == "" {
		clientID := newClientID()

		secure := r.TLS != nil ||
			strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")

		http.SetCookie(w, &http.Cookie{
			Name:     "click_client",
			Value:    clientID,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400,
		})

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusTemporaryRedirect)

		log.Printf(
			"BOOTSTRAP req=%d cookie-issued client=%s",
			reqID,
			clientID,
		)

		return
	}

	clientID := cookie.Value

	log.Printf(
		"COOKIE req=%d client=%s",
		reqID,
		clientID,
	)

	state := getClientState(clientID)

	// ------------------------------------------------------------
	// 2. PREVENT A RAPID RETRY AFTER A FAILED/DISCONNECTED REQUEST
	//
	// IMPORTANT:
	// We do NOT reject recently-successful requests.
	// ------------------------------------------------------------

	state.mu.Lock()

	if state.inFlight {
		state.mu.Unlock()

		log.Printf(
			"REJECT req=%d client=%s reason=validation-already-in-flight",
			reqID,
			clientID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !state.lastFailed.IsZero() &&
		time.Since(state.lastFailed) < duplicateWindow {

		state.mu.Unlock()

		log.Printf(
			"REJECT req=%d client=%s reason=recently-failed",
			reqID,
			clientID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	state.inFlight = true
	state.mu.Unlock()

	validationSucceeded := false

	defer func() {
		state.mu.Lock()

		state.inFlight = false

		if !validationSucceeded {
			state.lastFailed = time.Now()
		}

		state.mu.Unlock()

		if !validationSucceeded {
			log.Printf(
				"RELEASED req=%d reason=validation-failed",
				reqID,
			)
		}
	}()

	// ------------------------------------------------------------
	// 3. RESERVE A GLOBAL CLICK SLOT
	// ------------------------------------------------------------

	if !reserveClick() {
		log.Printf(
			"REJECT req=%d client=%s reason=click-limit",
			reqID,
			clientID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservationHeld := true

	defer func() {
		if reservationHeld {
			releaseReservation()
		}
	}()

	log.Printf(
		"RESERVED req=%d completed=%d validating=%d",
		reqID,
		atomic.LoadInt64(&validClicks),
		atomic.LoadInt64(&reservedClicks),
	)

	// ------------------------------------------------------------
	// 4. LIMIT CONCURRENT VALIDATIONS
	// ------------------------------------------------------------

	select {
	case waitSlots <- struct{}{}:
		defer func() {
			<-waitSlots
		}()

	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-before-validation",
			reqID,
			clientID,
		)
		return
	}

	// ------------------------------------------------------------
	// 5. ONE-SECOND VALIDATION
	// ------------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-timer.C:
		// Validation time completed.

	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-during-validation",
			reqID,
			clientID,
		)
		return
	}

	// Check again after the timer fires.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-after-validation",
			reqID,
			clientID,
		)
		return
	}

	// ------------------------------------------------------------
	// 6. READ INDEX
	// ------------------------------------------------------------

	if page == nil {
		log.Printf(
			"REJECT req=%d client=%s reason=index-not-loaded",
			reqID,
			clientID,
		)

		return
	}

	// ------------------------------------------------------------
	// 7. COMMIT THE CLICK BEFORE SENDING THE PAGE
	//
	// This makes the 40/10 limit authoritative before a 200 is sent.
	// ------------------------------------------------------------

	clickNumber, ok := commitClick()

	if !ok {
		reservationHeld = false

		log.Printf(
			"REJECT req=%d client=%s reason=click-limit",
			reqID,
			clientID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservationHeld = false
	validationSucceeded = true

	log.Printf(
		"VALID req=%d click=%d/%d",
		reqID,
		clickNumber,
		validClickLimit,
	)

	// ------------------------------------------------------------
	// 8. SEND INDEX.HTML
	// ------------------------------------------------------------

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"WRITE-ERROR req=%d client=%s err=%v",
			reqID,
			clientID,
			err,
		)

		return
	}

	log.Printf(
		"COMPLETED req=%d client=%s click=%d/%d",
		reqID,
		clientID,
		clickNumber,
		validClickLimit,
	)
}

// ---------- Main ----------

func main() {
	var err error

	page, err = os.ReadFile("public/index.html")
	if err != nil {
		log.Fatalf("failed to read public/index.html: %v", err)
	}

	http.HandleFunc("/", handler)

	server := &http.Server{
		Addr: ":8080",

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return ctx
		},
	}

	log.Printf(
		"server listening on %s clickLimit=%d validation=%s",
		server.Addr,
		validClickLimit,
		validationTime,
	)

	if err := server.ListenAndServe(); err != nil &&
		err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
