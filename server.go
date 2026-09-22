package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// ONE AND ONLY PUBLIC CLICK ROUTE
	//
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

// SERVE VALIDATED PAGE
//
// Reserves one of the 40 lifetime click slots,
// waits one second, checks for disconnect,
// then converts the reservation into one valid click.
func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {
	log.Printf(
		"REQUEST: method=%s path=%s",
		r.Method,
		r.URL.Path,
	)

	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Reserve a lifetime click slot.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		<-validationSemaphore

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	log.Printf(
		"reserved click: completed=%d validating=%d",
		validClicks,
		reservedClicks,
	)

	mu.Unlock()

	// Make sure the reservation is released if anything
	// exits before the click is successfully completed.
	completed := false

	defer func() {
		if !completed {
			mu.Lock()
			reservedClicks--
			mu.Unlock()
		}

		<-validationSemaphore
	}()

	// One-second validation.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return

	case <-timer.C:
	}

	// IMPORTANT:
	// Check the connection again immediately after the
	// one-second timer finishes, before counting the click.
	if r.Context().Err() != nil {
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Read the page before consuming the reservation.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("failed to read index.html:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Check again immediately before converting the
	// reservation into a valid click.
	if r.Context().Err() != nil {
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Convert this reservation into exactly one valid click.
	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Final disconnect check while holding the mutex.
	if r.Context().Err() != nil {
		mu.Unlock()

		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks--
	validClicks++

	clickNumber := validClicks

	mu.Unlock()

	completed = true

	log.Printf(
		"valid click %d/%d",
		clickNumber,
		validClickLimit,
	)

	// Send the actual page.
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

