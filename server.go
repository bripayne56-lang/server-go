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

	// After a successful click, suppress an immediate follow-up
	// GET / using the same client cookie.
	duplicateWindow = 1500 * time.Millisecond

	// How long a served page has to send its acknowledgement.
	ackTimeout = 10 * time.Second

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

	// Only one GET / validation can be active for this browser ID.
	inFlight bool

	// Last time this client completed a valid click.
	lastValid time.Time

	// Exactly one page/attempt can be waiting for its ACK.
	pendingToken string
	pendingAt    time.Time
}

var (
	// Protects clientStates.
	clientStatesMu sync.Mutex
	clientStates   = make(map[string]*clientState)

	// Protects validClicks.
	clickMu     sync.Mutex
	validClicks int

	// Limits simultaneous one-second validations.
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
	// HTTP ROUTING
	// --------------------------------------------------------

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/__click_ack":
			handleAck(w, r)

		case r.Method == http.MethodGet && r.URL.Path == "/":
			serveValidatedPage(w, r, filePath)

		default:
			w.WriteHeader(http.StatusNoContent)
		}
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
// Random value generator.
// ------------------------------------------------------------

func newRandomHex(n int) (string, error) {
	b := make([]byte, n)

	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

func newClientID() (string, error) {
	return newRandomHex(32)
}

func newAttemptToken() (string, error) {
	return newRandomHex(32)
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
// Bootstrap cookie with 307.
//
// This request does NOT:
//   - validate
//   - count
//   - serve index.html
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

	return true
}

// ------------------------------------------------------------
// Short log ID.
// ------------------------------------------------------------

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}

	return id[:8]
}

// ------------------------------------------------------------
// Get/create state for a client cookie.
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
// A previous FAILED GET / does NOT block this request.
//
// Only these can cause rejection:
//   - another validation is already running
//   - a page is currently waiting for ACK
//   - this client just completed a valid click
// ------------------------------------------------------------

func beginValidation(state *clientState) (bool, string) {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.inFlight {
		return false, "validation-already-in-flight"
	}

	if state.pendingToken != "" {
		if now.Sub(state.pendingAt) < ackTimeout {
			return false, "awaiting-acknowledgement"
		}

		// ACK expired.
		state.pendingToken = ""
		state.pendingAt = time.Time{}
	}

	if !state.lastValid.IsZero() &&
		now.Sub(state.lastValid) < duplicateWindow {
		return false, "recently-succeeded"
	}

	state.inFlight = true

	return true, ""
}

// ------------------------------------------------------------
// Mark a GET / validation as failed.
//
// There is intentionally NO "lastFailed" state.
//
// A failed GET / does not block a later GET /.
// ------------------------------------------------------------

func failValidation(state *clientState) {
	state.mu.Lock()
	state.inFlight = false
	state.mu.Unlock()
}

// ------------------------------------------------------------
// Put a successfully prepared page into "awaiting ACK" state.
// ------------------------------------------------------------

func setPendingAck(
	state *clientState,
	token string,
) {
	state.mu.Lock()

	state.inFlight = false
	state.pendingToken = token
	state.pendingAt = time.Now()

	state.mu.Unlock()
}

// ------------------------------------------------------------
// Cancel a pending ACK.
// ------------------------------------------------------------

func cancelPendingAck(state *clientState) {
	state.mu.Lock()

	state.inFlight = false
	state.pendingToken = ""
	state.pendingAt = time.Time{}

	state.mu.Unlock()
}

// ------------------------------------------------------------
// Validate and consume the browser ACK token.
//
// The token must exactly match the currently pending page.
// ------------------------------------------------------------

func acknowledge(
	state *clientState,
	token string,
) bool {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.pendingToken == "" {
		return false
	}

	if state.pendingToken != token {
		return false
	}

	if now.Sub(state.pendingAt) >= ackTimeout {
		state.pendingToken = ""
		state.pendingAt = time.Time{}
		return false
	}

	// Consume token so it cannot be reused.
	state.pendingToken = ""
	state.pendingAt = time.Time{}

	return true
}

// ------------------------------------------------------------
// Mark client successful.
// ------------------------------------------------------------

func markClientSuccessful(state *clientState) {
	state.mu.Lock()
	state.lastValid = time.Now()
	state.mu.Unlock()
}

// ------------------------------------------------------------
// Check actual completed click limit.
//
// ONLY validClicks matters.
//
// In-flight requests do NOT cause a 204.
// ------------------------------------------------------------

func limitReached() bool {
	clickMu.Lock()
	defer clickMu.Unlock()

	return validClicks >= validClickLimit
}

// ------------------------------------------------------------
// Get current valid click count.
// ------------------------------------------------------------

func validClickCount() int {
	clickMu.Lock()
	defer clickMu.Unlock()

	return validClicks
}

// ------------------------------------------------------------
// Commit a valid click.
//
// THIS IS THE ONLY PLACE validClicks IS INCREMENTED.
//
// GET / NEVER calls this directly.
// Only /__click_ack reaches this function.
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
// Create browser ACK JavaScript.
//
// The source public/index.html is NOT modified.
// ------------------------------------------------------------

