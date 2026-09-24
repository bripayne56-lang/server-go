package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// How long validation must last.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50

	// After a successful click OR failed validation, suppress
	// an immediate follow-up GET / using the same client cookie.
	duplicateWindow = 1500 * time.Millisecond

	// Browser/client identifier cookie.
	clientCookieName = "click_client_id"
)

// State belongs to a browser/client cookie.
//
// This replaces the old TCP-connection-based state.
//
// No IP address is stored or examined.
type clientState struct {
	mu sync.Mutex

	// A validation is currently running for this client.
	inFlight bool

	// Time when this client last completed a valid click.
	lastValid time.Time

	// Time when this client's most recent validation failed.
	//
	// This is important for the problem you showed:
	//
	// req=1 -> disconnect -> rejected
	// req=2 -> immediate GET / with same cookie
	//
	// req=2 will be rejected rather than becoming another click.
	lastFailed time.Time
}

var (
	// Protects the clientStates map.
	clientStatesMu sync.Mutex

	// Client state keyed by the random browser cookie.
	clientStates = make(map[string]*clientState)

	// Protects validClicks and reservedClicks.
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Clicks currently being validated.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)

	requestID atomic.Uint64
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// ------------------------------------------------------------
	// GET /
	// ------------------------------------------------------------
	//
	// Only GET / enters the one-second validation flow.
	//
	// The first-ever GET / has no cookie, so we bootstrap the
	// cookie with a 307 redirect. The browser then makes another
	// GET / containing the cookie.
	//
	// index.html is NOT served during the bootstrap.
	// ------------------------------------------------------------

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		serveValidatedPage(w, r, filePath)
	})

	log.Println("Server running on port", port)

	server := &http.Server{
		Addr: ":" + port,

		// Keep connection context available even though client
		// identity is now cookie-based.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return ctx
		},
	}

	log.Fatal(server.ListenAndServe())
}

// ------------------------------------------------------------
// Generate a random browser/client ID.
// ------------------------------------------------------------

func newClientID() (string, error) {
	var b [32]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

// ------------------------------------------------------------
// Determine whether the incoming request is HTTPS.
//
// This allows Secure cookies to work both when Go is directly
// serving HTTPS and when HTTPS is terminated by a proxy.
// ------------------------------------------------------------

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// ------------------------------------------------------------
// Get or create the browser/client cookie.
//
// Returns:
//
//   clientID
//   hasCookie
//
// When no cookie exists, this function DOES NOT start validation.
// The caller must issue the bootstrap redirect.
// ------------------------------------------------------------

func getClientID(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(clientCookieName)

	if err != nil || cookie.Value == "" {
		return "", false
	}

	return cookie.Value, true
}

// ------------------------------------------------------------
// Create a cookie and immediately redirect back to /.
//
// This is the bootstrap step:
//
// GET /
//   ↓
// 307 + Set-Cookie
//   ↓
// browser follows redirect
//   ↓
// GET / + Cookie
// ------------------------------------------------------------

func bootstrapClient(
	w http.ResponseWriter,
	r *http.Request,
	reqID uint64,
) bool {
	clientID, err := newClientID()
	if err != nil {
		log.Printf(
			"ERROR req=%d reason=client-id-generation err=%v",
			reqID,
			err,
		)

		http.Error(
			w,
			"internal server error",
			http.StatusInternalServerError,
		)

		return false
	}

	secure := requestIsHTTPS(r)

	http.SetCookie(w, &http.Cookie{
		Name:     clientCookieName,
		Value:    clientID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Redirect back to the exact same GET /.
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusTemporaryRedirect)

	log.Printf(
		"BOOTSTRAP req=%d cookie-issued client=%s secure=%t",
		reqID,
		shortID(clientID),
		secure,
	)

	return true
}

// ------------------------------------------------------------
// Shorten IDs in logs so the complete cookie value is not dumped.
// ------------------------------------------------------------

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}

	return id[:8]
}

// ------------------------------------------------------------
// Get client state.
//
// This state survives TCP connection changes because it is keyed
// by the browser cookie instead of net.Conn.
// ------------------------------------------------------------

