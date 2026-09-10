```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	validationTime = 1 * time.Second

	// Maximum number of valid visits during this
	// server process lifetime.
	validClickLimit = 40

	// Maximum number of validations running at once.
	maxConcurrentValidations = 500

	// How long client records are kept.
	clientLifetime = 24 * time.Hour
)

type clientState struct {
	createdAt time.Time

	// true while this client is currently being validated.
	validating bool

	// true after this client has successfully completed
	// validation and has been counted.
	counted bool
}

var (
	mu sync.Mutex

	validClicks    int
	reservedClicks int

	validationSemaphore = make(chan struct{}, maxConcurrentValidations)

	// Browser IDs.
	clients = make(map[string]*clientState)

	// Fallback protection for multiple simultaneous requests
	// arriving before the browser has received its cookie.
	pending = make(map[string]time.Time)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	// ------------------------------------------------------------
	// HEALTH CHECK
	// ------------------------------------------------------------

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// ------------------------------------------------------------
	// MAIN ENTRY POINT
	//
	// Users visiting the load balancer URL arrive here.
	//
	// There is intentionally NO public /index.html route.
	// ------------------------------------------------------------

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		verifyHandler(w, r, filePath)
	})

	// ------------------------------------------------------------
	// STATUS
	// ------------------------------------------------------------

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

// ------------------------------------------------------------
// VALIDATION
// ------------------------------------------------------------

func verifyHandler(w http.ResponseWriter, r *http.Request, filePath string) {

	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// ----------------------------------------------------------
	// Get browser ID.
	// ----------------------------------------------------------

	clientID, isNew := getClientID(r)

	// If this is a new browser, send its cookie immediately.
	//
	// This is important because the browser needs to receive
	// the cookie BEFORE the one-second validation finishes.
	if isNew {
		http.SetCookie(w, &http.Cookie{
			Name:     "client_id",
			Value:    clientID,
			Path:     "/",
			MaxAge:   int(clientLifetime.Seconds()),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	// ----------------------------------------------------------
	// Required headers.
	// ----------------------------------------------------------

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// ----------------------------------------------------------
	// Obtain Flusher.
	// ----------------------------------------------------------

	flusher, ok := w.(http.Flusher)

	if !ok {
		http.Error(
			w,
			"Streaming not supported",
			http.StatusInternalServerError,
		)
		return
	}

	// ----------------------------------------------------------
	// Lock this browser/client.
	// ----------------------------------------------------------

	mu.Lock()

	cleanupOldClients()

	state, exists := clients[clientID]

	if !exists {
		state = &clientState{
			createdAt: time.Now(),
		}

		clients[clientID] = state
	}

	// Already counted.
	//
	// Serve the page, but DO NOT increment the counter.
	if state.counted {
		mu.Unlock()

		log.Printf(
			"[ALREADY COUNTED] client=%s",
			clientID,
		)

		servePage(w, flusher, filePath)
		return
	}

	// Already being validated.
	//
	// This is the important duplicate protection.
	if state.validating {
		mu.Unlock()

		log.Printf(
			"[DUPLICATE VALIDATION] client=%s",
			clientID,
		)

		http.Error(
			w,
			"Validation already in progress",
			http.StatusConflict,
		)
		return
	}

	// ----------------------------------------------------------
	// Reserve one of the 40 lifetime slots.
	// ----------------------------------------------------------

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"[LIMIT REACHED] completed=%d reserved=%d",
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

	state.validating = true
	reservedClicks++

	mu.Unlock()

	// Always release validation state.
	defer func() {
		mu.Lock()

		state.validating = false
		reservedClicks--

		// If it wasn't counted, remove it so a later
		// legitimate visit can try again.
		if !state.counted {
			delete(clients, clientID)
		}

		mu.Unlock()
	}()

	// ----------------------------------------------------------
	// Concurrency limit.
	// ----------------------------------------------------------

	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		log.Printf(
			"[CONCURRENCY REJECTED] client=%s",
			clientID,
		)

		http.Error(
			w,
			"Too many validations",
			http.StatusServiceUnavailable,
		)
		return
	}

	// ----------------------------------------------------------
	// SEND INVISIBLE DATA.
	//
	// This flushes the response headers and cookie to the browser.
	// ----------------------------------------------------------

	_, _ = w.Write([]byte("<!-- validating -->"))
	flusher.Flush()

	log.Printf(
		"[VALIDATING] client=%s",
		clientID,
	)

	// ----------------------------------------------------------
	// WAIT ONE SECOND.
	// ----------------------------------------------------------

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {

	case <-r.Context().Done():

		// Client disconnected before validation completed.
		log.Printf(
			"[INVALID - DISCONNECTED] client=%s",
			clientID,
		)

		return

	case <-timer.C:

		// One second completed.
	}

	// ----------------------------------------------------------
	// CHECK CONNECTION AGAIN.
	// ----------------------------------------------------------

	select {

	case <-r.Context().Done():

		log.Printf(
			"[INVALID - DISCONNECTED AFTER TIMER] client=%s",
			clientID,
		)

		return

	default:
	}

	// ----------------------------------------------------------
	// READ INDEX.HTML.
	// ----------------------------------------------------------

	page, err := os.ReadFile(filePath)

	if err != nil {
		log.Printf(
			"[FILE ERROR] client=%s: %v",
			clientID,
			err,
		)

		http.Error(
			w,
			"Could not load page",
			http.StatusInternalServerError,
		)

		return
	}

	// ----------------------------------------------------------
	// FINAL CONNECTION CHECK.
	// ----------------------------------------------------------

	select {

	case <-r.Context().Done():

		log.Printf(
			"[INVALID - GONE BEFORE COUNT] client=%s",
			clientID,
		)

		return

	default:
	}

	// ----------------------------------------------------------
	// COUNT EXACTLY ONCE.
	// ----------------------------------------------------------

	mu.Lock()

	// Final duplicate check.
	//
	// Another request can never normally get here because
	// validating=true blocks it, but this makes the count
	// operation itself safe.
	state = clients[clientID]

	if state == nil || state.counted {
		mu.Unlock()

		log.Printf(
			"[DUPLICATE BLOCKED AT COMMIT] client=%s",
			clientID,
		)

		return
	}

	// Final limit check.
	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"[LIMIT REACHED AT COMMIT] client=%s",
			clientID,
		)

		http.Error(
			w,
			"Click limit reached",
			http.StatusTooManyRequests,
		)

		return
	}

	// THIS is the only place the counter increases.
	validClicks++

	// Permanently mark this browser as counted.
	state.counted = true
	state.createdAt = time.Now()

	count := validClicks

	mu.Unlock()

	log.Printf(
		"[VALID CLICK] client=%s count=%d/%d",
		clientID,
		count,
		validClickLimit,
	)

	// ----------------------------------------------------------
	// SEND THE ACTUAL PAGE.
	// ----------------------------------------------------------

	_, err = w.Write(page)

	if err != nil {
		log.Printf(
			"[PAGE WRITE ERROR] client=%s: %v",
			clientID,
			err,
		)

		return
	}

	flusher.Flush()
}

// ------------------------------------------------------------
// GET OR CREATE CLIENT ID
// ------------------------------------------------------------

func getClientID(r *http.Request) (string, bool) {

	cookie, err := r.Cookie("client_id")

	if err == nil && cookie.Value != "" {
		return cookie.Value, false
	}

	// Generate a random browser ID.
	buf := make([]byte, 16)

	if _, err := rand.Read(buf); err != nil {
		// Extremely unlikely.
		// Fall back to a deterministic temporary identifier.
		return fallbackClientID(r), true
	}

	return hex.EncodeToString(buf), true
}

// ------------------------------------------------------------
// FALLBACK CLIENT ID
// ------------------------------------------------------------
//
// This is only used if crypto/rand fails.
//
// ------------------------------------------------------------

func fallbackClientID(r *http.Request) string {

	ip := clientIP(r)

	userAgent := r.Header.Get("User-Agent")

	return ip + "|" + userAgent
}

// ------------------------------------------------------------
// CLIENT IP
// ------------------------------------------------------------

func clientIP(r *http.Request) string {

	host, _, err := net.SplitHostPort(r.RemoteAddr)

	if err == nil {
		return host
	}

	return strings.TrimSpace(r.RemoteAddr)
}

// ------------------------------------------------------------
// CLEAN OLD CLIENTS
// ------------------------------------------------------------

func cleanupOldClients() {

	now := time.Now()

	for id, state := range clients {

		if state.validating {
			continue
		}

		if now.Sub(state.createdAt) > clientLifetime {
			delete(clients, id)
		}
	}
}

// ------------------------------------------------------------
// SERVE PAGE WITHOUT COUNTING
// ------------------------------------------------------------

func servePage(
	w http.ResponseWriter,
	flusher http.Flusher,
	filePath string,
) {

	page, err := os.ReadFile(filePath)

	if err != nil {
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

	flusher.Flush()
}

// ------------------------------------------------------------
// SIMPLE INTEGER CONVERSION
// ------------------------------------------------------------

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






