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
	// How long validation must last.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 40

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50
)

var (
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Clicks currently being validated.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)

	// Used only to make log messages easy to correlate.
	requestID atomic.Uint64
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

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

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Waits one second, checks for disconnect,
// then converts the request into one valid click.
func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {
	id := requestID.Add(1)

	log.Printf(
		"REQUEST id=%d method=%s path=%s",
		id,
		r.Method,
		r.URL.Path,
	)

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
			"REJECT id=%d reason=validation-capacity",
			id,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Reserve a lifetime click slot.
	// ------------------------------------------------------------
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT id=%d reason=click-limit completed=%d validating=%d",
			id,
			validClicks,
			reservedClicks,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	log.Printf(
		"RESERVED id=%d completed=%d validating=%d",
		id,
		validClicks,
		reservedClicks,
	)

	mu.Unlock()

	// Make sure the reservation is released if the request
	// does not successfully become a valid click.
	completed := false

	defer func() {
		if !completed {
			mu.Lock()
			reservedClicks--
			mu.Unlock()

			log.Printf(
				"RELEASED id=%d reason=validation-failed",
				id,
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
			"REJECT id=%d reason=disconnect-during-validation",
			id,
		)

		w.WriteHeader(http.StatusNoContent)
		return

	case <-timer.C:
	}

	// Check for disconnect after the validation period.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT id=%d reason=disconnect-after-validation",
			id,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Read the page before consuming the reservation.
	// ------------------------------------------------------------
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf(
			"ERROR id=%d reason=read-index err=%v",
			id,
			err,
		)

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Final disconnect check before counting.
	if err := r.Context().Err(); err != nil {
		log.Printf(
			"REJECT id=%d reason=disconnect-before-count",
			id,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ------------------------------------------------------------
	// Convert reservation into one valid click.
	// ------------------------------------------------------------
	mu.Lock()

	// Make sure the lifetime limit has not been reached.
	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"REJECT id=%d reason=limit-reached-before-count",
			id,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Final disconnect check while holding the click lock.
	if err := r.Context().Err(); err != nil {
		mu.Unlock()

		log.Printf(
			"REJECT id=%d reason=disconnect-before-increment",
			id,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks--
	validClicks++

	clickNumber := validClicks

	mu.Unlock()

	completed = true

	log.Printf(
		"VALID id=%d click=%d/%d",
		id,
		clickNumber,
		validClickLimit,
	)

	// ------------------------------------------------------------
	// Send the actual page.
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
			"RESPONSE ERROR id=%d click=%d err=%v",
			id,
			clickNumber,
			err,
		)
	}
}
