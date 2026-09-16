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
	validClickLimit = 40

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50
)

var (
	mu sync.Mutex

	// Clicks that successfully completed validation.
	validClicks int

	// Clicks currently going through validation.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)
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

	// PRECHECK
	// Performs the 1-second validation but never sends HTML.
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		validateOnly(w, r)
	})

	// VERIFY
	// Performs validation and then serves index.html.
	http.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// ROOT
	// The normal website entry point.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// INDEX.HTML
	http.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
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

// VALIDATE ONLY
// Used by /precheck.
// Performs the same validation and counting logic,
// but never sends index.html.
func validateOnly(w http.ResponseWriter, r *http.Request) {
	if !reserveClick(w) {
		return
	}

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		releaseReservation()
		log.Println("validation disconnected; click discarded")
		return

	case <-timer.C:
	}

	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()
		releaseReservation()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	validClicks++
	reservedClicks--

	clickNumber := validClicks

	mu.Unlock()

	log.Printf("valid click %d/%d", clickNumber, validClickLimit)

	w.WriteHeader(http.StatusNoContent)
}

// VERIFY HANDLER
// Validates for one second, then serves index.html
// if the validation succeeds.
func verifyHandler(w http.ResponseWriter, r *http.Request, filePath string) {
	if !reserveClick(w) {
		return
	}

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		releaseReservation()
		log.Println("validation disconnected; click discarded")
		return

	case <-timer.C:
	}

	// Read the page before consuming the reserved slot.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("failed to read index.html:", err)
		releaseReservation()
		http.Error(w, "Could not load page", http.StatusInternalServerError)
		return
	}

	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()
		releaseReservation()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	validClicks++
	reservedClicks--

	clickNumber := validClicks

	mu.Unlock()

	log.Printf("valid click %d/%d", clickNumber, validClickLimit)

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// RESERVE CLICK
func reserveClick(w http.ResponseWriter) bool {
	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
	default:
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	mu.Lock()

	// Make sure completed + currently validating
	// never exceeds the lifetime limit.
	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		<-validationSemaphore
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	reservedClicks++

	mu.Unlock()

	return true
}

// RELEASE RESERVATION
func releaseReservation() {
	mu.Lock()
	reservedClicks--
	mu.Unlock()

	<-validationSemaphore
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






