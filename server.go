package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	// How long a click must remain alive before it is eligible
	// for completion.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50

	// After a successful click, reject another click from the
	// same persistent connection during this window.
	duplicateWindow = 1500 * time.Millisecond

	// How long the browser has to ACK a successfully validated click.
	//
	// If the browser disconnects before sending the ACK, the click
	// is never counted.
	ackTimeout = 5 * time.Second
)

// Every persistent HTTP connection gets its own state.
//
// No IP address is stored or examined.
type connectionState struct {
	mu sync.Mutex

	// A validation is currently running on this connection.
	validationInFlight bool

	// Validation finished, but the browser has not acknowledged
	// the success yet.
	awaitingAck bool

	// When the last successfully acknowledged click on this
	// connection was completed.
	lastValid time.Time
}

type connectionStateKey struct{}

type pendingClick struct {
	state     *connectionState
	createdAt time.Time
}

type ackRequest struct {
	Token string `json:"token"`
}

var (
	// Protects validClicks and reservedClicks.
	mu sync.Mutex

	// Successfully acknowledged valid clicks.
	validClicks int

	// Clicks currently reserved while validating or awaiting ACK.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)

	// Pending successful validations waiting for browser ACK.
	pendingMu sync.Mutex
	pending   = make(map[string]*pendingClick)

	requestID atomic.Uint64
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	mux := http.NewServeMux()

	// ------------------------------------------------------------
	// GET /
	//
	// This ONLY serves the page.
	//
	// It is NOT counted as a click.
	// ------------------------------------------------------------
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		serveIndex(w, filePath)
	})

	// ------------------------------------------------------------
	// POST /click
	//
	// This is the actual click validation request.
	//
	// It waits one second. If the request disappears before then,
	// nothing is counted and no VALID log is emitted.
	//
	// If validation survives one second, the server sends a token.
	// The click is STILL NOT COUNTED.
	//
	// It only becomes valid after POST /click/ack is received.
	// ------------------------------------------------------------
	mux.HandleFunc("/click", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/click" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		handleClick(w, r)
	})

	// ------------------------------------------------------------
	// POST /click/ack
	//
	// The browser sends this only after it actually receives the
	// successful one-second validation response.
	//
	// ONLY HERE can validClicks be incremented.
	// ------------------------------------------------------------
	mux.HandleFunc("/click/ack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/click/ack" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		handleAck(w, r)
	})

	server := &http.Server{
		Addr: ":" + port,

		// Give each persistent HTTP connection its own state.
		//
		// No IP address is used.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			state := &connectionState{}

			return context.WithValue(
				ctx,
				connectionStateKey{},
				state,
			)
		},
	}

	log.Println("Server running on port", port)

	log.Fatal(server.ListenAndServe())
}

// ------------------------------------------------------------
// Serve index.html.
// ------------------------------------------------------------

func serveIndex(w http.ResponseWriter, filePath string) {
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("ERROR reason=read-index err=%v", err)

		http.Error(
			w,
			"internal server error",
			http.StatusInternalServerError,
		)

		return
	}

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

	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"ERROR reason=index-write err=%v",
			err,
		)
	}
}

// ------------------------------------------------------------
// Connection state.
// ------------------------------------------------------------

func getConnectionState(r *http.Request) *connectionState {
	state, ok := r.Context().
		Value(connectionStateKey{}).
		(*connectionState)

	if !ok {
		return &connectionState{}
	}

	return state
}

// beginClick atomically claims the connection for a new click.
//
// It rejects:
//   - another validation currently running
//   - a validation waiting for ACK
//   - a recent successful click
func beginClick(state *connectionState) bool {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.validationInFlight {
		return false
	}

	if state.awaitingAck {
		return false
	}

	if !state.lastValid.IsZero() &&
		now.Sub(state.lastValid) < duplicateWindow {
		return false
	}

	state.validationInFlight = true

	return true
}

// validationFailed releases the per-connection validation state.
func validationFailed(state *connectionState) {
	state.mu.Lock()
	state.validationInFlight = false
	state.mu.Unlock()
}

// validationSucceeded changes the connection into the
// "waiting for ACK" state.
//
// The global reservation remains held.
func validationSucceeded(state *connectionState) {
	state.mu.Lock()

	state.validationInFlight = false
	state.awaitingAck = true

	state.mu.Unlock()
}

