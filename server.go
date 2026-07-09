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

	// Limits concurrent validation requests to 50
	semaphore = make(chan struct{}, 50)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		log.Println("PRECHECK HIT")

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		// Concurrency protection
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		default:
			log.Println("Rejected: concurrency limit reached")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Server-side validation timer
		start := time.Now()

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		timeout := time.NewTimer(1 * time.Second)
		defer timeout.Stop()

		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(start).Milliseconds()
				log.Println("Validation time:", elapsed, "ms")

			case <-timeout.C:
				goto VALIDATE
			}
		}

	VALIDATE:

		duration := time.Since(start).Milliseconds()

		// Require full 1 second validation window
		if duration < 1000 {
			log.Println("Rejected: validation too short")
			w.WriteHeader(http.StatusNoContent)
			return
		}

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

		http.ServeFile(w, r, filePath)
	})

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

