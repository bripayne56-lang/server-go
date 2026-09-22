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
	validClickLimit = 40

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50

	// How long to remember a failed/disconnected validation
	// on the SAME HTTP connection.
	//
	// This is NOT an IP cooldown.
	disconnectRetryWindow = 2 * time.Second
)

type connectionKey struct {
	id uint64
}

var (
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Clicks currently being validated.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)

	// Connections whose validation recently failed/disconnected.
	//
	// Keyed by HTTP connection ID, NOT IP address.
	recentFailedConnections = make(map[uint64]time.Time)

	requestID    atomic.Uint64
	connectionID atomic.Uint64
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// ------------------------------------------------------------
	// HTTP server.
	//
	// ConnContext gives every TCP connection a unique server-side
	// ID. This lets us recognize a retry on the same connection
	// without using IP addresses.
	// ------------------------------------------------------------
	server := &http.Server{
		Addr: ":" + port,

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			id := connectionID.Add(1)

			return context.WithValue(
				ctx,
				connectionKey{},
				id,
			)
		},
	}

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

func getConnectionID(r *http.Request) uint64 {
	id, ok := r.Context().Value(connectionKey{}).(uint64)
	if !ok {
		return 0
	}

	return id
}

// Returns true when this exact HTTP connection recently failed
// a validation.
func recentlyFailed(connID uint64) bool {
	if connID == 0 {
		return false
	}

	now := time.Now()

	mu.Lock()
	defer mu.Unlock()

	last, found := recentFailedConnections[connID]
	if !found {
		return false
	}

	if now.Sub(last) >= disconnectRetryWindow {
		delete(recentFailedConnections, connID)
		return false
	}

	return true
}

// Records a failed validation for this HTTP connection.
func rememberFailedConnection(connID uint64) {
	if connID == 0 {
		return
	}

	mu.Lock()
	recentFailedConnections[connID] = time.Now()
	mu.Unlock()
}

// Waits one second, checks for disconnect,
// then converts the request into one valid click.
func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {
	reqID := requestID.Add(1)
	connID := getConnectionID(r)

	log.Printf(
		"REQUEST req=%d conn=%d method=%s path=%s",
		reqID,
		connID,
		r.Method,
		r.URL.Path,
	)

	// ------------------------------------------------------------
	// Reject only an immediate retry on the SAME connection after
	// a failed/disconnected validation.
	//
	// There is NO IP check here.
	// ------------------------------------------------------------
	if recentlyFailed(connID) {
		log.Printf(
			"REJECT req=%d conn=%d reason=recent-failed-connection",
			reqID,
			connID,
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
			"REJECT req=%d conn=%d reason=validation-capacity",
			reqID,
			connID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Reserve one lifetime click slot.
	// ------------------------------------------------------------
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d conn=%d reason=click-limit completed=%d validating=%d",
			reqID,
			connID,
			validClicks,
			reservedClicks,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	reservationHeld := true

	log.Printf(
		"RESERVED req=%d conn=%d completed=%d validating=%d",
		reqID,
		connID,
		validClicks,
		reservedClicks,
	)

	mu.Unlock()

	// ------------------------------------------------------------
	// Release reservation if validation does not complete.
	// ------------------------------------------------------------
	completed := false

	defer func() {
		if !completed && reservationHeld {
			mu.Lock()
			reservedClicks--
			mu.Unlock()

			log.Printf(
				"RELEASED req=%d conn=%d reason=validation-failed",
				reqID,
				connID,
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
		rememberFailedConnection(connID)

		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-during-validation",
			reqID,
			connID,
		)

		w.WriteHeader(http.StatusNoContent)
		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Check for disconnect after validation.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		rememberFailedConnection(connID)

		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-after-validation",
			reqID,
			connID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Read the page before counting.
	// ------------------------------------------------------------
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf(
			"ERROR req=%d conn=%d reason=read-index err=%v",
			reqID,
			connID,
			err,
		)

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// ------------------------------------------------------------
	// Final disconnect check.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		rememberFailedConnection(connID)

		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-before-count",
			reqID,
			connID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Count exactly once for THIS request.
	// ------------------------------------------------------------
	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d conn=%d reason=limit-reached-before-count",
			reqID,
			connID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := r.Context().Err(); err != nil {
		mu.Unlock()

		rememberFailedConnection(connID)

		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-before-increment",
			reqID,
			connID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks--
	reservationHeld = false

	validClicks++

	clickNumber := validClicks

	mu.Unlock()

	completed = true

	log.Printf(
		"VALID req=%d conn=%d click=%d/%d",
		reqID,
		connID,
		clickNumber,
		validClickLimit,
	)

	// ------------------------------------------------------------
	// Send actual page.
	// ------------------------------------------------------------
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"RESPONSE ERROR req=%d conn=%d click=%d err=%v",
			reqID,
			connID,
			clickNumber,
			err,
		)
	}
}
