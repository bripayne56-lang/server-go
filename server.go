```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	validationTime = 1 * time.Second

	// Maximum number of valid browser visits during
	// the lifetime of this server process.
	validClickLimit = 40

	// Maximum number of validations at once.
	maxConcurrentValidations = 500

	// How long a browser ID remains remembered.
	clientIDLifetime = 24 * time.Hour
)

type clientRecord struct {
	LastSeen time.Time
	Counted  bool
}

var (
	mu sync.Mutex

	validClicks    int
	reservedClicks int

	validationSemaphore = make(chan struct{}, maxConcurrentValidations)

	// Browser/client IDs that have already been counted.
	clients = make(map[string]clientRecord)
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

	// MAIN ENTRY POINT
	//
	// Users visiting the load balancer URL arrive here.
	//
	// Example:
	//
	// https://your-load-balancer-url/
	//
	// There is no button and no JavaScript required.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

		_, _ = w.Write([]byte(
			"Completed: " + itoa(completed) +
				"/" + itoa(validClickLimit) +
				"\nValidating: " + itoa(reserved),
		))
	})

	log.Println("Server running on port", port)

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// VERIFY HANDLER
//
// Every visit to "/" comes here.
//
// Flow:
//
//   browser
//      ↓
//   load balancer
//      ↓
//   "/"
//      ↓
//   1 second validation
//      ↓
//   browser still connected?
//      ↓
//   has this browser already been counted?
//      ↓
//   count once
//      ↓
//   return index.html
//
func verifyHandler(w http.ResponseWriter, r *http.Request, filePath string) {

	// Only GET is needed because users are simply visiting the URL.
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Don't let the browser/cache store this response.
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")

	// ------------------------------------------------------------
	// Get or create the browser ID.
	// ------------------------------------------------------------

	clientID := getClientID(w, r)

	// ------------------------------------------------------------
	// Check whether this browser has already been counted.
	// ------------------------------------------------------------

	mu.Lock()

	// Clean up old client records.
	now := time.Now()

	for id, record := range clients {
		if now.Sub(record.LastSeen) > clientIDLifetime {
			delete(clients, id)
		}
	}

	record, exists := clients[clientID]

	if exists && record.Counted {
		clients[clientID] = clientRecord{
			LastSeen: now,
			Counted:  true,
		}

		mu.Unlock()

		log.Printf(
			"[DUPLICATE] client=%s already counted",
			clientID,
		)

		// Already counted.
		// Serve the page without incrementing the counter.
		servePage(w, filePath)
		return
	}

	// ------------------------------------------------------------
	// Reserve a validation slot.
	// ------------------------------------------------------------

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"[REJECTED] limit reached: completed=%d reserved=%d",
			validClicks,
			reservedClicks,
		)

		http.Error(
			w,
			"Click limit reached",
			http.StatusTooManyRequests,
		)
		return
	}

	reservedClicks++

	// Remember that this client is currently being validated.
	clients[clientID] = clientRecord{
		LastSeen: now,
		Counted:  false,
	}

	log.Printf(
		"[RESERVED] client=%s completed=%d reserved=%d limit=%d",
		clientID,
		validClicks,
		reservedClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Always release the reservation.
	defer func() {
		mu.Lock()
		reservedClicks--
		mu.Unlock()
	}()

	// ------------------------------------------------------------
	// Limit simultaneous validations.
	// ------------------------------------------------------------

	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		log.Printf(
			"[REJECTED] concurrency limit reached client=%s",
			clientID,
		)

		http.Error(
			w,
			"Too many validations in progress",
			http.StatusServiceUnavailable,
		)
		return
	}

	// ------------------------------------------------------------
	// Wait one second.
	// ------------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():

		log.Printf(
			"[INVALID] client=%s disconnected during validation",
			clientID,
		)

		removeUncountedClient(clientID)
		return

	case <-timer.C:
	}

	// ------------------------------------------------------------
	// Check connection after validation.
	// ------------------------------------------------------------

	select {
	case <-r.Context().Done():

		log.Printf(
			"[INVALID] client=%s disconnected after validation",
			clientID,
		)

		removeUncountedClient(clientID)
		return

	default:
	}

	// ------------------------------------------------------------
	// Load the page before counting.
	// ------------------------------------------------------------

	page, err := os.ReadFile(filePath)

	if err != nil {
		log.Println("Failed to read index.html:", err)
		removeUncountedClient(clientID)
		http.Error(
			w,
			"Could not load page",
			http.StatusInternalServerError,
		)
		return
	}

	// ------------------------------------------------------------
	// Final connection check.
	// ------------------------------------------------------------

	select {
	case <-r.Context().Done():

		log.Printf(
			"[INVALID] client=%s disconnected before commit",
			clientID,
		)

		removeUncountedClient(clientID)
		return

	default:
	}

	// ------------------------------------------------------------
	// COUNT THE VISIT.
	//
	// This section is protected by the mutex so two simultaneous
	// requests cannot increment the counter for the same client.
	// ------------------------------------------------------------

	mu.Lock()

	// Another request from the same browser may have completed
	// while this request was validating.
	record = clients[clientID]

	if record.Counted {
		mu.Unlock()

		log.Printf(
			"[DUPLICATE] client=%s was counted by another request",
			clientID,
		)

		_, _ = w.Write(page)
		return
	}

	// Re-check the limit.
	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"[REJECTED] limit reached before commit client=%s",
			clientID,
		)

		http.Error(
			w,
			"Click limit reached",
			http.StatusTooManyRequests,
		)
		return
	}

	// Mark this browser as counted.
	clients[clientID] = clientRecord{
		LastSeen: time.Now(),
		Counted:  true,
	}

	validClicks++

	count := validClicks

	mu.Unlock()

	log.Printf(
		"[VALID] client=%s count=%d/%d",
		clientID,
		count,
		validClickLimit,
	)

	// ------------------------------------------------------------
	// Send the actual page.
	// ------------------------------------------------------------

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(page)

	if err != nil {
		log.Printf(
			"[WARNING] client=%s page delivery failed: %v",
			clientID,
			err,
		)
	}
}

// GET OR CREATE CLIENT ID
//
// A cookie identifies the browser for subsequent visits.
//
// This prevents:
//   refresh → count again
//   retry   → count again
//   same browser opening another request → count again
//
func getClientID(w http.ResponseWriter, r *http.Request) string {

	cookie, err := r.Cookie("client_id")

	if err == nil && cookie.Value != "" {
		return cookie.Value
	}

	// Generate a cryptographically random ID.
	randomBytes := make([]byte, 16)

	if _, err := rand.Read(randomBytes); err != nil {
		http.Error(
			w,
			"Could not create client ID",
			http.StatusInternalServerError,
		)
		return ""
	}

	clientID := hex.EncodeToString(randomBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     "client_id",
		Value:    clientID,
		Path:     "/",
		MaxAge:   int(clientIDLifetime.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	return clientID
}

// REMOVE A CLIENT THAT FAILED VALIDATION.
func removeUncountedClient(clientID string) {
	mu.Lock()
	defer mu.Unlock()

	record, exists := clients[clientID]

	if exists && !record.Counted {
		delete(clients, clientID)
	}
}

// SERVE PAGE WITHOUT COUNTING.
//
// Used when the same browser comes back after it has already
// been counted.
func servePage(w http.ResponseWriter, filePath string) {

	page, err := os.ReadFile(filePath)

	if err != nil {
		log.Println("Failed to read index.html:", err)

		http.Error(
			w,
			"Could not load page",
			http.StatusInternalServerError,
		)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
```





