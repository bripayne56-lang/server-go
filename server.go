package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	delaySeconds = 1
	port         = "8080" // change if needed
)

func main() {
	// Determine file path
	filePath := filepath.Join("public", "index.html")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		log.Fatalf("index.html not found in public/: %v", err)
	}

	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Channel to signal completion
		done := make(chan bool, 1)

		go func() {
			select {
			case <-ctx.Done():
				// Client disconnected before 1 second
				w.WriteHeader(http.StatusNoContent) // 204
				done <- true
				return
			case <-time.After(time.Second * delaySeconds):
				// Delay finished, serve the page
				http.ServeFile(w, r, filePath)
				done <- true
			}
		}()

		<-done
		fmt.Println("Precheck handled for client:", r.RemoteAddr)
	})

	// Catch-all 404
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	log.Println("Server running on port", port)
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatal(err)
	}
}
