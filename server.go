package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"
)

const precheckPath = "/precheck"
const filePath = "./public/index.html"

func precheckHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	done := make(chan bool)

	go func() {
		time.Sleep(1 * time.Second)
		select {
		case <-ctx.Done():
			return
		default:
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
		log.Println("Client disconnected early — 204 sent")
		done <- true
	case <-done:
		log.Println("Precheck page served")
	}
}

func main() {
	// Use PORT environment variable from Render
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc(precheckPath, precheckHandler)

	// Cron ping to keep server alive every 14s
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
