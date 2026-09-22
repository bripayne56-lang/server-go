package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	validationTime     = 1 * time.Second
	validClickLimit    = 40
	maxConcurrentWaits = 50
)

type clickState int

const (
	clickPending clickState = iota
	clickCounted
	clickFailed
)

var (
	mu sync.Mutex

	validClicks   int
	reservedClicks int

	validationSemaphore = make(chan struct{}, maxConcurrentWaits)

	// Tracks each individual click ID.
	// A click ID can only be counted once.
	clicks = make(map[string]clickState)

	requestID atomic.Uint64
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		serveValidatedPage(w, r, filePath)
	})

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {
	reqID := requestID.Add(1)

	// The frontend must send a unique ID for each physical click.
	clickID := r.URL.Query().Get("click_id")

	log.Printf(
		"REQUEST req=%d click=%q method=%s path=%s",
		reqID,
		clickID,
		r.Method,
		r.URL.Path,
	)

	// ------------------------------------------------------------
	// A click ID is required.
	// ------------------------------------------------------------
	if clickID == "" {
		log.Printf(
			"REJECT req=%d reason=missing-click-id",
			reqID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Claim this click ID.
	//
	// This is the important duplicate protection:
	// the same click ID cannot be validated by two requests
	// simultaneously.
	// ------------------------------------------------------------
	mu.Lock()

	if state, exists := clicks[clickID]; exists {
		switch state {
		case clickPending:
			mu.Unlock()

			log.Printf(
				"REJECT req=%d click=%q reason=duplicate-pending",
				reqID,
				clickID,
			)

			w.WriteHeader(http.StatusNoContent)
			return

		case clickCounted:
			mu.Unlock()

			log.Printf(
				"REJECT req=%d click=%q reason=already-counted",
				reqID,
				clickID,
			)

			w.WriteHeader(http.StatusNoContent)
			return

		case clickFailed:
			mu.Unlock()

			log.Printf(
				"REJECT req=%d click=%q reason=already-failed",
				reqID,
				clickID,
			)

			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Mark the click as pending before doing any validation.
	clicks[clickID] = clickPending

	mu.Unlock()

	// If this request fails for any reason, the click must
	// never become counted.
	completed := false

	defer func() {
		if !completed {
			mu.Lock()

			// Keep the click ID permanently failed.
			// A retry using the same click ID cannot become
			// a second valid click.
			clicks[clickID] = clickFailed

			if reservedClicks > 0 {
				reservedClicks--
			}

			mu.Unlock()

			log.Printf(
				"FAILED req=%d click=%q",
				reqID,
				clickID,
			)
		}
	}()

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
			"REJECT req=%d click=%q reason=validation-capacity",
			reqID,
			clickID,
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
			"REJECT req=%d click=%q reason=click-limit completed=%d validating=%d",
			reqID,
			clickID,
			validClicks,
			reservedClicks,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	log.Printf(
		"RESERVED req=%d click=%q completed=%d validating=%d",
		reqID,
		clickID,
		validClicks,
		reservedClicks,
	)

	mu.Unlock()

	// ------------------------------------------------------------
	// One-second validation.
	// ------------------------------------------------------------
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Printf(
			"REJECT req=%d click=%q reason=disconnect-during-validation",
			reqID,
			clickID,
		)

		w.WriteHeader(http.StatusNoContent)
		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Check for disconnect after validation.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d click=%q reason=disconnect-after-validation",
			reqID,
			clickID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Read page.
	// ------------------------------------------------------------
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf(
			"ERROR req=%d click=%q reason=read-index err=%v",
			reqID,
			clickID,
			err,
		)

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// ------------------------------------------------------------
	// Final disconnect check.
	// ------------------------------------------------------------
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT req=%d click=%q reason=disconnect-before-count",
			reqID,
			clickID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Convert reservation into one valid click.
	// ------------------------------------------------------------
	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d click=%q reason=limit-reached-before-count",
			reqID,
			clickID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Final check while holding the lock.
	if err := r.Context().Err(); err != nil {
		mu.Unlock()

		log.Printf(
			"REJECT req=%d click=%q reason=disconnect-before-increment",
			reqID,
			clickID,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Convert reservation -> completed click.
	reservedClicks--
	validClicks++

	clickNumber := validClicks

	// This click can never be counted again.
	clicks[clickID] = clickCounted

	mu.Unlock()

	completed = true

	log.Printf(
		"VALID req=%d click=%q number=%d/%d",
		reqID,
		clickID,
		clickNumber,
		validClickLimit,
	)

	// ------------------------------------------------------------
	// Send the page.
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
			"RESPONSE ERROR req=%d click=%q err=%v",
			reqID,
			clickID,
			err,
		)
	}
}