func getClientState(clientID string) *clientState {
	clientStatesMu.Lock()
	defer clientStatesMu.Unlock()

	state, ok := clientStates[clientID]

	if !ok {
		state = &clientState{}
		clientStates[clientID] = state
	}

	return state
}

// ------------------------------------------------------------
// Begin validation for a client.
//
// Reject when:
//
// 1. Another validation is already running.
// 2. The client just completed a valid click.
// 3. The client just failed a validation.
//
// The third case specifically prevents:
//
// req=1 -> early disconnect
// req=2 -> immediate retry
//
// from turning req=2 into a valid click.
// ------------------------------------------------------------

func beginValidation(state *clientState) (bool, string) {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.inFlight {
		return false, "validation-already-in-flight"
	}

	if !state.lastValid.IsZero() &&
		now.Sub(state.lastValid) < duplicateWindow {
		return false, "recently-succeeded"
	}

	if !state.lastFailed.IsZero() &&
		now.Sub(state.lastFailed) < duplicateWindow {
		return false, "recently-failed"
	}

	state.inFlight = true

	return true, ""
}

// ------------------------------------------------------------
// Finish validation and record the result for this client.
// ------------------------------------------------------------

func finishValidation(
	state *clientState,
	successful bool,
) {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	state.inFlight = false

	if successful {
		state.lastValid = now
		state.lastFailed = time.Time{}
		return
	}

	state.lastFailed = now
}

// ------------------------------------------------------------
// Reserve a lifetime click slot.
//
// This preserves your existing 40-click limit semantics.
// ------------------------------------------------------------

func reserveClick(reqID uint64) bool {
	mu.Lock()
	defer mu.Unlock()

	if validClicks+reservedClicks >= validClickLimit {
		log.Printf(
			"REJECT req=%d reason=click-limit completed=%d validating=%d",
			reqID,
			validClicks,
			reservedClicks,
		)

		return false
	}

	reservedClicks++

	log.Printf(
		"RESERVED req=%d completed=%d validating=%d",
		reqID,
		validClicks,
		reservedClicks,
	)

	return true
}

// ------------------------------------------------------------
// Release a reservation after validation fails.
// ------------------------------------------------------------

func releaseReservation(
	reqID uint64,
	reason string,
) {
	mu.Lock()

	if reservedClicks > 0 {
		reservedClicks--
	}

	mu.Unlock()

	log.Printf(
		"RELEASED req=%d reason=%s",
		reqID,
		reason,
	)
}

// ------------------------------------------------------------
// Commit a valid click.
//
// THIS IS THE ONLY PLACE validClicks is incremented.
//
// Therefore VALID can only appear when this function succeeds.
// ------------------------------------------------------------

func commitClick(reqID uint64) (bool, int) {
	mu.Lock()
	defer mu.Unlock()

	// Final protection against exceeding 40.
	if validClicks >= validClickLimit {
		log.Printf(
			"REJECT req=%d reason=limit-reached-before-count completed=%d validating=%d",
			reqID,
			validClicks,
			reservedClicks,
		)

		return false, 0
	}

	if reservedClicks <= 0 {
		log.Printf(
			"ERROR req=%d reason=missing-reservation",
			reqID,
		)

		return false, 0
	}

	reservedClicks--
	validClicks++

	clickNumber := validClicks

	log.Printf(
		"VALID req=%d click=%d/%d",
		reqID,
		clickNumber,
		validClickLimit,
	)

	return true, clickNumber
}

// ------------------------------------------------------------
// Serve the one-second validated page.
// ------------------------------------------------------------

