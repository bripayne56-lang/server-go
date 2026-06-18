```go
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	clickCount int
	mu         sync.Mutex

	// Limits concurrent requests to 50
	semaphore = make(chan struct{}, 50)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		// Try to claim one of 50 concurrent slots
		select {
		case semaphore <- struct{}{}:
			// Got a slot; release when done
			defer func() { <-semaphore }()
		default:
			// Already at 50 concurrent → reject
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Lock counter safely
		mu.Lock()

		// Stop after 1,000 total valid clicks
		if clickCount >= 1000 {
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Count valid click
		clickCount++
		currentClick := clickCount

		mu.Unlock()

		// Serve page
		select {
		case <-r.Context().Done():
			w.WriteHeader(http.StatusNoContent)
			return

		case <-time.After(1 * time.Second):
			log.Println("Valid click:", currentClick)
			http.ServeFile(w, r, filePath)
		}
	})

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
```

