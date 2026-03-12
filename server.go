package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	// Get Render-assigned port
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // fallback for local testing
	}

	// Path to index.html
	filePath := filepath.Join("public", "index.html")

	// Serve /precheck endpoint
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filePath)
		log.Println("Served index.html to", r.RemoteAddr)
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
