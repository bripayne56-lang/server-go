package main

import (
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	precheckPath = "/precheck"
	servePath    = "/serve"
	filePath     = "./public/index.html"
)

var precheckUsers = struct {
	sync.Mutex
	m map[string]time.Time
}{m: make(map[string]time.Time)}

// Serve invisible JS precheck
func precheckHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.RemoteAddr

	precheckUsers.Lock()
	precheckUsers.m[userID] = time.Now()
	precheckUsers.Unlock()

	js := `
		<script>
			setTimeout(() => {
				fetch('/serve', { credentials: 'same-origin' });
			}, 1000);
		</script>
	`
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(js))

	log.Println("Precheck JS served for", userID)
}

// Serve index.html only after 1s precheck
func serveHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.RemoteAddr

	precheckUsers.Lock()
	start, ok := precheckUsers.m[userID]
	if !ok || time.Since(start) < 1*time.Second {
		// User left early or no precheck → 204
		delete(precheckUsers.m, userID)
		precheckUsers.Unlock()
		w.WriteHeader(http.StatusNoContent)
		log.Println("Early exit detected for", userID, "→ 204 sent")
		return
	}
	delete(precheckUsers.m, userID)
	precheckUsers.Unlock()

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		http.Error(w, "Error loading page", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	log.Println("index.html served to", userID)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	http.HandleFunc(precheckPath, precheckHandler)
	http.HandleFunc(servePath, serveHandler)

	// Cron ping to keep server alive on free tiers
	go func() {
		for {
			time.Sleep(14 * time.Second)
			_, err := http.Get("http://localhost:" + port + precheckPath)
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
