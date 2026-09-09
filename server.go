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
	// Validation delay.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during the lifetime
	// of this server process.
	validClickLimit = 10

	// Maximum number of validations happening at once.
	maxConcurrentValidations = 500
)

var (
	mu sync.Mutex

	// Clicks that successfully completed validation.
	validClicks int

	// Clicks currently going through validation.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentValidations)
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

	// LANDING PAGE
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveLandingPage(w, r, filePath)
	})

	// PRECHECK
	// This is the public entry point.
	// It performs the 1-second validation before
	// sending the user to the hidden gate.
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// VERIFY
	// Kept available in case it is needed later.
	http.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// GATE
	// Holds the browser for another 1 second,
	// then sends it to index.html.
	http.HandleFunc("/gate", func(w http.ResponseWriter, r *http.Request) {
		gateHandler(w, r)
	})

	// INDEX.HTML
	// Serves the actual landing page after /gate.
	http.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		indexHandler(w, r, filePath)
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

// SERVE LANDING PAGE
// Used for "/".
// This delays the page but does not count the visit
// as one of the 10 valid clicks.
func serveLandingPage(w http.ResponseWriter, r *http.Request, filePath string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
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

	// Invisible data is sent first.
	_, _ = w.Write([]byte("<!-- waiting -->"))
	flusher.Flush()

	// Wait one second.
	time.Sleep(validationTime)

	// Send the actual page.
	_, _ = w.Write(page)
	flusher.Flush()
}

// VERIFY HANDLER
// Used by /precheck and /verify.
//
// Reserves one of the 10 lifetime slots,
// waits one second,
// checks whether the client stayed connected,
// then counts the click and sends the user to /gate.
func verifyHandler(w http.ResponseWriter, r *http.Request, filePath string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// Anti-caching headers.
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Limit simultaneous validations.
	select {
	case validationSemaphore <- struct{}{}:
		defer func() {
			<-validationSemaphore
		}()

	default:
		log.Println("Rejected: concurrency limit reached")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Reserve one of the 10 available lifetime slots.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Println("Rejected: valid click limit reached")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	reservedClicks++

	log.Printf(
		"[VERIFY] Reserved: completed=%d reserved=%d limit=%d",
		validClicks,
		reservedClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Release the reservation when this request ends.
	defer func() {
		mu.Lock()
		reservedClicks--
		mu.Unlock()
	}()

	// Send invisible data first.
	// Nothing visual appears on the page.
	_, _ = w.Write([]byte("<!-- waiting for validation -->"))
	flusher.Flush()

	// Wait one second while watching for
	// the client disconnecting.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println("[DISCONNECT DETECTED] Client disconnected during validation.")
		return

	case <-timer.C:
		// Validation completed.
	}

	// Read the actual index.html to make sure it exists.
	_, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
		return
	}

	// Count the click only after the full
	// validation period has completed.
	mu.Lock()
	validClicks++

	log.Printf(
		"[VERIFY] Valid click: %d/%d",
		validClicks,
		validClickLimit,
	)

	mu.Unlock()

	// Send the browser to the gate.
	_, _ = w.Write([]byte(`
<script>
	window.location.href = "/gate";
</script>
`))
	flusher.Flush()
}

// GATE HANDLER
// The browser reaches this after successful validation.
// It waits one additional second before navigating
// to the actual index.html page.
func gateHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate, max-age=0",
	)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	w.WriteHeader(http.StatusOK)

	// The page is visually blank.
	// JavaScript waits one second before navigating.
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html>
<head>
	<meta charset="utf-8">
	<script>
		setTimeout(function() {
			window.location.href = "/index.html";
		}, 1000);
	</script>
</head>
<body></body>
</html>`))
}

// INDEX.HTML
// Serves the actual page after the gate.
func indexHandler(w http.ResponseWriter, r *http.Request, filePath string) {
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
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



