package main

import (
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// Every accepted request waits this long before the page is served.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks that can ever be counted
	// during the lifetime of this server process.
	validClickLimit = 10

	// Maximum number of requests allowed to wait during validation
	// at the same time.
	maxConcurrentWaits = 500
)

var (
	mu sync.Mutex

	// Successfully completed valid clicks.
	validClicks int

	// Requests that passed the validation delay and have reserved
	// one of the 10 available click slots, but have not yet
	// finished serving index.html.
	reservedClicks int

	// Prevents unlimited numbers of requests from simultaneously
	// sitting in the 1-second validation period.
	waitSemaphore = make(chan struct{}, maxConcurrentWaits)
)

// ---------------------------------------------------------
// HEALTH CHECK
// ---------------------------------------------------------

func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// ---------------------------------------------------------
// PAGE REQUEST
// ---------------------------------------------------------

func landing(w http.ResponseWriter, r *http.Request) {

	// -----------------------------------------------------
	// NO-CACHE / NO-STORE HEADERS
	// -----------------------------------------------------

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0, s-maxage=0",
	)

	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Surrogate-Control", "no-store")
	w.Header().Set("Vary", "*")

	start := time.Now()

	log.Printf(
		"PAGE REQUEST START: %s",
		start.Format(time.RFC3339Nano),
	)

	// -----------------------------------------------------
	// LIMIT SIMULTANEOUS VALIDATION REQUESTS
	// -----------------------------------------------------

	select {

	case waitSemaphore <- struct{}{}:

		defer func() {
			<-waitSemaphore
		}()

	default:

		log.Println("VALIDATION CAPACITY REACHED - 204")

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// -----------------------------------------------------
	// ONE-SECOND SERVER-SIDE DELAY
	// -----------------------------------------------------

	log.Println("STARTING 1 SECOND VALIDATION")

	timer := time.NewTimer(validationTime)

	select {

	case <-timer.C:

		log.Printf(
			"VALIDATION COMPLETE: elapsed=%v",
			time.Since(start),
		)

	case <-r.Context().Done():

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		log.Printf(
			"REQUEST CANCELLED DURING VALIDATION: elapsed=%v",
			time.Since(start),
		)

		return
	}

	// -----------------------------------------------------
	// RESERVE ONE VALID CLICK SLOT
	// -----------------------------------------------------

	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {

		mu.Unlock()

		log.Printf(
			"VALID CLICK LIMIT REACHED: valid=%d reserved=%d - 204",
			validClicks,
			reservedClicks,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	reservedClicks++

	currentReserved := validClicks + reservedClicks

	mu.Unlock()

	log.Printf(
		"VALID CLICK SLOT RESERVED: %d/%d",
		currentReserved,
		validClickLimit,
	)

	// -----------------------------------------------------
	// RELEASE RESERVATION IF REQUEST DOES NOT COMPLETE
	// -----------------------------------------------------

	counted := false

	defer func() {

		if !counted {

			mu.Lock()

			reservedClicks--

			mu.Unlock()

			log.Println(
				"CLICK NOT COUNTED - RESERVATION RELEASED",
			)
		}
	}()

	// -----------------------------------------------------
	// SERVE INDEX
	// -----------------------------------------------------

	log.Printf(
		"VALIDATION COMPLETE - SERVING INDEX: elapsed=%v",
		time.Since(start),
	)

	http.ServeFile(
		w,
		r,
		"public/index.html",
	)

	// -----------------------------------------------------
	// FINALIZE VALID CLICK
	// -----------------------------------------------------

	mu.Lock()

	reservedClicks--
	validClicks++

	currentValidClicks := validClicks

	mu.Unlock()

	counted = true

	log.Printf(
		"VALID CLICK: %d/%d | total elapsed=%v",
		currentValidClicks,
		validClickLimit,
		time.Since(start),
	)
}

// ---------------------------------------------------------
// MAIN
// ---------------------------------------------------------

func main() {

	// -----------------------------------------------------
	// PORT
	// -----------------------------------------------------

	// Use the platform-provided PORT when available.
	// Otherwise use 8080 locally.
	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	// -----------------------------------------------------
	// ROUTES
	// -----------------------------------------------------

	http.HandleFunc("/health", health)
	http.HandleFunc("/", landing)

	// -----------------------------------------------------
	// SERVER
	// -----------------------------------------------------

	log.Printf("SERVER STARTING ON PORT %s", port)

	err := http.ListenAndServe(
		":"+port,
		nil,
	)

	if err != nil {
		log.Fatal(err)
	}
}


