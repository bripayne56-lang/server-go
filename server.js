package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"
)

const (
	port      = "3000"               // Use Render's PORT environment if needed
	filePath  = "./public/index.html"
	precheckPath = "/precheck"
)

func precheckHandler(w http.ResponseWriter, r *http.Request) {
	// Get request context (canceled if client disconnects)
	ctx := r.Context()

	// Channel to signal done
	done := make(chan bool)

	go func() {
		// Wait 1 second
		time.Sleep(1 * time.Second)
		select {
		case <-ctx.Done():
			// Client disconnected before 1 second
			// Note: nothing sent yet
			return
		default:
			// Serve the page
			data, err := ioutil.ReadFile(filePath)
			if err != nil {
				http.Error(w, "Error loading page", http.StatusInternalServerError)
				done <- true
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			done <- true
		}
	}()

	select {
	case <-ctx.Done():
		// Client disconnected early → send 204
		log.Println("Client disconnected early — 204 sent")
		done <- true
	case <-done:
		// Page served
		log.Println("Precheck page served")
	}
}

func main() {
	http.HandleFunc(precheckPath, precheckHandler)

	// Optional: cron-like ping to keep server alive every 14s
	go func() {
		for {
			time.Sleep(14 * time.Second)
			resp, err := http.Get("http://localhost:" + port + precheckPath)
			if err != nil {
				log.Println("Ping error:", err)
				continue
			}
			if resp.StatusCode == 204 {
				log.Println("Server alive (204 ping)")
			} else {
				log.Println("Server alive (status code):", resp.StatusCode)
			}
			resp.Body.Close()
		}
	}()

	log.Printf("Server running on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
