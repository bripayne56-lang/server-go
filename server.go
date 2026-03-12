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
	js := `
		<script>
			// Invisible precheck: wait 1s then request /serve
			setTimeout(() => {
				window.location.href = '/serve';
			}, 1000);
		</script>
	`
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(js))
	log.Println("Precheck JS served")
}

func serveHandler(w http.ResponseWriter, r *http.Request) {
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

func rootHandler(w http.ResponseWriter, r *http.Request) {
	serveHandler(w, r)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc("/", rootHandler)
	http.HandleFunc(precheckPath, precheckHandler)
	http.HandleFunc(servePath, serveHandler)

	go func() {
		for {
			time.Sleep(14 * time.Second)
			http.Get("http://localhost:" + port + precheckPath)
		}
	}()

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
