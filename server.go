package main

import (
	"log"
	"net"
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

	// Minimum time between requests from the same IP.
	ipCooldown = 3 * time.Second
)

var (
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Clicks currently being validated.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentWaits)

	// Stores the most recent request time for each IP.
	ipTrack sync.Map
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// Only GET / can enter validation.
	// Every other path returns 204 and does not count.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		serveValidatedPage(w, r, filePath)
	})

	log.Println("Server running on port", port)

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Waits one second, checks for disconnect,
// then converts the request into one valid click.
func serveValidatedPage(w http.ResponseWriter, r *http.Request, filePath string) {
	log.Printf(
		"REQUEST: method=%s path=%s",
		r.Method,
		r.URL.Path,
	)

	// Get the client's source IP.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}

	// Reject requests from the same IP that arrive
	// within the three-second cooldown period.
	now := time.Now()

	if lastRequest, found := ipTrack.Load(ip); found {
		if now.Sub(lastRequest.(time.Time)) < ipCooldown {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Record this request's arrival time.
	ipTrack.Store(ip, now)

	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Reserve a lifetime click slot.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	log.Printf(
		"reserved click: completed=%d validating=%d",
		validClicks,
		reservedClicks,
	)

	mu.Unlock()

	// Make sure the reservation is released if the request
	// does not successfully become a valid click.
	completed := false

	defer func() {
		if !completed {
			mu.Lock()
			reservedClicks--
			mu.Unlock()
		}
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

	// Check for disconnect after the one-second validation.
	if r.Context().Err() != nil {
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Read the page before consuming the reservation.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("failed to read index.html:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Final disconnect check before counting.
	if r.Context().Err() != nil {
		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Convert the reservation into one valid click.
	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Final disconnect check before incrementing.
	if r.Context().Err() != nil {
		mu.Unlock()

		log.Println("validation disconnected; click discarded")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks--
	validClicks++

	clickNumber := validClicks

	mu.Unlock()

	completed = true

	log.Printf(
		"valid click %d/%d",
		clickNumber,
		validClickLimit,
	)

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


