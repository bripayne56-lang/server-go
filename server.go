package main

import (
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	// Every accepted request waits this long before completing.
	validationTime = 1 * time.Second

	// Maximum number of clicks that can ever be successfully completed
	// during the lifetime of this server process.
	validClickLimit = 10

	// Maximum number of clicks allowed to be validating simultaneously.
	maxConcurrentValidations = 500
)

var (
	mu sync.Mutex

	// Clicks that have successfully completed validation.
	validClicks int

	// Clicks that have claimed one of the 10 lifetime slots
	// and are currently waiting through the validation period.
	reservedClicks int

	// Limits the number of requests simultaneously inside validation.
	validationSemaphore = make(chan struct{}, maxConcurrentValidations)
)

// HEALTH CHECK
func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// LANDING PAGE
func landing(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(
			w,
			"Streaming not supported by server configuration",
			http.StatusInternalServerError,
		)
		return
	}

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Send only invisible HTML data first.
	// Nothing visible is displayed from this.
	_, _ = w.Write([]byte("<!-- waiting for validation -->"))
	flusher.Flush()

	// Wait 1 second before sending the actual landing page.
	time.Sleep(validationTime)

	// Send the visible landing page.
	_, _ = w.Write([]byte(`
<!doctype html>
<html>
<head>
	<meta charset="utf-8">
	<title>Landing Page</title>
</head>
<body>
	<h1>Landing Page</h1>
</body>
</html>
`))

	flusher.Flush()
}

// EXPLICIT USER ACTION / CLICK VALIDATION
func action(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(
			w,
			"Streaming not supported by server configuration",
			http.StatusInternalServerError,
		)
		return
	}

	// Limit the number of clicks validating at the same time.
	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		http.Error(
			w,
			"Server Busy: too many concurrent validations",
			http.StatusServiceUnavailable,
		)
		return
	}

	// Reserve one of the 10 lifetime slots.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		http.Error(
			w,
			"Click limit reached",
			http.StatusTooManyRequests,
		)
		return
	}

	reservedClicks++

	log.Printf(
		"[CLICK] Reserved slot: completed=%d reserved=%d limit=%d",
		validClicks,
		reservedClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Release the reservation when this request finishes.
	defer func() {
		mu.Lock()
		reservedClicks--
		mu.Unlock()
	}()

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Send only invisible HTML data before the delay.
	_, _ = w.Write([]byte("<!-- waiting for validation -->"))
	flusher.Flush()

	// Wait 1 second.
	time.Sleep(validationTime)

	// Validation completed successfully.
	mu.Lock()
	validClicks++

	log.Printf(
		"[CLICK] Completed: %d/%d (still validating: %d)",
		validClicks,
		validClickLimit,
		reservedClicks-1,
	)

	mu.Unlock()

	// Send the visible result after the delay.
	_, _ = w.Write([]byte(`
<div>
	Click successfully validated.
</div>
`))

	flusher.Flush()
}

// STATUS ENDPOINT
func status(w http.ResponseWriter, r *http.Request) {
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

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", health)
	mux.HandleFunc("/action", action)
	mux.HandleFunc("/status", status)
	mux.HandleFunc("/", landing)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,

		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Println("Server running on http://localhost:8080")

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}





