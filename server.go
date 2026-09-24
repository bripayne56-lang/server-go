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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// How long validation must last before index.html can be served.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50

	// Prevent an immediate second GET / after a successful click
	// from being counted as another click.
	duplicateWindow = 1500 * time.Millisecond

	// If a browser cancels a GET /, this is simply considered a
	// failed attempt. A new GET / from the same cookie is allowed.
	//
	// This value is only used for logging the continuation.
	retryWindow = 3 * time.Second

	// After the page has been flushed, wait briefly before counting.
	// This gives the server time to observe an immediate disconnect.
	responseGrace = 150 * time.Millisecond

	// Browser/client identifier cookie.
	clientCookieName = "click_client_id"
)

// ------------------------------------------------------------
// Browser/client state.
//
// State is keyed by the cookie, NOT by TCP connection.
// ------------------------------------------------------------

type clientState struct {
	mu sync.Mutex

	// Only one GET / may be validating for this browser/client
	// at a time.
	inFlight bool

	// Last time this client completed a valid click.
	lastValid time.Time

	// If a browser disconnects during validation, the next GET /
	// can be treated as a continuation of that navigation.
	retryUntil time.Time
}

var (
	// Protects clientStates.
	clientStatesMu sync.Mutex
	clientStates   = make(map[string]*clientState)

	// Protects validClicks.
	clickMu     sync.Mutex
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

	// --------------------------------------------------------
	// ROUTING
	// --------------------------------------------------------

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
// Determine whether request is HTTPS.
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
// Disable caching.
// ------------------------------------------------------------

func noStore(w http.ResponseWriter) {
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// ------------------------------------------------------------
// 307 COOKIE BOOTSTRAP
//
// First GET / with no cookie:
//     307 + Set-Cookie + Location: /
//
// Browser then makes GET / with the cookie.
//
// No page is served during the bootstrap.
// ------------------------------------------------------------

func bootstrapClient(
	w http.ResponseWriter,
	r *http.Request,
	reqID uint64,
) {
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

		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     clientCookieName,
		Value:    clientID,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})

	noStore(w)

	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusTemporaryRedirect)

	log.Printf(
		"BOOTSTRAP req=%d cookie-issued client=%s",
		reqID,
		shortID(clientID),
	)
}

// ------------------------------------------------------------
// Shorten cookie ID in logs.
// ------------------------------------------------------------

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}

	return id[:8]
}

// ------------------------------------------------------------
// Ignore browser speculative/prefetch requests.
//
// These requests are not user navigations and can be canceled
// immediately when the real navigation begins.
// They must NEVER enter validation.
// ------------------------------------------------------------

func isSpeculativeRequest(r *http.Request) bool {
	purpose := strings.ToLower(
		strings.TrimSpace(r.Header.Get("Purpose")),
	)

	secPurpose := strings.ToLower(
		strings.TrimSpace(r.Header.Get("Sec-Purpose")),
	)

	if purpose == "prefetch" ||
		strings.Contains(secPurpose, "prefetch") {
		return true
	}

	// If Fetch Metadata headers are available, a real page load
	// should be a document navigation.
	dest := strings.ToLower(
		strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")),
	)

	mode := strings.ToLower(
		strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")),
	)

	if dest != "" && dest != "document" {
		return true
	}

	if mode != "" && mode != "navigate" {
		return true
	}

	return false
}

// ------------------------------------------------------------
// Get/create browser state.
// ------------------------------------------------------------

func getClientState(clientID string) *clientState {
	clientStatesMu.Lock()
	defer clientStatesMu.Unlock()

	state := clientStates[clientID]

	if state == nil {
		state = &clientState{}
		clientStates[clientID] = state
	}

	return state
}

// ------------------------------------------------------------
// Begin validation.
//
// IMPORTANT:
//
// A previous failed GET / does NOT block a new GET /.
//
// This is what allows:
//
//   req=1 -> browser cancels
//   req=2 -> browser retries
//
// to work.
//
// Only an active request or a recent successful click blocks
// another request.
// ------------------------------------------------------------

func beginValidation(state *clientState) (bool, string) {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	// Another GET / for this same browser is still active.
	if state.inFlight {
		return false, "validation-already-in-flight"
	}

	// A successful click just happened.
	if !state.lastValid.IsZero() &&
		now.Sub(state.lastValid) < duplicateWindow {
		return false, "recently-succeeded"
	}

	// Determine whether this is a replacement for a request
	// that was just canceled.
	continuation := now.Before(state.retryUntil)

	state.inFlight = true

	if continuation {
		return true, "continuation-after-disconnect"
	}

	return true, "new-validation"
}

// ------------------------------------------------------------
// Validation failed.
//
// IMPORTANT:
//
// We do NOT permanently block the next GET /.
//
// Instead, we simply allow another GET / to take over.
// ------------------------------------------------------------

func finishFailedValidation(state *clientState) {
	state.mu.Lock()

	state.inFlight = false
	state.retryUntil = time.Now().Add(retryWindow)

	state.mu.Unlock()
}

// ------------------------------------------------------------
// Validation succeeded.
//
// Called ONLY after the click has actually been committed.
// ------------------------------------------------------------

func finishSuccessfulValidation(state *clientState) {
	state.mu.Lock()

	state.inFlight = false
	state.retryUntil = time.Time{}
	state.lastValid = time.Now()

	state.mu.Unlock()
}

