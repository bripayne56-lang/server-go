package main

import (
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"
)

const filePath = "./public/index.html"

func handler(w http.ResponseWriter, r *http.Request) {
	// Listen for client disconnect
	ctx := r.Context()

	select {
	case <-ctx.Done():
		// User left early → 204
		w.WriteHeader(http.StatusNoContent)
		log.Println("Early exit detected, 204 sent to", r.RemoteAddr)
		return
	case <-time.After(1 * time.Second):
		// 1-second delay passed, serve index.html
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			http.Error(w, "Error loading page", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		log.Println("index.html served to", r.RemoteAddr)
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc("/", handler)

	// Optional: keep-alive ping to prevent free Render sleeping
	go func() {
		for {
			time.Sleep(14 * time.Second)
			_, err := http.Get("http://localhost:" + port)
			if err != nil {
				log.Println("Ping error:", err)
			} else {
				log.Println("Server pinged to stay alive")
			}
		}
	}()

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
