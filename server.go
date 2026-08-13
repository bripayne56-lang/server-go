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

	// Start the 1-second validation timer.
	timer := time.NewTimer(validationTime)

	defer timer.Stop()

	select {

	// -----------------------------------------------------
	// BROWSER CANCELLED THE REQUEST
	// -----------------------------------------------------

	case <-r.Context().Done():

		log.Println("REQUEST CANCELLED - invalid click")

		w.WriteHeader(http.StatusNoContent)
		return

	// -----------------------------------------------------
	// 1 SECOND COMPLETED
	// -----------------------------------------------------

	case <-timer.C:

		mu.Lock()

		// Check the global valid-click limit.
		if validClicks >= validClickLimit {

			mu.Unlock()

			log.Println("VALID CLICK LIMIT REACHED - 204")

			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Count this as a valid click.
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
	// SERVE THE REAL PAGE
	// ---------------------------------------------------------

	w.Header().Set(
		"Cache-Control",
		"public, max-age=31536000",
	)

	http.ServeFile(
		w,
		r,
		"public/index.html",
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