func serveValidatedPage(
	w http.ResponseWriter,
	r *http.Request,
	filePath string,
) {
	reqID := requestID.Add(1)

	log.Printf(
		"REQUEST req=%d method=%s path=%s",
		reqID,
		r.Method,
		r.URL.Path,
	)

	// ------------------------------------------------------------
	// FIRST: preserve the 40-click -> 204 behavior.
	// ------------------------------------------------------------

	mu.Lock()
	limitAlreadyReached :=
		validClicks+reservedClicks >= validClickLimit
	mu.Unlock()

	if limitAlreadyReached {
		log.Printf(
			"REJECT req=%d reason=click-limit",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// COOKIE BOOTSTRAP
	// ------------------------------------------------------------

	clientID, hasCookie := getClientID(r)

	if !hasCookie {
		// This request is ONLY for establishing the client ID.
		//
		// It does NOT validate.
		// It does NOT reserve a click.
		// It does NOT serve index.html.
		//
		// Browser receives:
		//
		//   Set-Cookie
		//   307 Location: /
		//
		// and automatically makes GET / again.
		bootstrapClient(w, r, reqID)

		return
	}

	log.Printf(
		"COOKIE req=%d client=%s",
		reqID,
		shortID(clientID),
	)

	clientState := getClientState(clientID)

	// ------------------------------------------------------------
	// CLIENT-LEVEL DUPLICATE PROTECTION.
	// ------------------------------------------------------------

	ok, reason := beginValidation(clientState)

	if !ok {
		log.Printf(
			"REJECT req=%d client=%s reason=%s",
			reqID,
			shortID(clientID),
			reason,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	completed := false
	reservationHeld := false

	// ------------------------------------------------------------
	// Cleanup.
	// ------------------------------------------------------------

	defer func() {
		// Record success/failure on the cookie-based client state.
		finishValidation(
			clientState,
			completed,
		)

		// Return a global reservation if this click never became
		// valid.
		if !completed && reservationHeld {
			releaseReservation(
				reqID,
				"validation-failed",
			)
		}
	}()

	// ------------------------------------------------------------
	// GLOBAL VALIDATION CONCURRENCY LIMIT.
	// ------------------------------------------------------------

	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		log.Printf(
			"REJECT req=%d reason=validation-capacity",
			reqID,
		)

		return
	}

	// ------------------------------------------------------------
	// RESERVE ONE OF THE 40 CLICK SLOTS.
	// ------------------------------------------------------------

	if !reserveClick(reqID) {
		// Important:
		// The user receives 204 when the limit has been reached.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservationHeld = true

	// ------------------------------------------------------------
	// ONE-SECOND VALIDATION.
	// ------------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-during-validation",
			reqID,
			shortID(clientID),
		)

		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// The full one second has elapsed.
	//
	// Check whether the request was cancelled.
	// ------------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-after-validation err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// READ INDEX.HTML.
	// ------------------------------------------------------------

	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf(
			"ERROR req=%d reason=read-index err=%v",
			reqID,
			err,
		)

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// ------------------------------------------------------------
	// CHECK AGAIN BEFORE RESPONSE.
	// ------------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-before-response",
			reqID,
			shortID(clientID),
		)

		return
	}

	// ------------------------------------------------------------
	// RESPONSE HEADERS.
	// ------------------------------------------------------------

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set(
		"Content-Type",
		"text/html; charset=utf-8",
	)
	w.Header().Set(
		"X-Content-Type-Options",
		"nosniff",
	)

	// ------------------------------------------------------------
	// SERVE PAGE.
	//
	// This does NOT happen until the full one-second validation
	// has completed.
	// ------------------------------------------------------------

	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=response-write-failed err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// FLUSH RESPONSE.
	// ------------------------------------------------------------

	rc := http.NewResponseController(w)

	if err := rc.Flush(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=response-flush-failed err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// FINAL CANCELLATION CHECK.
	// ------------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-after-response",
			reqID,
			shortID(clientID),
		)

		return
	}

	// ------------------------------------------------------------
	// FINAL COMMIT.
	//
	// VALID can ONLY be generated by commitClick().
	// ------------------------------------------------------------

	ok, clickNumber := commitClick(reqID)

	if !ok {
		// The global limit won the race.
		//
		// Preserve your 204 behavior.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// The reservation has now been consumed.
	reservationHeld = false

	// Mark this request successful.
	completed = true

	log.Printf(
		"COMPLETED req=%d client=%s click=%d/%d",
		reqID,
		shortID(clientID),
		clickNumber,
		validClickLimit,
	)
}
