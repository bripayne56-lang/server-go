package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	validationTime           = 1 * time.Second
	validClickLimit          = 10
	maxConcurrentValidations = 500
)

var (
	mu sync.Mutex

	// Clicks that successfully completed the 1-second validation.
	validClicks int

	// Clicks currently waiting through the 1-second validation.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentValidations)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

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
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write([]byte(
			"Completed: " + itoa(completed) +
				"/" + itoa(validClickLimit) +
				"\nValidating: " + itoa(reserved),
		))
	})

	// LANDING PAGE
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		landing(w, r, filePath)
	})

	log.Println("Server running on port", port)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func landing(w http.ResponseWriter, r *http.Request, filePath string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback for non-streaming environments.
		time.Sleep(validationTime)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		http.ServeFile(w, r, filePath)
		return
	}

	// Avoid counting icon requests that browsers silently launch
	// alongside the page load.
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Read the actual index.html before beginning the delay.
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

	// Limit simultaneous validations.
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

	// Release the reservation when the request finishes.
	defer func() {
		mu.Lock()
		reservedClicks--
		mu.Unlock()
	}()

	// Cloud-safe streaming headers.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)

	// Send 1 KB of invisible data immediately.
	// Nothing visible from this portion is shown to the user.
	_, _ = fmt.Fprintf(
		w,
		"<!DOCTYPE html><html><head><!--%s--></head><body>",
		strings.Repeat(" ", 1024),
	)

	// Force the invisible buffer out immediately.
	flusher.Flush()

	// Wait up to 1 second, but stop if the client disconnects.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println(
			"[DISCONNECT] User left before 1 second elapsed. Processing aborted.",
		)
		return

	case <-timer.C:
		// User successfully waited the full 1 second.
	}

	// Count the click only after the full 1-second validation.
	mu.Lock()
	validClicks++

	log.Printf(
		"[CLICK] Valid click: %d/%d",
		validClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Send the actual contents of public/index.html.
	_, _ = w.Write(page)

	flusher.Flush()
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