func makeAckScript(token string) string {
	return `<script>
(function() {
	var token = "` + token + `";
	var body = "token=" + encodeURIComponent(token);

	try {
		if (navigator.sendBeacon) {
			var blob = new Blob(
				[body],
				{type: "application/x-www-form-urlencoded;charset=UTF-8"}
			);

			if (navigator.sendBeacon("/__click_ack", blob)) {
				return;
			}
		}
	} catch (e) {}

	try {
		fetch("/__click_ack", {
			method: "POST",
			headers: {
				"Content-Type":
					"application/x-www-form-urlencoded;charset=UTF-8"
			},
			body: body,
			keepalive: true,
			credentials: "same-origin"
		});
	} catch (e) {}
})();
</script>`
}

// ------------------------------------------------------------
// Inject ACK script before </body>.
//
// If no </body> exists, append the script.
// ------------------------------------------------------------

func injectAckScript(
	page []byte,
	token string,
) []byte {
	script := makeAckScript(token)

	lower := strings.ToLower(string(page))

	index := strings.LastIndex(
		lower,
		"</body>",
	)

	if index >= 0 {
		out := make(
			[]byte,
			0,
			len(page)+len(script),
		)

		out = append(
			out,
			page[:index]...,
		)

		out = append(
			out,
			script...,
		)

		out = append(
			out,
			page[index:]...,
		)

		return out
	}

	out := make(
		[]byte,
		0,
		len(page)+len(script)+1,
	)

	out = append(out, page...)
	out = append(out, script...)
	out = append(out, '\n')

	return out
}

// ------------------------------------------------------------
// ACK endpoint.
//
// THIS is what makes a click valid.
// ------------------------------------------------------------

func handleAck(
	w http.ResponseWriter,
	r *http.Request,
) {
	reqID := requestID.Add(1)

	log.Printf(
		"ACK-REQUEST req=%d method=%s path=%s",
		reqID,
		r.Method,
		r.URL.Path,
	)

	// Only POST acknowledgements are accepted.
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	clientID, ok := getClientID(r)

	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	token := r.FormValue("token")

	if token == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	state := getClientState(clientID)

	// Token must match the exact page currently awaiting ACK.
	if !acknowledge(state, token) {
		log.Printf(
			"ACK-REJECT req=%d client=%s reason=invalid-or-expired-token",
			reqID,
			shortID(clientID),
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ONLY NOW can the click become valid.
	ok, clickNumber := commitClick(reqID)

	if !ok {
		log.Printf(
			"ACK-NOT-COUNTED req=%d client=%s reason=click-limit",
			reqID,
			shortID(clientID),
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	markClientSuccessful(state)

	log.Printf(
		"ACK-VALID req=%d client=%s click=%d/%d",
		reqID,
		shortID(clientID),
		clickNumber,
		validClickLimit,
	)

	// ACK response intentionally returns no content.
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------
// GET / validation.
//
// IMPORTANT:
//
// GET / NEVER increments validClicks.
//
// It can only:
//   - bootstrap cookie
//   - reject
//   - wait one second
//   - serve the page
//
// The browser ACK is what counts the click.
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
	// 204 ONLY when the actual completed lifetime limit is reached.
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
	// DUPLICATE PROTECTION
	// --------------------------------------------------------

	ok, reason := beginValidation(state)

	if !ok {
		log.Printf(
			"REJECT req=%d client=%s reason=%s",
			reqID,
			shortID(clientID),
			reason,
		)

		// Duplicate/rejected requests can return 204.
		// They NEVER call commitClick().
		w.WriteHeader(http.StatusNoContent)
		return
	}

	validationActive := true

	defer func() {
		if validationActive {
			failValidation(state)

			log.Printf(
				"FAILED req=%d client=%s",
				reqID,
				shortID(clientID),
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

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// --------------------------------------------------------
	// ONE-SECOND VALIDATION
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

		// This request cannot count.
		return

	case <-timer.C:
	}

	// --------------------------------------------------------
	// Check cancellation immediately after one second.
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
	// Check cancellation before response.
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
	// Create unique acknowledgement token.
	// --------------------------------------------------------

	token, err := newAttemptToken()

	if err != nil {
		log.Printf(
			"ERROR req=%d reason=attempt-token-generation err=%v",
			reqID,
			err,
		)

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Inject ACK script into response.
	//
	// The original public/index.html file is NOT modified.
	page = injectAckScript(page, token)

	// --------------------------------------------------------
	// Register token BEFORE sending page.
	// --------------------------------------------------------

	setPendingAck(state, token)

	validationActive = false

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
	// SERVE PAGE.
	//
	// STILL NOT COUNTED.
	// --------------------------------------------------------

	if _, err := w.Write(page); err != nil {
		cancelPendingAck(state)

		log.Printf(
			"REJECT req=%d client=%s reason=response-write-failed err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// --------------------------------------------------------
	// FLUSH RESPONSE.
	// --------------------------------------------------------

	if err := http.NewResponseController(w).Flush(); err != nil {
		cancelPendingAck(state)

		log.Printf(
			"REJECT req=%d client=%s reason=response-flush-failed err=%v",
			reqID,
			shortID(clientID),
			err,
		)

		return
	}

	// --------------------------------------------------------
	// GET / is finished.
	//
	// It is NOT valid yet.
	//
	// It is waiting for /__click_ack.
	// --------------------------------------------------------

	log.Printf(
		"PAGE-SENT req=%d client=%s awaiting-ack",
		reqID,
		shortID(clientID),
	)
}