// ------------------------------------------------------------
// Check actual completed click limit.
//
// IMPORTANT:
//
// ONLY validClicks matters.
//
// No reservations.
// No pending validations.
// No in-flight requests.
//
// Therefore 4 completed + 6 validating can NEVER cause 204.
// ------------------------------------------------------------

func limitReached() bool {
	clickMu.Lock()
	defer clickMu.Unlock()

	return validClicks >= validClickLimit
}

// ------------------------------------------------------------
// Current completed click count.
// ------------------------------------------------------------

func validClickCount() int {
	clickMu.Lock()
	defer clickMu.Unlock()

	return validClicks
}

// ------------------------------------------------------------
// COMMIT A VALID CLICK.
//
// THIS IS THE ONLY PLACE validClicks++ EXISTS.
//
// There is NO other path to incrementing the counter.
// ------------------------------------------------------------

func commitClick(reqID uint64) (bool, int) {
	clickMu.Lock()
	defer clickMu.Unlock()

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
// SERVE VALIDATED PAGE
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

	// --------------------------------------------------------
	// Ignore speculative browser requests.
	// --------------------------------------------------------

	if isSpeculativeRequest(r) {
		log.Printf(
			"REJECT req=%d reason=speculative-request",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// --------------------------------------------------------
	// 204 ONLY when the actual completed limit is reached.
	// --------------------------------------------------------

	if limitReached() {
		log.Printf(
			"REJECT req=%d reason=click-limit completed=%d",
			reqID,
			validClickCount(),
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// --------------------------------------------------------
	// COOKIE BOOTSTRAP
	// --------------------------------------------------------

	clientID, hasCookie := getClientID(r)

	if !hasCookie {
		bootstrapClient(w, r, reqID)
		return
	}

	log.Printf(
		"COOKIE req=%d client=%s",
		reqID,
		shortID(clientID),
	)

	state := getClientState(clientID)

	// --------------------------------------------------------
	// ONE ACTIVE GET / PER CLIENT
	// --------------------------------------------------------

	allowed, mode := beginValidation(state)

	if !allowed {
		log.Printf(
			"REJECT req=%d client=%s reason=%s",
			reqID,
			shortID(clientID),
			mode,
		)

		// Duplicate requests can return 204.
		// They DO NOT count.
		w.WriteHeader(http.StatusNoContent)

		return
	}

	log.Printf(
		"VALIDATING req=%d client=%s mode=%s",
		reqID,
		shortID(clientID),
		mode,
	)

	// Assume failure until the final commit succeeds.
	successful := false

	defer func() {
		if !successful {
			finishFailedValidation(state)

			log.Printf(
				"FAILED req=%d client=%s retry-window=%s",
				reqID,
				shortID(clientID),
				retryWindow,
			)
		}
	}()

	// --------------------------------------------------------
	// GLOBAL VALIDATION CONCURRENCY LIMIT
	// --------------------------------------------------------

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

		// Not a valid click.
		w.WriteHeader(http.StatusNoContent)

		return
	}

	// --------------------------------------------------------
	// FULL ONE-SECOND VALIDATION
	// --------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():

		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-during-validation",
			reqID,
			shortID(clientID),
		)

		// No commit.
		return

	case <-timer.C:
	}

	// --------------------------------------------------------
	// Cancellation check after one second.
	// --------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-after-validation err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// --------------------------------------------------------
	// Check actual completed limit.
	// --------------------------------------------------------

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

	// --------------------------------------------------------
	// READ INDEX.HTML
	// --------------------------------------------------------

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

	// --------------------------------------------------------
	// Check again before response.
	// --------------------------------------------------------

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

	// --------------------------------------------------------
	// RESPONSE HEADERS
	// --------------------------------------------------------

	noStore(w)

	w.Header().Set(
		"Content-Type",
		"text/html; charset=utf-8",
	)

	w.Header().Set(
		"X-Content-Type-Options",
		"nosniff",
	)

	// --------------------------------------------------------
	// SEND INDEX.HTML
	//
	// IMPORTANT:
	// This still does NOT immediately count the click.
	// --------------------------------------------------------

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=response-write-failed err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// --------------------------------------------------------
	// FLUSH THE RESPONSE
	// --------------------------------------------------------

	if err := http.NewResponseController(w).Flush(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=response-flush-failed err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	log.Printf(
		"PAGE-SENT req=%d client=%s",
		reqID,
		shortID(clientID),
	)

	// --------------------------------------------------------
	// IMPORTANT:
	//
	// Keep this request marked inFlight during this period.
	//
	// Therefore another GET / from the same cookie cannot sneak
	// through and become another click while this request is
	// finishing.
	// --------------------------------------------------------

	graceTimer := time.NewTimer(responseGrace)
	defer graceTimer.Stop()

	select {
	case <-r.Context().Done():

		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-after-response",
			reqID,
			shortID(clientID),
		)

		// No commit.
		return

	case <-graceTimer.C:
	}

	// --------------------------------------------------------
	// FINAL CANCELLATION CHECK
	// --------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d client=%s reason=disconnect-final err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// --------------------------------------------------------
	// FINAL COMMIT.
	//
	// ONLY HERE can the click become valid.
	// --------------------------------------------------------

	ok, clickNumber := commitClick(reqID)

	if !ok {
		// Another request reached the limit first.
		//
		// Do not count this request.
		return
	}

	// Mark this browser/client successful only AFTER the
	// click has actually been counted.
	finishSuccessfulValidation(state)

	successful = true

	log.Printf(
		"COMPLETED req=%d client=%s click=%d/%d",
		reqID,
		shortID(clientID),
		clickNumber,
		validClickLimit,
	)
}
