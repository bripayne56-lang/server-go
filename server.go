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
	validationTime            = 1 * time.Second
	validClickLimit           = 40
	maxConcurrentValidations  = 500
	ipCooldown                = 3 * time.Second
	confirmationTimeout       = 10 * time.Second
)

type validationSession struct {
	ip        string
	expiresAt time.Time
}

var (
	mu             sync.Mutex
	validClicks    int
	reservedClicks int

	validationSemaphore = make(chan struct{}, maxConcurrentValidations)
	ipTrack             sync.Map
	validationSessions  sync.Map
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	filePath := filepath.Join("public", "index.html")

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveLandingPage(w, r, filePath)
	})

	http.HandleFunc("/precheck", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	http.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		verifyHandler(w, r, filePath)
	})

	http.HandleFunc("/confirm-click", confirmClickHandler)

	http.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		indexHandler(w, r, filePath)
	})

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
				"\nReserved: " + itoa(reserved),
		))
	})

	log.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

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

	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	_, _ = w.Write([]byte("<!-- waiting -->"))
	flusher.Flush()

	time.Sleep(validationTime)

	_, _ = w.Write(page)
	flusher.Flush()
}

func verifyHandler(w http.ResponseWriter, r *http.Request, filePath string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	ip := getClientIP(r)

	// 3-second IP cooldown.
	if ip != "" {
		if lastClickAt, found := ipTrack.Load(ip); found {
			if time.Since(lastClickAt.(time.Time)) < ipCooldown {
				log.Printf("[RATE LIMIT] Rejected IP: %s (too fast). Sending 204.", ip)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		ipTrack.Store(ip, time.Now())
	}

	// Maximum 500 simultaneous validations.
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

	// Reserve one of the 40 available slots.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Println("Rejected: valid click limit completely reached")
		w.WriteHeader(http.StatusNoContent)
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

	// Normally this reservation belongs to this validation.
	// Once a confirmation session is successfully created,
	// the reservation is transferred to that pending session.
	reservationTransferred := false

	defer func() {
		if !reservationTransferred {
			mu.Lock()
			reservedClicks--

			log.Printf(
				"[RESERVATION RELEASED] completed=%d reserved=%d limit=%d",
				validClicks,
				reservedClicks,
				validClickLimit,
			)

			mu.Unlock()
		}
	}()

	// Send an invisible response immediately so the validation can begin.
	_, _ = w.Write([]byte("<!-- waiting for validation -->"))
	flusher.Flush()

	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		log.Println("[DISCONNECT DETECTED] Client disconnected during validation. Click discarded.")
		return

	case <-timer.C:
	}

	// Read the actual page before creating the confirmation session.
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
		return
	}

	// Create a secure one-time confirmation token.
	token, err := generateToken()
	if err != nil {
		log.Println("Failed to generate confirmation token:", err)
		return
	}

	expiresAt := time.Now().Add(confirmationTimeout)

	session := validationSession{
		ip:        ip,
		expiresAt: expiresAt,
	}

	validationSessions.Store(token, session)

	// The reservation now belongs to the pending confirmation session.
	reservationTransferred = true

	// Automatically release the reservation if confirmation never arrives.
	time.AfterFunc(confirmationTimeout, func() {
		value, found := validationSessions.Load(token)
		if !found {
			return
		}

		session := value.(validationSession)

		if time.Now().Before(session.expiresAt) {
			return
		}

		// Only one goroutine can successfully delete the session.
		if validationSessions.CompareAndDelete(token, value) {
			mu.Lock()
			reservedClicks--

			log.Printf(
				"[CONFIRMATION TIMEOUT] IP: %s. Click discarded. completed=%d reserved=%d",
				session.ip,
				validClicks,
				reservedClicks,
			)

			mu.Unlock()
		}
	})

	// Give the browser the one-time token.
	tokenScript := []byte(
		"<script>window.__confirmationToken='" + token + "';</script>\n",
	)

	_, _ = w.Write(tokenScript)
	_, _ = w.Write(page)
	flusher.Flush()

	log.Printf(
		"[PAGE DELIVERED] Confirmation pending. completed=%d reserved=%d",
		getCompletedClicks(),
		getReservedClicks(),
	)
}

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
		if validationSessions.CompareAndDelete(token, value) {
			mu.Lock()
			reservedClicks--

			log.Printf(
				"[CONFIRMATION EXPIRED] IP: %s. Click discarded. completed=%d reserved=%d",
				session.ip,
				validClicks,
				reservedClicks,
			)

			mu.Unlock()
		}

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Confirmation must come from the same IP that started validation.
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

	// Consume the token exactly once.
	if !validationSessions.CompareAndDelete(token, value) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	mu.Lock()

	// Defensive check.
	if validClicks >= validClickLimit {
		reservedClicks--

		log.Printf(
			"[CONFIRMATION REJECTED] Valid click limit reached. completed=%d reserved=%d",
			validClicks,
			reservedClicks,
		)

		mu.Unlock()

		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	// Transfer the reservation into a completed valid click.
	reservedClicks--
	validClicks++

	log.Printf(
		"[CONFIRMED] Click successfully delivered to active browser! %d/%d | reserved=%d",
		validClicks,
		validClickLimit,
		reservedClicks,
	)

	mu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

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

func generateToken() (string, error) {
	var bytes [32]byte

	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes[:]), nil
}

func indexHandler(w http.ResponseWriter, r *http.Request, filePath string) {
	page, err := os.ReadFile(filePath)
	if err != nil {
		log.Println("Failed to read index.html:", err)
		http.Error(w, "Could not load page", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func getCompletedClicks() int {
	mu.Lock()
	defer mu.Unlock()

	return validClicks
}

func getReservedClicks() int {
	mu.Lock()
	defer mu.Unlock()

	return reservedClicks
}

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





