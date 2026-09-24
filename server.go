package main

import (
	"context"
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

	// Suppress an immediate follow-up GET / on the same
	// persistent HTTP connection after a successful click.
	//
	// This is NOT IP rate limiting.
	duplicateWindow = 1500 * time.Millisecond
)

// Every persistent HTTP connection gets its own state.
// No IP address is stored or examined.
type connectionState struct {
	mu sync.Mutex

	// When the last successful click on this connection
	// was completed.
	lastValid time.Time
}

type connectionStateKey struct{}

var (
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

	server := &http.Server{
		Addr: ":" + port,

		// Assign state to each TCP connection.
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

	// Only GET / can enter validation.
	// Every other path returns 204 and does not count.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		serveValidatedPage(w, r, filePath)
	})

	log.Println("Server running on port", port)

	log.Fatal(server.ListenAndServe())
}

func getConnectionState(r *http.Request) *connectionState {
	state, ok := r.Context().
		Value(connectionStateKey{}).
		(*connectionState)

	if !ok {
		// ConnContext should always provide this.
		// Return a private fallback rather than panic.
		return &connectionState{}
	}

	return state
}

// recentlySucceeded checks whether this connection just
// completed a valid click.
//
// This does NOT use an IP address.
func recentlySucceeded(state *connectionState) bool {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.lastValid.IsZero() {
		return false
	}

	if now.Sub(state.lastValid) >= duplicateWindow {
		return false
	}

	return true
}

func markSuccessfulConnection(state *connectionState) {
	state.mu.Lock()
	state.lastValid = time.Now()
	state.mu.Unlock()
}

// serveValidatedPage waits one second, verifies the request
// remained alive, sends the page, flushes the response,
// and only then commits the click.
func serveValidatedPage(
	w http.ResponseWriter,
	r *http.Request,
	filePath string,
) {
	reqID := requestID.Add(1)
	connState := getConnectionState(r)

	log.Printf(
		"REQUEST req=%d method=%s path=%s",
		reqID,
		r.Method,
		r.URL.Path,
	)

	// ------------------------------------------------------------
	// Prevent the immediate duplicate request pattern:
	//
	//   req=15 -> VALID
	//   req=16 -> GET /
	//
	// when req=16 arrives immediately after the successful
	// response on the same persistent connection.
	//
	// This is connection-based only.
	// ------------------------------------------------------------
	if recentlySucceeded(connState) {
		log.Printf(
			"REJECT req=%d reason=duplicate-same-connection",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Limit simultaneous validations.
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
	// Reserve one of the 40 lifetime click slots.
	// ------------------------------------------------------------
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d reason=click-limit completed=%d validating=%d",
			reqID,
			validClicks,
			reservedClicks,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++
	reservationHeld := true

	log.Printf(
		"RESERVED req=%d completed=%d validating=%d",
		reqID,
		validClicks,
		reservedClicks,
	)

	mu.Unlock()

	// If validation fails, the reservation is returned.
	completed := false

	defer func() {
		if !completed && reservationHeld {
			mu.Lock()
			reservedClicks--
			mu.Unlock()

			log.Printf(
				"RELEASED req=%d reason=validation-failed",
				reqID,
			)
		}
	}()

	// ------------------------------------------------------------
	// One-second validation.
	// ------------------------------------------------------------
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d reason=disconnect-during-validation",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Check immediately after validation.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d reason=disconnect-after-validation",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Read index.html before the reservation is consumed.
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
	// Check again before writing.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d reason=disconnect-before-response",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Set response headers.
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
	// Send the page BEFORE counting the click.
	// ------------------------------------------------------------
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"REJECT req=%d reason=response-write-failed err=%v",
			reqID,
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// Explicitly flush the response.
	//
	// This gives net/http a chance to surface a broken connection
	// before the click is committed.
	// ------------------------------------------------------------
	rc := http.NewResponseController(w)

	if err := rc.Flush(); err != nil {
		log.Printf(
			"REJECT req=%d reason=response-flush-failed err=%v",
			reqID,
			err,
		)

		return
	}

	// ------------------------------------------------------------
	// Final request cancellation check.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d reason=disconnect-after-response",
			reqID,
		)

		return
	}

	// ------------------------------------------------------------
	// Now, and only now, convert the reservation into
	// a valid click.
	// ------------------------------------------------------------
	mu.Lock()

	// Another validation could have consumed the final slot.
	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d reason=limit-reached-before-count",
			reqID,
		)

		return
	}

	// One final cancellation check while holding the click lock.
	if err := r.Context().Err(); err != nil {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d reason=disconnect-before-increment",
			reqID,
		)

		return
	}

	// Reservation -> completed click.
	reservedClicks--
	reservationHeld = false

	validClicks++
	clickNumber := validClicks

	mu.Unlock()

	completed = true

	// Record the successful request on this connection.
	markSuccessfulConnection(connState)

	log.Printf(
		"VALID req=%d click=%d/%d",
		reqID,
		clickNumber,
		validClickLimit,
	)
}
