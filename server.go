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

	// Serve invisible JS immediately
	js := `
	<script>
	  // Invisible client-assisted precheck
	  setTimeout(() => {
	    fetch(window.location.href, { method: 'POST', credentials: 'same-origin' });
	  }, 1000);
	</script>
	`
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(js))
		log.Println("Invisible precheck JS served to", r.RemoteAddr)
		return
	}

	// Handle the POST from client after 1-second delay
	if r.Method == http.MethodPost {
		select {
		case <-ctx.Done():
			// User left early → 204
			w.WriteHeader(http.StatusNoContent)
			log.Println("Early exit detected, 204 sent to", r.RemoteAddr)
			return
		case <-time.After(10 * time.Millisecond):
			// tiny wait to ensure client POST fully registered
		}

		// Serve the real page
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			http.Error(w, "Error loading page", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		log.Println("index.html served to", r.RemoteAddr)
		return
	}

	// Fallback for other methods
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc("/", handler)

	// Keep-alive ping for free Render tier
	go func() {
		for {
			time.Sleep(14 * time.Second)
			_, err := http.Get("http://localhost:" + port)
			if err != nil {
				log.Println("Ping error:", err)
			}
		}
	}()

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
