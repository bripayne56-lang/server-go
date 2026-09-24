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

	// Small grace period after the response has been flushed.
	//
	// This gives the server a chance to observe a client disconnect
	// that happens immediately around response completion.
	postResponseGrace = 150 * time.Millisecond

	// Browser/client identifier cookie.
	clientCookieName = "click_client_id"
)

// State belongs to a browser/client cookie.
//
// This is intentionally NOT based on TCP connection identity.
type clientState struct {
	mu sync.Mutex

	// A validation is currently running for this client.
	inFlight bool

	// Time when this client last completed a valid click.
	lastValid time.Time

	// Time when this client's most recent validation failed.
	lastFailed time.Time
}

var (
	// Protects the clientStates map.
	clientStatesMu sync.Mutex
	clientStates   = make(map[string]*clientState)

	// Protects validClicks.
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

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

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return ctx
		},
	}

	log.Fatal(server.ListenAndServe())
}

// ------------------------------------------------------------
// Generate random browser/client ID.
// ------------------------------------------------------------

func newClientID() (string, error) {
	var b [32]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

// ------------------------------------------------------------
// Determine whether incoming request is HTTPS.
// ------------------------------------------------------------

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// ------------------------------------------------------------
// Read client cookie.
// ------------------------------------------------------------

func getClientID(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(clientCookieName)

	if err != nil || cookie.Value == "" {
		return "", false
	}

	return cookie.Value, true
}

// ------------------------------------------------------------
// Bootstrap cookie with 307.
//
// This request:
//   - does not validate
//   - does not count
//   - does not serve index.html
//
// Browser receives the cookie and follows the redirect,
// producing the real GET / with the cookie.
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
// Shorten IDs in logs.
// ------------------------------------------------------------

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}

	return id[:8]
}

// ------------------------------------------------------------
// Get client state.
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
// Begin validation.
//
// Reject when:
//   1. another validation is already running
//   2. client recently succeeded
//   3. client recently failed
//
// These rejected requests return 204, but NEVER count.
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
// Finish validation.
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
// Check whether the ACTUAL completed click limit is reached.
//
// IMPORTANT:
// This checks ONLY validClicks.
//
// It does NOT consider in-flight requests.
// Therefore an in-flight request cannot cause an early 204.
// ------------------------------------------------------------

func limitReached() bool {
	mu.Lock()
	defer mu.Unlock()

	return validClicks >= validClickLimit
}

// ------------------------------------------------------------
// Get current valid click count.
// ------------------------------------------------------------

func validClickCount() int {
	mu.Lock()
	defer mu.Unlock()

	return validClicks
}

// ------------------------------------------------------------
// Commit a valid click.
//
// THIS IS THE ONLY PLACE validClicks IS INCREMENTED.
//
// A request must make it all the way through validation,
// response writing, flushing, and final cancellation checks
// before getting here.
// ------------------------------------------------------------

func commitClick(reqID uint64) (bool, int) {
	mu.Lock()
	defer mu.Unlock()

	// Final protection against exceeding the lifetime limit.
	if validClicks >= validClickLimit {
		log.Printf(
			"REJECT req=%d reason=limit-reached-before-count completed=%d",
			reqID,
			validClicks,
		)

		return false, 0
	}

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
	// 204 because the ACTUAL completed click limit is reached.
	// ------------------------------------------------------------

	if limitReached() {
		log.Printf(
			"REJECT req=%d reason=click-limit completed=%d",
			reqID,
			validClickCount(),
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// COOKIE BOOTSTRAP
	// ------------------------------------------------------------

	clientID, hasCookie := getClientID(r)

	if !hasCookie {
		// Bootstrap only.
		//
		// No validation.
		// No reservation.
		// No index.html.
		// No click count.
		bootstrapClient(w, r, reqID)

		return
	}

	log.Printf(
		"COOKIE req=%d client=%s",
		reqID,
		shortID(clientID),
	)

	state := getClientState(clientID)

	// ------------------------------------------------------------
	// CLIENT DUPLICATE PROTECTION
	// ------------------------------------------------------------

	ok, reason := beginValidation(state)

	if !ok {
		log.Printf(
			"REJECT req=%d client=%s reason=%s",
			reqID,
			shortID(clientID),
			reason,
		)

		// Duplicate/failed requests can return 204.
		// They NEVER call commitClick().
		w.WriteHeader(http.StatusNoContent)
		return
	}

	successful := false

	defer func() {
		finishValidation(state, successful)
	}()

	// ------------------------------------------------------------
	// GLOBAL VALIDATION CONCURRENCY LIMIT
	// ------------------------------------------------------------

	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		log.Printf(
			"REJECT req=%d client=%s reason=validation-capacity",
			reqID,
			shortID(clientID),
		)

		// Capacity rejection is also 204, but is NOT counted.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// ONE-SECOND VALIDATION
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

		// Do NOT call commitClick().
		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Check immediately after the one-second validation.
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
	// If limit was reached by another completed request while
	// this request was waiting, return 204.
	// ------------------------------------------------------------

	if limitReached() {
		log.Printf(
			"REJECT req=%d client=%s reason=click-limit-after-validation completed=%d",
			reqID,
			shortID(clientID),
			validClickCount(),
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// READ INDEX.HTML
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

	if limitReached() {
		log.Printf(
			"REJECT req=%d client=%s reason=click-limit-before-response completed=%d",
			reqID,
			shortID(clientID),
			validClickCount(),
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// RESPONSE HEADERS
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
	// This happens ONLY after the full one-second validation.
	// ------------------------------------------------------------

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
	// POST-RESPONSE GRACE PERIOD.
	//
	// This attempts to catch a browser/client disconnect that
	// happens immediately after the response is sent.
	// ------------------------------------------------------------

	graceTimer := time.NewTimer(postResponseGrace)
	defer graceTimer.Stop()

	select {
	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-post-response",
			reqID,
			shortID(clientID),
		)

		// Do NOT call commitClick().
		return

	case <-graceTimer.C:
	}

	// ------------------------------------------------------------
	// FINAL CANCELLATION CHECK.
	// ------------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-final err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// FINAL COMMIT.
	//
	// THIS IS THE ONLY PLACE THAT CAN GENERATE "VALID".
	// ------------------------------------------------------------

	ok, clickNumber := commitClick(reqID)

	if !ok {
		// Another request reached the limit first.
		//
		// The response has already been sent, so we cannot turn
		// that already-sent response into a 204.
		//
		// Most importantly: this request is NOT counted.
		log.Printf(
			"NOT-COUNTED req=%d client=%s reason=limit-reached-after-response",
			reqID,
			shortID(clientID),
		)

		return
	}

	successful = true

	log.Printf(
		"COMPLETED req=%d client=%s click=%d/%d",
		reqID,
		shortID(clientID),
		clickNumber,
		validClickLimit,
	)
}
