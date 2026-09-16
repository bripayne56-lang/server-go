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
	// How long validation must last.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 40

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50
)

var (
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Clicks currently being validated.
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
	// Waits one second but NEVER counts a click
	// and NEVER sends HTML.
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		if !waitForPrecheck(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	// ROOT
	// This is the actual click/validation endpoint.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveValidatedPage(w, r, filePath)
	})

	// VERIFY
	http.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		serveValidatedPage(w, r, filePath)
	})

	// INDEX.HTML
	http.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		serveValidatedPage(w, r, filePath)
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

// PRECHECK
//
// Performs the one-second connection check.
// It does NOT reserve or consume a click slot.
func waitForPrecheck(r *http.Request) bool {
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println("precheck disconnected; no click counted")
		return false

	case <-timer.C:
		return true
	}
}

// SERVE VALIDATED PAGE
//
// Reserves one of the 40 lifetime click slots,
// waits one second, checks for disconnect,
// then converts the reservation into one valid click.
func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {

	// LOG EVERY REQUEST PATH
	log.Printf("REQUEST: method=%s path=%s", r.Method, r.URL.Path)

	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Reserve a lifetime click slot.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		<-validationSemaphore

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	mu.Unlock()

	// Make sure the reservation is released if anything
	// exits before the click is successfully completed.
	completed := false

	defer func() {
		if !completed {
			mu.Lock()
			reservedClicks--
			mu.Unlock()
		}

		<-validationSemaphore
	}()

	// One-second validation.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return

	case <-timer.C:
	}

	// Read the page before consuming the reservation.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("failed to read index.html:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Convert this reservation into exactly one valid click.
	mu.Lock()

	if validClicks+reservedClicks > validClickLimit {
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks--
	validClicks++

	clickNumber := validClicks

	mu.Unlock()

	completed = true

	log.Printf("valid click %d/%d", clickNumber, validClickLimit)

	// Send the actual page.
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












