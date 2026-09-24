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
	// How long validation must last before a click can become valid.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50

	// After a successful click on a connection, reject another GET /
	// arriving within this time window.
	duplicateWindow = 1500 * time.Millisecond
)

// Each persistent HTTP connection gets its own state.
//
// IMPORTANT:
// This is connection-based, not IP-based.
type connectionState struct {
	mu sync.Mutex

	// True while a request on this connection is currently
	// being validated.
	//
	// This prevents multiple concurrent requests on the same
	// connection from all becoming valid clicks.
	inFlight bool

	// Time when the most recent successful click on this
	// connection was committed.
	lastValid time.Time
}

type connectionStateKey struct{}

var (
	// Protects validClicks and reservedClicks.
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Clicks currently being validated.
	//
	// A reservation is created before validation and converted
	// into validClicks only after the response has successfully
	// been written/flushed and the request is still alive.
	reservedClicks int

	// Limits simultaneous validations globally.
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

		// Create one connectionState for each TCP connection.
		//
		// No IP address is stored, inspected, or used.
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
	// All other paths/methods return 204 and do not count.
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

// getConnectionState retrieves the state associated with the
// current persistent HTTP connection.
func getConnectionState(r *http.Request) *connectionState {
	state, ok := r.Context().
		Value(connectionStateKey{}).
		(*connectionState)

	if !ok {
		// ConnContext should normally always provide this.
		//
		// Return a private state instead of panicking.
		return &connectionState{}
	}

	return state
}

// beginConnectionValidation atomically determines whether this
// connection is allowed to begin a new validation.
//
// It rejects:
//   - another request currently validating on this connection
//   - a request arriving immediately after a successful click
//
// The important difference from the original code is that
// "validation is currently running" is recorded BEFORE the
// validation begins.
//
// That closes the race where request A, B, and C could all check
// lastValid before A had finished.
func beginConnectionValidation(state *connectionState) bool {
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	// Another request on this same connection is already
	// validating.
	if state.inFlight {
		return false
	}

	// A successful click happened recently.
	if !state.lastValid.IsZero() &&
		now.Sub(state.lastValid) < duplicateWindow {
		return false
	}

	// Claim this connection before doing any work.
	state.inFlight = true

	return true
}

// finishConnectionValidation releases the per-connection
// validation lock.
//
// If successful is true, the current time becomes lastValid.
func finishConnectionValidation(
	state *connectionState,
	successful bool,
) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.inFlight = false

	if successful {
		state.lastValid = time.Now()
	}
}

// reserveClick attempts to reserve one of the lifetime click
// slots.
//
// The reservation means:
//   completed + currently-validating < limit
//
// Reserving before validation prevents more than validClickLimit
// validations from being allowed to compete for the final slots.
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

// releaseReservation gives a reserved click slot back because
// this validation did not become a valid click.
func releaseReservation(reqID uint64) {
	mu.Lock()
	reservedClicks--
	mu.Unlock()

	log.Printf(
		"RELEASED req=%d reason=validation-failed",
		reqID,
	)
}

// commitClick converts a reservation into one completed click.
//
// This is the only place validClicks is incremented.
func commitClick(reqID uint64) (bool, int) {
	mu.Lock()
	defer mu.Unlock()

	// Theoretically this should be prevented by reservations,
	// but keep this guard as a final safety check.
	if validClicks >= validClickLimit {
		log.Printf(
			"REJECT req=%d reason=limit-reached-before-count completed=%d validating=%d",
			reqID,
			validClicks,
			reservedClicks,
		)
		return false, 0
	}

	// Convert reservation -> completed click.
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

// serveValidatedPage performs the validation and serves the page.
//
// Flow:
//
//	GET /
//	  |
//	  +--> reject if another request is already validating
//	  |    on this connection
//	  |
//	  +--> reject if connection recently succeeded
//	  |
//	  +--> acquire global validation slot
//	  |
//	  +--> reserve one lifetime click slot
//	  |
//	  +--> wait 1 second
//	  |
//	  +--> verify request is still alive
//	  |
//	  +--> read index.html
//	  |
//	  +--> write response
//	  |
//	  +--> flush response
//	  |
//	  +--> final cancellation check
//	  |
//	  +--> commit reservation as valid click
//
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
	// Per-connection duplicate / in-flight protection.
	// ------------------------------------------------------------
	//
	// This MUST happen before validation begins.
	//
	// The original code only remembered completed successes.
	// That allowed several requests to enter validation
	// simultaneously.
	// ------------------------------------------------------------
	if !beginConnectionValidation(connState) {
		log.Printf(
			"REJECT req=%d reason=duplicate-or-in-flight",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// completed becomes true only after the global click counter
	// has actually been incremented.
	completed := false

	// reserved becomes true after the global reservation is made.
	reservationHeld := false

	// Always release the per-connection inFlight state.
	//
	// If this validation did not become a valid click, also return
	// the global reservation.
	defer func() {
		finishConnectionValidation(connState, completed)

		if !completed && reservationHeld {
			releaseReservation(reqID)
		}
	}()

	// ------------------------------------------------------------
	// Global validation concurrency limit.
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
	if !reserveClick(reqID) {
		return
	}

	reservationHeld = true

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
		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Check immediately after validation.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d reason=disconnect-after-validation err=%v",
			reqID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// Read the page before consuming the reservation.
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
			"REJECT req=%d reason=disconnect-before-response err=%v",
			reqID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// Set response headers.
	// ------------------------------------------------------------
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)

	w.Header().Set(
		"Pragma",
		"no-cache",
	)

	w.Header().Set(
		"Expires",
		"0",
	)

	w.Header().Set(
		"Content-Type",
		"text/html; charset=utf-8",
	)

	w.Header().Set(
		"X-Content-Type-Options",
		"nosniff",
	)

	// ------------------------------------------------------------
	// Send the response BEFORE committing the click.
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
	// Explicitly flush buffered response data.
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
	// Final cancellation check.
	// ------------------------------------------------------------
	//
	// This is useful, but note that it cannot provide an absolute
	// guarantee that the client received the response. A disconnect
	// can occur immediately after this check.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d reason=disconnect-after-response err=%v",
			reqID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// Commit the click.
	//
	// The reservation becomes a completed click here.
	// ------------------------------------------------------------
	ok, _ := commitClick(reqID)

	if !ok {
		// commitClick did not consume the reservation.
		return
	}

	// The reservation has been consumed by commitClick.
	reservationHeld = false

	// IMPORTANT:
	// Set completed only after commitClick successfully
	// incremented validClicks.
	completed = true

	// finishConnectionValidation() in the deferred cleanup
	// will record lastValid because completed == true.
}
