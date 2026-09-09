package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// Validation delay.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during the lifetime
	// of this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentValidations = 500
)

var (
	mu sync.Mutex

	// Clicks that successfully completed validation.
	validClicks int

	// Clicks that have reserved a slot and are currently
	// going through the 1-second validation.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentValidations)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// HEALTH CHECK
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// LANDING PAGE
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveLandingPage(w, r, filePath)
	})

	// VERIFICATION REQUEST
	http.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// STATUS
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		completed := validClicks
		reserved := reservedClicks
		mu.Unlock()

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write([]byte(
			"Completed: " + itoa(completed) +
				"/" + itoa(validClickLimit) +
				"\nValidating: " + itoa(reserved),
		))
	})

	log.Println("Server running on port", port)

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// SERVE LANDING PAGE
func serveLandingPage(w http.ResponseWriter, r *http.Request, filePath string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
		http.Error(w, "Could not load page", http.StatusInternalServerError)
		return
	}

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Invisible data is sent and flushed first.
	_, _ = w.Write([]byte("<!-- waiting -->"))
	flusher.Flush()

	// Wait one second.
	time.Sleep(validationTime)

	// Send the actual index.html.
	_, _ = w.Write(page)
	flusher.Flush()
}

// VERIFY HANDLER
func verifyHandler(w http.ResponseWriter, r *http.Request, filePath string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// Anti-caching headers.
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		log.Println("Rejected: concurrency limit reached")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Reserve one of the 10 available lifetime slots.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Println("Rejected: valid click limit reached")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	reservedClicks++

	log.Printf(
		"[VERIFY] Reserved: completed=%d reserved=%d limit=%d",
		validClicks,
		reservedClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Release the reservation when this request ends.
	defer func() {
		mu.Lock()
		reservedClicks--
		mu.Unlock()
	}()

	// Send invisible data first and flush it.
	// Nothing visually meaningful is displayed.
	_, _ = w.Write([]byte("<!-- waiting for validation -->"))
	flusher.Flush()

	// Wait for the validation period, while also watching
	// for the client disconnecting.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println("[DISCONNECT DETECTED] Client disconnected during validation.")
		return

	case <-timer.C:
		// Validation completed.
	}

	// Read the actual index.html.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
		return
	}

	// Count the click only after the full validation period.
	mu.Lock()
	validClicks++

	log.Printf(
		"[VERIFY] Valid click: %d/%d",
		validClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Send the actual index.html after validation.
	_, _ = w.Write(page)
	flusher.Flush()
}

// SIMPLE INTEGER CONVERSION
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}


