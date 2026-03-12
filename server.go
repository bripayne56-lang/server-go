package main

import (
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"
)

const (
	precheckPath = "/precheck"
	servePath    = "/serve"
	filePath     = "./public/index.html"
)

func precheckHandler(w http.ResponseWriter, r *http.Request) {
	// Serve tiny invisible JS for precheck
	js := `
		<script>
			// Invisible precheck: wait 1s then request /serve
			setTimeout(() => {
				fetch('/serve', {credentials: 'same-origin'});
			}, 1000);
		</script>
	`
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(js))
	log.Println("Precheck JS served")
}

func serveHandler(w http.ResponseWriter, r *http.Request) {
	// Serve index.html
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		http.Error(w, "Error loading page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	log.Println("index.html served")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc(precheckPath, precheckHandler)
	http.HandleFunc(servePath, serveHandler)

	// Cron ping to keep server alive every 14 seconds
	go func() {
		for {
			time.Sleep(14 * time.Second)
			resp, err := http.Get("http://localhost:" + port + precheckPath)
			if err != nil {
				log.Println("Ping error:", err)
				continue
			}
			resp.Body.Close()
			log.Println("Server pinged to stay alive")
		}
	}()

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
