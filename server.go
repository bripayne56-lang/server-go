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
	clickCount = 0
	clickLimit = 2 // CHANGE THIS
	mutex      sync.Mutex
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {

		// Block if limit reached
		mutex.Lock()
		if clickCount >= clickLimit {
			mutex.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mutex.Unlock()

		ctx := r.Context()

		select {
		case <-ctx.Done():
			// User left early
			w.WriteHeader(http.StatusNoContent)
			return

		case <-time.After(1 * time.Second):
			// Count valid click
			mutex.Lock()
			clickCount++
			current := clickCount
			mutex.Unlock()

			log.Println("Valid click:", current)

			http.ServeFile(w, r, filePath)
		}
	})

	log.Println("Server running on port", port)
	http.ListenAndServe(":"+port, nil)
}
