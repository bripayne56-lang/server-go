package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	validationTime    = 1 * time.Second
	validClickLimit   = 10
	maxConcurrentWaits = 50
)

type contextKey struct{}

var connectionStateKey contextKey

type connectionState struct {
	id   uint64
	slot chan struct{}
}

var (
	validClicks int

	countMu sync.Mutex

	// Serializes the final response/count operation.
	// This prevents two requests from racing for the last
	// available lifetime click.
	commitMu sync.Mutex

	// Maximum number of requests that can be in the 1-second
	// validation phase at once.
	validationSem = make(chan struct{}, maxConcurrentWaits)

	requestSeq   atomic.Uint64
	connectionSeq atomic.Uint64
)

func main() {
	page, err := os.ReadFile("public/index.html")
	if err != nil {
		log.Fatalf("failed to read public/index.html: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reqID := requestSeq.Add(1)

		if r.Method != http.MethodGet {
			log.Printf(
				"REJECT req=%d method=%s path=%s reason=method-not-allowed",
				reqID, r.Method, r.URL.Path,
			)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if r.URL.Path != "/" {
			log.Printf(
				"REJECT req=%d method=%s path=%s reason=not-found",
				reqID, r.Method, r.URL.Path,
			)
			http.NotFound(w, r)
			return
		}

		serveValidatedPage(w, r, page, reqID)
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,

		// This gives dead/broken clients a finite write window.
		WriteTimeout: 15 * time.Second,

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			connID := connectionSeq.Add(1)

			state := &connectionState{
				id:   connID,
				slot: make(chan struct{}, 1),
			}

			// One token means one active request on this
			// connection at a time.
			state.slot <- struct{}{}

			return context.WithValue(ctx, connectionStateKey, state)
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

		// Serialize all requests on this connection.
		//
		// This is important because the browser can send another
		// GET on the same persistent connection while the previous
		// request is finishing/canceling.
		select {
		case <-state.slot:
			defer func() {
				state.slot <- struct{}{}
			}()

		case <-r.Context().Done():
			log.Printf(
				"REJECT req=%d conn=%d reason=disconnect-waiting-for-connection-slot",
				reqID, connID,
			)
			return
		}
	}

	log.Printf("REQUEST req=%d conn=%d", reqID, connID)

	// IMPORTANT:
	// Do NOT return 204 if all 50 validation slots are busy.
	// Instead, wait for one. A busy validation pool is NOT
	// the lifetime click limit.
	select {
	case validationSem <- struct{}{}:
		defer func() {
			<-validationSem
			log.Printf(
				"RELEASED req=%d conn=%d",
				reqID, connID,
			)
		}()

		log.Printf(
			"RESERVED req=%d conn=%d",
			reqID, connID,
		)

	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-before-validation",
			reqID, connID,
		)
		return
	}

	// ------------------------------------------------------------
	// 1 SECOND VALIDATION
	// ------------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-timer.C:
		// Continue.

	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-during-validation",
			reqID, connID,
		)
		return
	}

	// Check again immediately after the timer.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-after-validation err=%v",
			reqID, connID, err,
		)
		return
	}

	// Load the page before entering the final commit section.
	// The page is already loaded into memory in main(), so this is cheap.
	if len(page) == 0 {
		log.Printf(
			"REJECT req=%d conn=%d reason=empty-page",
			reqID, connID,
		)
		http.Error(w, "empty page", http.StatusInternalServerError)
		return
	}

	// ------------------------------------------------------------
	// FINAL COMMIT
	// ------------------------------------------------------------
	//
	// Only one request at a time is allowed into this section.
	//
	// That guarantees:
	//   - no two requests can both consume the same final slot
	//   - validClicks cannot unexpectedly change between the
	//     limit check and the count
	//   - 204 means the actual counter is at the limit
	//
	commitMu.Lock()
	defer commitMu.Unlock()

	// The client might have disconnected while we were waiting
	// for commitMu.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-before-response",
			reqID, connID,
		)
		return
	}

	// ------------------------------------------------------------
	// THE ONLY 204 PATH IN THIS PROGRAM
	// ------------------------------------------------------------

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
	// SEND THE RESPONSE FIRST
	// ------------------------------------------------------------
	//
	// We do NOT increment validClicks until the response has been
	// successfully written/flushed and the request context is still
	// alive.
	//
	// Therefore a normal early disconnect should never become a
	// valid click.
	// ------------------------------------------------------------

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))

	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(page); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=response-write-failed err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// Force net/http to push the response now where supported.
	rc := http.NewResponseController(w)

	if err := rc.Flush(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=response-flush-failed err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// Final disconnect check immediately before counting.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d conn=%d reason=disconnect-after-response err=%v",
			reqID,
			connID,
			err,
		)
		return
	}

	// ------------------------------------------------------------
	// COUNT EXACTLY ONCE
	// ------------------------------------------------------------

	countMu.Lock()

	// commitMu guarantees another request cannot change the count
	// while we're doing this finalization.
	if validClicks >= validClickLimit {
		countMu.Unlock()

		// This should normally be impossible because commitMu
		// serializes the final section, but keep the guard anyway.
		log.Printf(
			"204 req=%d conn=%d reason=lifetime-limit-race click=%d/%d",
			reqID,
			connID,
			validClicks,
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

	// No other code path increments validClicks.
	// This request is done.
}
