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
	// How long validation must last.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50

	// Suppress immediate duplicate/retry requests for this client.
	duplicateWindow = 1500 * time.Millisecond

	// How long a served page has to send its acknowledgement.
	ackTimeout = 10 * time.Second

	// Browser/client identifier cookie.
	clientCookieName = "click_client_id"
)

// ------------------------------------------------------------
// Browser/client state.
//
// This is keyed by the cookie, NOT by TCP connection.
// ------------------------------------------------------------

type clientState struct {
	mu sync.Mutex

	// A GET / validation is currently running.
	inFlight bool

	// Last time this client completed a valid click.
	lastValid time.Time

	// Last time this client's validation failed.
	lastFailed time.Time

	// A page was successfully served and is waiting for the
	// browser acknowledgement.
	pendingToken string
	pendingAt    time.Time
}

var (
	// Protects clientStates.
	clientStatesMu sync.Mutex
	clientStates   = make(map[string]*clientState)

	// Protects validClicks.
	mu sync.Mutex

	// ONLY actual acknowledged clicks are counted here.
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
// Cache prevention.
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
// Create the browser cookie and redirect.
//
// IMPORTANT:
// This request does NOT:
//   - validate
//   - count
//   - serve index.html
//
// It only establishes the cookie.
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

	state, ok := clientStates[clientID]

	if !ok {
		state = &clientState{}
		clientStates[clientID] = state
	}

	return state
}

// ------------------------------------------------------------
// Begin a validation.
//
// Duplicate/failed requests are rejected with 204.
// They NEVER count.
// ------------------------------------------------------------

func beginValidation(state *clientState) (bool, string) {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	// Another GET / is already validating for this client.
	if state.inFlight {
		return false, "validation-already-in-flight"
	}

	// A page was already served and is waiting for its ACK.
	if state.pendingToken != "" {
		if now.Sub(state.pendingAt) < ackTimeout {
			return false, "awaiting-acknowledgement"
		}

		// ACK never arrived. Treat that attempt as failed.
		state.pendingToken = ""
		state.pendingAt = time.Time{}
		state.lastFailed = now
	}

	// Recently completed valid click.
	if !state.lastValid.IsZero() &&
		now.Sub(state.lastValid) < duplicateWindow {
		return false, "recently-succeeded"
	}

	// Recently failed validation.
	if !state.lastFailed.IsZero() &&
		now.Sub(state.lastFailed) < duplicateWindow {
		return false, "recently-failed"
	}

	state.inFlight = true

	return true, ""
}

// ------------------------------------------------------------
// Mark validation failure.
// ------------------------------------------------------------

func failValidation(state *clientState) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.inFlight = false
	state.lastFailed = time.Now()
}

// ------------------------------------------------------------
// Put a successful page into "awaiting browser ACK" state.
//
// This happens BEFORE sending the page because the browser may
// send the ACK extremely quickly.
// ------------------------------------------------------------

func setPendingAck(
	state *clientState,
	token string,
) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.inFlight = false
	state.pendingToken = token
	state.pendingAt = time.Now()
}

// ------------------------------------------------------------
// Cancel an ACK that can no longer be completed.
// ------------------------------------------------------------

func cancelPendingAck(state *clientState) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.inFlight = false
	state.pendingToken = ""
	state.pendingAt = time.Time{}
	state.lastFailed = time.Now()
}

// ------------------------------------------------------------
// Validate and consume the browser ACK token.
//
// Returns true ONLY for the exact pending token.
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
		state.lastFailed = now
		return false
	}

	// Consume this token so it cannot be acknowledged twice.
	state.pendingToken = ""
	state.pendingAt = time.Time{}

	return true
}

// ------------------------------------------------------------
// Mark the client successful AFTER commit succeeds.
// ------------------------------------------------------------

func markClientSuccessful(state *clientState) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.lastValid = time.Now()
	state.lastFailed = time.Time{}
}

// ------------------------------------------------------------
// Check actual completed click limit.
//
// IMPORTANT:
// ONLY validClicks is checked.
//
// There is no reservedClicks here.
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
// No GET / disconnect can increment the counter.
// No duplicate can increment the counter.
// No 204 response can increment the counter.
// Only a valid browser ACK gets here.
// ------------------------------------------------------------

func commitClick(reqID uint64) (bool, int) {
	mu.Lock()
	defer mu.Unlock()

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
// Create the browser ACK JavaScript.
//
// This is injected into the response.
// public/index.html itself is NOT modified.
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
// Inject the ACK script before </body>.
//
// If there is no </body>, append it to the document.
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
// This is what actually makes a click valid.
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

	// ACK response itself is intentionally empty.
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
	// 204 ONLY when the ACTUAL completed limit is reached.
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

		// Duplicate/failed requests can return 204.
		// They NEVER call commitClick().
		w.WriteHeader(http.StatusNoContent)
		return
	}

	validationFailed := true

	defer func() {
		if validationFailed {
			failValidation(state)

			log.Printf(
				"FAILED req=%d client=%s",
				reqID,
				shortID(clientID),
			)
		}
	}()

	// --------------------------------------------------------
	// VALIDATION CONCURRENCY LIMIT
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
	// Limit check.
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

	// Inject ACK code into the HTML being served.
	//
	// The original public/index.html file is NOT modified.
	page = injectAckScript(page, token)

	// --------------------------------------------------------
	// IMPORTANT:
	//
	// Register the token BEFORE sending the page.
	//
	// Otherwise the browser could send the ACK before the server
	// has recorded the token.
	// --------------------------------------------------------

	setPendingAck(state, token)

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
	// STILL NO COUNT HERE.
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

	rc := http.NewResponseController(w)

	if err := rc.Flush(); err != nil {
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
	// It is waiting for the browser to execute the injected ACK.
	// --------------------------------------------------------

	validationFailed = false

	log.Printf(
		"PAGE-SENT req=%d client=%s awaiting-ack",
		reqID,
		shortID(clientID),
	)
}
