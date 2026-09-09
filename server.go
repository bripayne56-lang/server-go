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
	validationTime          = 1 * time.Second
	validClickLimit         = 10
	maxConcurrentValidations = 500
)

var (
	mu sync.Mutex

	// Clicks that successfully completed the 1-second validation.
	validClicks int

	// Clicks currently inside the 1-second validation period.
	reservedClicks int

	// Maximum number of simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentValidations)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Your actual landing page.
	filePath := filepath.Join("public", "index.html")

	// HEALTH CHECK
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// STATUS
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		completed := validClicks
		reserved := reservedClicks
		mu.Unlock()

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		_, _ = w.Write([]byte(
			"Completed: " + itoa(completed) +
				"/" + itoa(validClickLimit) +
				"\nValidating: " + itoa(reserved),
		))
	})

	// LANDING PAGE / VALIDATION
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(
				w,
				"Streaming not supported",
				http.StatusInternalServerError,
			)
			return
		}

		// Read your real index.html.
		page, err := os.ReadFile(filePath)
		if err != nil {
			log.Println("Failed to read index.html:", err)
			http.Error(
				w,
				"Could not load page",
				http.StatusInternalServerError,
			)
			return
		}

		// Try to claim one of the concurrent validation slots.
		select {
		case validationSemaphore <- struct{}{}:
			defer func() {
				<-validationSemaphore
			}()

		default:
			log.Println("Rejected: concurrency limit reached")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		// Reserve one of the 10 valid-click slots.
		mu.Lock()

		if validClicks+reservedClicks >= validClickLimit {
			mu.Unlock()

			log.Println("Rejected: valid click limit reached")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		reservedClicks++

		log.Printf(
			"[CLICK] Reserved: completed=%d reserved=%d limit=%d",
			validClicks,
			reservedClicks,
			validClickLimit,
		)

		mu.Unlock()

		defer func() {
			mu.Lock()
			reservedClicks--
			mu.Unlock()
		}()

		// Prevent caching.
		w.Header().Set(
			"Cache-Control",
			"no-store, no-cache, must-revalidate, max-age=0",
		)
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Send invisible data immediately.
		// This opens/flushed the response without displaying
		// the landing page yet.
		_, _ = w.Write([]byte("<!-- waiting for validation -->"))
		flusher.Flush()

		// Wait 1 second.
		time.Sleep(validationTime)

		// The request survived the validation period.
		mu.Lock()
		validClicks++

		log.Printf(
			"[CLICK] Valid click: %d/%d",
			validClicks,
			validClickLimit,
		)

		mu.Unlock()

		// NOW send your actual public/index.html.
		_, _ = w.Write(page)
		flusher.Flush()
	})

	log.Println("Server running on port", port)

	log.Fatal(
		http.ListenAndServe(":"+port, nil),
	)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}

