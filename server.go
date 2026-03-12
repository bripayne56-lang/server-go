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
	ctx := r.Context() // listens for TCP disconnect

	select {
	case <-ctx.Done():
		// Connection closed before 1 second → 204
		w.WriteHeader(http.StatusNoContent)
		log.Println("User disconnected early, sent 204")
		return
	case <-time.After(1 * time.Second):
		// 1-second delay done → serve page
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			http.Error(w, "Error loading page", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		log.Println("index.html served after 1 second")
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc("/", handler)

	// Cron ping to keep server alive on Render free tier
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