// completeAcknowledgedClick records a successful click on the
// connection.
func completeAcknowledgedClick(state *connectionState) {
	state.mu.Lock()

	state.awaitingAck = false
	state.lastValid = time.Now()

	state.mu.Unlock()
}

// expirePendingConnection releases the connection state when
// an ACK never arrives.
func expirePendingConnection(state *connectionState) {
	state.mu.Lock()

	state.awaitingAck = false

	state.mu.Unlock()
}

// ------------------------------------------------------------
// Global click reservation.
// ------------------------------------------------------------

// reserveClick reserves one of the 40 available lifetime slots.
//
// A reservation prevents several simultaneous validations from
// consuming the final slots at the same time.
func reserveClick(reqID uint64) bool {
	mu.Lock()
	defer mu.Unlock()

	// IMPORTANT:
	//
	// Once 40 clicks are either completed OR reserved, return 204.
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

// releaseClickReservation releases a slot because the click
// failed validation or never received an ACK.
func releaseClickReservation(reqID uint64, reason string) {
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
// Commit.
//
// THIS IS THE ONLY FUNCTION THAT PRODUCES A VALID CLICK.
// ------------------------------------------------------------

func commitAcknowledgedClick(reqID uint64) (bool, int) {
	mu.Lock()
	defer mu.Unlock()

	// If 40 valid clicks have already been committed, do not
	// increment again.
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

	// Reservation -> completed valid click.
	reservedClicks--
	validClicks++

	clickNumber := validClicks

	// IMPORTANT:
	//
	// VALID is logged only here.
	//
	// Therefore:
	//
	//   timer completed
	//   AND browser received token
	//   AND browser sent ACK
	//   AND click was still available
	//
	// must all happen before VALID can appear.
	log.Printf(
		"VALID req=%d click=%d/%d",
		reqID,
		clickNumber,
		validClickLimit,
	)

	return true, clickNumber
}

// ------------------------------------------------------------
// Generate a cryptographically random click token.
// ------------------------------------------------------------

func newToken() (string, error) {
	var b [32]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

// ------------------------------------------------------------
// Pending click storage.
// ------------------------------------------------------------

func addPending(token string, p *pendingClick) {
	pendingMu.Lock()
	pending[token] = p
	pendingMu.Unlock()
}

func takePending(token string) (*pendingClick, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()

	p, ok := pending[token]
	if !ok {
		return nil, false
	}

	delete(pending, token)

	return p, true
}

// ------------------------------------------------------------
// POST /click
// ------------------------------------------------------------

func handleClick(
	w http.ResponseWriter,
	r *http.Request,
) {
	reqID := requestID.Add(1)
	state := getConnectionState(r)

	log.Printf(
		"CLICK req=%d",
		reqID,
	)

	// ------------------------------------------------------------
	// Reject another click already running on this connection.
	// ------------------------------------------------------------

	if !beginClick(state) {
		log.Printf(
			"REJECT req=%d reason=duplicate-or-in-flight",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	validationStateFinished := false
	reservationHeld := false

	defer func() {
		if !validationStateFinished {
			validationFailed(state)
		}

		if reservationHeld {
			releaseClickReservation(
				reqID,
				"validation-failed",
			)
		}
	}()

	// ------------------------------------------------------------
	// Global concurrency limit.
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

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Reserve a lifetime click slot.
	// ------------------------------------------------------------

	if !reserveClick(reqID) {
		return
	}

	reservationHeld = true

	// ------------------------------------------------------------
	// EXACT one-second validation.
	// ------------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d reason=disconnect-before-one-second",
			reqID,
		)

		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Validation has now lasted a full second.
	//
	// We STILL do not count anything yet.
	// ------------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d reason=disconnect-at-validation-end err=%v",
			reqID,
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// Create a one-time success token.
	// ------------------------------------------------------------

	token, err := newToken()
	if err != nil {
		log.Printf(
			"ERROR req=%d reason=token-generation err=%v",
			reqID,
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// Store the pending click BEFORE sending the token.
	// ------------------------------------------------------------

	p := &pendingClick{
		state:     state,
		createdAt: time.Now(),
	}

	addPending(token, p)

	// The reservation is now owned by the pending click.
	reservationHeld = false

	validationSucceeded(state)
	validationStateFinished = true

	// ------------------------------------------------------------
	// Expire the pending click if no ACK arrives.
	// ------------------------------------------------------------

	go func() {
		timer := time.NewTimer(ackTimeout)
		defer timer.Stop()

		<-timer.C

		pendingClick, ok := takePending(token)
		if !ok {
			// ACK already consumed it.
			return
		}

		expirePendingConnection(pendingClick.state)

		releaseClickReservation(
			reqID,
			"ack-timeout",
		)

		log.Printf(
			"EXPIRED req=%d reason=ack-timeout",
			reqID,
		)
	}()

	// ------------------------------------------------------------
	// Tell the browser that one second successfully passed.
	//
	// IMPORTANT:
	// This is NOT VALID yet.
	// ------------------------------------------------------------

	response := struct {
		Token string `json:"token"`
	}{
		Token: token,
	}

	body, err := json.Marshal(response)
	if err != nil {
		// Remove pending click and give reservation back.
		_, _ = takePending(token)

		expirePendingConnection(state)

		releaseClickReservation(
			reqID,
			"json-encode-failed",
		)

		log.Printf(
			"ERROR req=%d reason=json-encode err=%v",
			reqID,
			err,
		)

		return
	}

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set(
		"Content-Type",
		"application/json; charset=utf-8",
	)

	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(body); err != nil {
		// Browser did not successfully receive the response
		// according to the server's write operation.
		//
		// No ACK is possible, so remove the pending click.
		_, _ = takePending(token)

		expirePendingConnection(state)

		releaseClickReservation(
			reqID,
			"response-write-failed",
		)

		log.Printf(
			"REJECT req=%d reason=response-write-failed err=%v",
			reqID,
			err,
		)

		return
	}

	rc := http.NewResponseController(w)

	if err := rc.Flush(); err != nil {
		_, _ = takePending(token)

		expirePendingConnection(state)

		releaseClickReservation(
			reqID,
			"response-flush-failed",
		)

		log.Printf(
			"REJECT req=%d reason=response-flush-failed err=%v",
			reqID,
			err,
		)

		return
	}

	log.Printf(
		"VALIDATION-PASSED req=%d waiting-for-ack",
		reqID,
	)

	// IMPORTANT:
	//
	// There is intentionally NO "VALID" log here.
	//
	// There is intentionally NO increment here.
	//
	// The click is still only pending.
}

// ------------------------------------------------------------
// POST /click/ack
// ------------------------------------------------------------

func handleAck(
	w http.ResponseWriter,
	r *http.Request,
) {
	reqID := requestID.Add(1)

	var req ackRequest

	decoder := json.NewDecoder(r.Body)

	if err := decoder.Decode(&req); err != nil {
		log.Printf(
			"REJECT req=%d reason=invalid-ack",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	if req.Token == "" {
		log.Printf(
			"REJECT req=%d reason=empty-token",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	log.Printf(
		"ACK req=%d token=%s",
		reqID,
		req.Token,
	)

	// ------------------------------------------------------------
	// Atomically consume the token.
	//
	// This guarantees the same ACK cannot be counted twice.
	// ------------------------------------------------------------

	p, ok := takePending(req.Token)

	if !ok {
		log.Printf(
			"REJECT req=%d reason=invalid-or-already-used-token",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Now and ONLY now does the click become valid.
	// ------------------------------------------------------------

	ok, clickNumber := commitAcknowledgedClick(reqID)

	if !ok {
		// The click could not be committed because the global
		// limit has already been reached.
		expirePendingConnection(p.state)

		// IMPORTANT:
		// 204 is preserved when the 40-click limit is reached.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Update the connection's duplicate window.
	// ------------------------------------------------------------

	completeAcknowledgedClick(p.state)

	response := struct {
		Click int `json:"click"`
		Limit int `json:"limit"`
	}{
		Click: clickNumber,
		Limit: validClickLimit,
	}

	body, err := json.Marshal(response)
	if err != nil {
		// The click is already committed at this point.
		// Do not attempt to decrement it.
		http.Error(
			w,
			"internal server error",
			http.StatusInternalServerError,
		)
		return
	}

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set(
		"Content-Type",
		"application/json; charset=utf-8",
	)

	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(body); err != nil {
		log.Printf(
			"INFO req=%d reason=ack-response-write-failed err=%v",
			reqID,
			err,
		)
	}
}
