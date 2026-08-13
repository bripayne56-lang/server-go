package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	validationTime     = 1 * time.Second
	validClickLimit    = 10
	maxConcurrentWaits = 50
)

var (
	mu sync.Mutex

	validClicks int

	waitSemaphore = make(chan struct{}, maxConcurrentWaits)
)

// ---------------------------------------------------------
// HEALTH CHECK
// ---------------------------------------------------------

func health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------
// PAGE REQUEST / VALIDATION
// ---------------------------------------------------------

func landing(w http.ResponseWriter, r *http.Request) {

	// Limit simultaneous validation waits.
	select {

	case waitSemaphore <- struct{}{}:

		defer func() {
			<-waitSemaphore
		}()

	default:

		w.WriteHeader(http.StatusNoContent)
		return
	}

	log.Println("PAGE REQUEST received")

	// -----------------------------------------------------
	// ONE-SECOND VALIDATION GATE
	// -----------------------------------------------------

	time.Sleep(validationTime)

	// -----------------------------------------------------
	// CHECK VALID CLICK LIMIT
	// -----------------------------------------------------

	mu.Lock()

	if validClicks >= validClickLimit {

		mu.Unlock()

		log.Println("VALID CLICK LIMIT REACHED - 204")

		w.WriteHeader(http.StatusNoContent)
		return
	}

	mu.Unlock()

	// -----------------------------------------------------
	// DISABLE BROWSER CACHING
	// -----------------------------------------------------

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)

	w.Header().Set(
		"Pragma",
		"no-cache",
	)

	w.Header().Set(
		"Expires",
		"0",
	)

	// -----------------------------------------------------
	// SERVE YOUR EXISTING PAGE
	// -----------------------------------------------------

	log.Println("VALIDATION COMPLETE - SERVING INDEX")

	http.ServeFile(
		w,
		r,
		"public/index.html",
	)

	// -----------------------------------------------------
	// COUNT VALID CLICK
	// -----------------------------------------------------

	mu.Lock()

	validClicks++

	currentValidClicks := validClicks

	mu.Unlock()

	log.Printf(
		"VALID CLICK: %d/%d",
		currentValidClicks,
		validClickLimit,
	)
}

// ---------------------------------------------------------
// MAIN
// ---------------------------------------------------------

func main() {

	http.HandleFunc(
		"/health",
		health,
	)

	http.HandleFunc(
		"/",
		landing,
	)

	fmt.Println(
		"Server running on :8080",
	)

	err := http.ListenAndServe(
		":8080",
		nil,
	)

	if err != nil {
		log.Fatal(err)
	}
}

