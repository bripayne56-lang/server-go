package main

import (
	"context"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	validationTime      = 1 * time.Second
	validClickLimit     = 10
	maxConcurrentWaits  = 50
)

type contextKey struct{}

type connectionState struct {
	id uint64
	mu sync.Mutex
}

var connectionStateKey contextKey

var (
	countMu sync.Mutex

	validClicks int

	validationSem = make(chan struct{}, maxConcurrentWaits)

	requestSeq    uint64
	connectionSeq uint64
)

func nextRequestID() uint64 {
	return atomic.AddUint64(&requestSeq, 1)
}

func nextConnectionID() uint64 {
	return atomic.AddUint64(&connectionSeq, 1)
}

func main() {
	page, err := ioutil.ReadFile("public/index.html")
	if err != nil {
		log.Fatalf("failed to read public/index.html: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reqID := nextRequestID()

		if r.Method != http.MethodGet {
			log.Printf(
				"REJECT req=%d reason=method-not-allowed method=%s path=%s",
				reqID,
				r.Method,
				r.URL.Path,
			)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if r.URL.Path != "/" {
			log.Printf(
				"REJECT req=%d reason=not-found path=%s",
				reqID,
				r.URL.Path,
			)
			http.NotFound(w, r)
			return
		}

		serveValidatedPage(w, r, page, reqID)
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			state := &connectionState{
				id: nextConnectionID(),
			}

			return context.WithValue(
				ctx,
				connectionStateKey,
				state,
			)
		},
	}

	log.Printf(
		"server starting on %s validation=%s validLimit=%d maxConcurrent=%d",
		server.Addr,
		validationTime,
		validClickLimit,
		maxConcurrentWaits,
	)

	log.Fatal(server.ListenAndServe())
}

func serveValidatedPage(
	w http.ResponseWriter,
	r *http.Request,
	page []byte,
	reqID uint64,
) {
	state, _ := r.Context().Value(connectionStateKey).(*connectionState)

	connID := uint64(0)

	if state != nil {
		connID = state.id

		// Only one request from the same TCP connection is allowed
		// to run through this handler at a time.
		//
		// This prevents a second GET on the same connection from
		// racing the first request.
		state.mu.Lock()
		defer state.mu.Unlock()
	}

	log.Printf(
		"REQUEST req=%d conn=%d",
		reqID,
		connID,
	)

	// ------------------------------------------------------------
	// RESERVE VALIDATION SLOT
	// ------------------------------------------------------------

	select {
	case validationSem <- struct{}{}:
		defer func() {
			<-validationSem

			log.Printf(
				"RELEASED req=%d conn=%d",
				reqID,
				connID,
			)
		}()

		log.Printf(
			"RESERVED req=%d conn=%d",
			reqID,
			connID,
		)

	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-before-validation",
			reqID,
			connID,
		)
		return
	}

	// ------------------------------------------------------------
	// VALIDATION PERIOD
	// ------------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-timer.C:
		// Validation completed.

	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-during-validation",
			reqID,
			connID,
		)
		return
	}

	// Check immediately after the 1-second validation.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-after-validation err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// LIFETIME LIMIT CHECK
	// ------------------------------------------------------------
	//
	// 204 is ONLY returned here.
	// It is NEVER returned because the validation pool is busy,
	// because of a disconnect, or because of a duplicate request.

	countMu.Lock()
	current := validClicks
	countMu.Unlock()

	if current >= validClickLimit {
		log.Printf(
			"204 req=%d conn=%d reason=lifetime-limit click=%d/%d",
			reqID,
			connID,
			current,
			validClickLimit,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// FINAL DISCONNECT CHECK
	// ------------------------------------------------------------

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-before-write err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// SEND RESPONSE
	// ------------------------------------------------------------

	w.Header().Set(
		"Content-Type",
		"text/html; charset=utf-8",
	)

	w.Header().Set(
		"Content-Length",
		strconv.Itoa(len(page)),
	)

	_, err := w.Write(page)

	if err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=response-write-failed err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// FINAL DISCONNECT CHECK
	// ------------------------------------------------------------
	//
	// We do not count the click until after the response has been
	// successfully written.

	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-after-write err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// COUNT VALID CLICK
	// ------------------------------------------------------------
	//
	// There is exactly ONE place in the entire program where
	// validClicks can increase.

	countMu.Lock()

	// Re-check the limit while holding the mutex.
	//
	// This prevents the count from ever going above 40.
	if validClicks >= validClickLimit {
		current = validClicks
		countMu.Unlock()

		log.Printf(
			"204 req=%d conn=%d reason=lifetime-limit-final-check click=%d/%d",
			reqID,
			connID,
			current,
			validClickLimit,
		)
		return
	}

	validClicks++
	clickNumber := validClicks

	countMu.Unlock()

	log.Printf(
		"VALID req=%d conn=%d click=%d/%d",
		reqID,
		connID,
		clickNumber,
		validClickLimit,
	)
}
