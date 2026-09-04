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
	maxConcurrentWaits = 500
)

var (
	mu sync.Mutex

	// Completed valid clicks.
	validClicks int

	// Click slots that passed validation but have not
	// finished ServeFile() yet.
	reservedClicks int

	// Limits simultaneous validation requests.
	waitSemaphore = make(chan struct{}, maxConcurrentWaits)
)

// ---------------------------------------------------------
// HEALTH CHECK
// ---------------------------------------------------------

func health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------
// INTERNAL VALIDATION
// ---------------------------------------------------------

func validate(w http.ResponseWriter, r *http.Request) {

	log.Println("VALIDATION POST received")

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {

	case <-timer.C:

		log.Println("VALIDATION PASS")

		w.WriteHeader(http.StatusOK)

	case <-r.Context().Done():

		log.Println("VALIDATION CANCELLED")

		return
	}
}

// ---------------------------------------------------------
// PAGE REQUEST
// ---------------------------------------------------------

func landing(w http.ResponseWriter, r *http.Request) {

	// -----------------------------------------------------
	// LIMIT SIMULTANEOUS VALIDATIONS
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

	log.Println("PAGE REQUEST received")

	// -----------------------------------------------------
	// BLOCKING SERVER-SIDE POST
	// -----------------------------------------------------

	validationReq, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		"http://127.0.0.1:8080/validate",
		nil,
	)

	if err != nil {

		log.Println("VALIDATION REQUEST ERROR")

		w.WriteHeader(http.StatusNoContent)
		return
	}

	log.Println("STARTING BLOCKING VALIDATION POST")

	validationResp, err := http.DefaultClient.Do(validationReq)

	if err != nil {

		log.Println("VALIDATION POST FAILED")

		return
	}

	defer validationResp.Body.Close()

	// -----------------------------------------------------
	// VALIDATION VERDICT
	// -----------------------------------------------------

	if validationResp.StatusCode != http.StatusOK {

		log.Println("VALIDATION FAILED - 204")

		w.WriteHeader(http.StatusNoContent)
		return
	}

	log.Println("VALIDATION PASSED")

	// -----------------------------------------------------
	// RESERVE VALID CLICK SLOT
	// -----------------------------------------------------

	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {

		mu.Unlock()

		log.Println("VALID CLICK LIMIT REACHED - 204")

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
	// RELEASE RESERVATION IF NOT COMPLETED
	// -----------------------------------------------------

	counted := false

	defer func() {

		if !counted {

			mu.Lock()

			reservedClicks--

			mu.Unlock()

			log.Println("CLICK NOT COUNTED - RESERVATION RELEASED")
		}
	}()

	// -----------------------------------------------------
	// RESPONSE HEADERS
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
	// SERVE INDEX
	// -----------------------------------------------------

	log.Println("VALIDATION COMPLETE - SERVING INDEX")

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
		"/validate",
		validate,
	)

	http.HandleFunc(
		"/",
		landing,
	)

	fmt.Println("Server running on :8080")

	err := http.ListenAndServe(
		":8080",
		nil,
	)

	if err != nil {
		log.Fatal(err)
	}
}
