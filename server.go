package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	// Use Render or DO environment port
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // fallback for local testing
	}

	// Path to your index.html
	filePath := filepath.Join("public", "index.html")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		log.Fatalf("index.html not found in public/: %v", err)
	}

	// /precheck endpoint
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		done := make(chan bool, 1)

		go func() {
			select {
			case <-ctx.Done():
				// Client disconnected before 1 second
				w.WriteHeader(http.StatusNoContent) // 204
				done <- true
				return
			case <-time.After(1 * time.Second):
				// Delay finished, serve the page
				http.ServeFile(w, r, filePath)
				done <- true
			}
		}()

		<-done
		log.Println("Precheck handled for client:", r.RemoteAddr)
	})

	// Default 404
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	log.Println("Server running on port", port)
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatal(err)
	}
}
