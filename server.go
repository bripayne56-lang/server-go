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
	// Validation delay.
	validationTime = 1 * time.Second

	// Maximum number of valid clicks during the lifetime
	// of this server process.
	validClickLimit = 40

	// Maximum number of validations happening at once.
	maxConcurrentValidations = 100

	// How long an IP is blocked between clicks.
	ipCooldown = 3 * time.Second

	// How long a successful validation can wait
	// for the browser to confirm page delivery.
	confirmationTimeout = 10 * time.Second
)

type validationSession struct {
	ip        string
	expiresAt time.Time
}

var (
	mu sync.Mutex

	// Clicks that successfully completed validation
	// AND were confirmed by the browser.
	validClicks int

	// Clicks currently going through validation.
	reservedClicks int

	// Limits simultaneous validations.
	validationSemaphore = make(chan struct{}, maxConcurrentValidations)

	// Stores the last click time for each IP.
	ipTrack sync.Map

	// Stores validation sessions waiting for browser confirmation.
	validationSessions sync.Map
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
	// sending the actual index.html.
	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// VERIFY
	// Kept available in case it is needed later.
	http.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	// CONFIRM CLICK
	// Called by index.html after the browser receives
	// and begins rendering the page.
	http.HandleFunc("/confirm-click", confirmClickHandler)

	// INDEX.HTML
	// Serves the actual landing page.
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
// as one of the 40 valid clicks.
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
// Applies the IP cooldown,
// reserves one of the 40 lifetime slots,
// waits one second,
// checks whether the client stayed connected,
// then sends index.html with a one-time
// confirmation token.
//
// The click is NOT counted yet.
// It is counted only when /confirm-click
// successfully confirms the browser received the page.
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

	// 1. IP RATE LIMITING CHECK (GCP Load Balancer Compatible)
	ip := getClientIP(r)

	if ip != "" {
		if lastClickAt, found := ipTrack.Load(ip); found {
			if time.Since(lastClickAt.(time.Time)) < ipCooldown {
				log.Printf(
					"[RATE LIMIT] Rejected IP: %s (too fast). Sending 204.",
					ip,
				)

				// Return 204 No Content instead of 429.
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		// Track or update this IP's arrival timestamp.
		ipTrack.Store(ip, time.Now())
	}

	// 2. CONCURRENCY SEMAPHORE
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

	// 3. CAPACITY PRE-CHECK
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Println("Rejected: valid click limit completely reached")
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

	// Send invisible validation data.
	// Nothing visual appears on the page.
	_, _ = w.Write([]byte("<!-- waiting for validation -->"))
	flusher.Flush()

	// 4. WATCH TIMER & CONTEXT DROPS
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println(
			"[DISCONNECT DETECTED] Client disconnected during validation. Click discarded.",
		)
		return

	case <-timer.C:
		// Validation completed.
	}

	// 5. READ THE ACTUAL PAGE
	// Do this before creating a confirmation session.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
		return
	}

	// 6. CREATE ONE-TIME CONFIRMATION SESSION
	token, err := generateToken()
	if err != nil {
		log.Println("Failed to generate confirmation token:", err)
		return
	}

	validationSessions.Store(token, validationSession{
		ip:        ip,
		expiresAt: time.Now().Add(confirmationTimeout),
	})

	// If the browser never confirms, clean up the session
	// and release the reserved slot after the timeout.
	time.AfterFunc(confirmationTimeout, func() {
		if value, found := validationSessions.Load(token); found {
			session := value.(validationSession)

			if time.Now().After(session.expiresAt) {
				if validationSessions.CompareAndDelete(token, value) {
					log.Printf(
						"[CONFIRMATION TIMEOUT] IP: %s. Click discarded.",
						session.ip,
					)
				}
			}
		}
	})

	// 7. INJECT THE ONE-TIME CONFIRMATION TOKEN
	// The token allows index.html to prove that this
	// page came from a completed validation.
	tokenScript := []byte(
		"<script>window.__confirmationToken=" +
			"'" + token + "';</script>\n",
	)

	_, _ = w.Write(tokenScript)
	_, _ = w.Write(page)
	flusher.Flush()

	// The click remains reserved until /confirm-click
	// confirms the browser received the page.
}

// CONFIRM CLICK
// Called by index.html after the browser receives
// and begins rendering the page.
//
// The request must provide a valid one-time token.
// The token must belong to the same IP that passed validation.
// A successful confirmation increments validClicks.
func confirmClickHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	value, found := validationSessions.Load(token)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	session := value.(validationSession)

	// Check expiration.
	if time.Now().After(session.expiresAt) {
		validationSessions.Delete(token)

		log.Printf(
			"[CONFIRMATION EXPIRED] IP: %s",
			session.ip,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Confirm that the browser's IP matches
	// the IP that completed validation.
	ip := getClientIP(r)

	if ip == "" || ip != session.ip {
		log.Printf(
			"[CONFIRMATION REJECTED] IP mismatch. Expected=%s Received=%s",
			session.ip,
			ip,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Consume the token BEFORE incrementing the count.
	// This prevents the same token from being confirmed twice.
	if !validationSessions.CompareAndDelete(token, value) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Final lifetime-capacity check.
	mu.Lock()

	if validClicks >= validClickLimit {
		mu.Unlock()

		log.Println(
			"[CONFIRMATION REJECTED] Valid click limit reached.",
		)

		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	validClicks++

	log.Printf(
		"[CONFIRMED] Click successfully delivered to active browser! %d/%d",
		validClicks,
		validClickLimit,
	)

	mu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// GET CLIENT IP
// Uses X-Forwarded-For when behind the GCP Load Balancer.
// The first IP in the list is treated as the original client.
// Falls back to RemoteAddr for local testing.
func getClientIP(r *http.Request) string {
	var ip string

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip = xff

		for i, ch := range xff {
			if ch == ',' {
				ip = xff[:i]
				break
			}
		}

		ip = strings.TrimSpace(ip)
	} else {
		var err error

		ip, _, err = net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}

		ip = strings.TrimSpace(ip)
	}

	return ip
}

// GENERATE RANDOM CONFIRMATION TOKEN
func generateToken() (string, error) {
	var bytes [32]byte

	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes[:]), nil
}

// INDEX.HTML
// Serves the actual page.
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





