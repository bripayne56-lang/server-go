package main

import (
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
		log.Println("Client disconnected early — 204")
		w.WriteHeader(http.StatusNoContent)
	case <-done:
		log.Println("Page served after 1 second")
	}
}

func main() {

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc(precheckPath, precheckHandler)

	// cron-style self ping every 14s
	go func() {
		for {
			time.Sleep(14 * time.Second)
			http.Get("http://localhost:" + port + precheckPath)
		}
	}()

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
