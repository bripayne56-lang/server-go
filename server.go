package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	clickCount int
	mu         sync.Mutex

	// Limits concurrent requests to 50
	semaphore = make(chan struct{}, 50)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// Health check for GCP Load Balancer
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		log.Println("PRECHECK HIT")

		// Prevent caching
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		// Try to claim one of 50 concurrent slots
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		default:
			log.Println("Rejected: concurrency limit reached")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Monitor request for 1 second
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		timeout := time.NewTimer(1 * time.Second)
		defer timeout.Stop()

		elapsed := 0

		for {
			select {
			case <-r.Context().Done():
				log.Println("Client disconnected at:", elapsed, "ms")
				w.WriteHeader(http.StatusNoContent)
				return

			case <-ticker.C:
				elapsed += 100
				log.Println("Request alive:", elapsed, "ms")

			case <-timeout.C:
				goto VALIDATE
			}
		}

	VALIDATE:

		// Count only after surviving 1 second
		mu.Lock()

		if clickCount >= 10 {
			mu.Unlock()
			log.Println("Rejected: valid click limit reached")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		clickCount++
		currentClick := clickCount

		mu.Unlock()

		log.Println("Valid click:", currentClick)

		// Serve page
		http.ServeFile(w, r, filePath)
	})

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

