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
	// How long a request must remain connected to become valid.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during this server process.
	validClickLimit = 40

	// Maximum number of validations happening at once.
	maxConcurrentWaits = 50
)

var (
	mu sync.Mutex

	// Clicks that successfully completed validation.
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
	// Validates for one second but never sends HTML.
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		if !validateRequest(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	// ROOT
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

// VALIDATE REQUEST
//
// Reserves a lifetime click slot, waits one second,
// and only then converts the reservation into a valid click.
//
// A disconnect before the timer completes releases the
// reservation and does not increment validClicks.
func validateRequest(r *http.Request) bool {
	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
	default:
		return false
	}

	// Reserve one lifetime click slot.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()
		<-validationSemaphore
		return false
	}

	reservedClicks++

	mu.Unlock()

	// Make exactly one outcome responsible for this reservation.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		// The client disconnected before validation completed.
		mu.Lock()

		// Remove the reservation.
		reservedClicks--

		mu.Unlock()

		<-validationSemaphore

		log.Println("validation disconnected; click discarded")
		return false

	case <-timer.C:
		// The full validation period completed first.
	}

	// Convert the reservation into a valid click.
	mu.Lock()

	// The reservation still belongs to this request.
	// Convert it directly into one completed click.
	reservedClicks--
	validClicks++

	clickNumber := validClicks

	mu.Unlock()

	<-validationSemaphore

	log.Printf("valid click %d/%d", clickNumber, validClickLimit)

	return true
}

// SERVE VALIDATED PAGE
func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {
	if !validateRequest(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("failed to read index.html:", err)
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








