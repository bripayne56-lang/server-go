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

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)
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
	log.Printf(
		"REQUEST: method=%s path=%s",
		r.Method,
		r.URL.Path,
	)

	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

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

	// Check for disconnect after the one-second validation.
	if r.Context().Err() != nil {
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Read the page before consuming a valid-click slot.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("failed to read index.html:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Final disconnect check before counting.
	if r.Context().Err() != nil {
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Convert this request into one valid click.
	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Final disconnect check before incrementing.
	if r.Context().Err() != nil {
		mu.Unlock()

		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	validClicks++

	clickNumber := validClicks

	mu.Unlock()

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

