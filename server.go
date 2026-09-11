package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	validationTime           = 1 * time.Second
	validClickLimit          = 40
	maxConcurrentValidations = 500
	ipCooldown               = 3 * time.Second
	confirmationTimeout      = 10 * time.Second
)

var (
	mu sync.Mutex

	validClicks    int
	reservedClicks int

	ipTrack sync.Map

	validationSlots = make(chan struct{}, maxConcurrentValidations)

	sessions sync.Map
)

type validationSession struct {
	ip        string
	expiresAt time.Time
}

func getClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}

	return r.RemoteAddr
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	completed := validClicks
	reserved := reservedClicks
	mu.Unlock()

	w.Header().Set("Content-Type", "text/plain")

	fmt.Fprintf(
		w,
		"completed=%d reserved=%d limit=%d\n",
		completed,
		reserved,
		validClickLimit,
	)
}

func verifyHandler(w http.ResponseWriter, r *http.Request) {
	ip := getClientIP(r)
	now := time.Now()

	// 3-second IP cooldown.
	if lastValue, ok := ipTrack.Load(ip); ok {
		lastTime := lastValue.(time.Time)

		if now.Sub(lastTime) < ipCooldown {
			log.Printf(
				"[RATE LIMIT] Rejected IP: %s (too fast). Sending 204.",
				ip,
			)

			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	ipTrack.Store(ip, now)

	// Limit simultaneous validations.
	select {
	case validationSlots <- struct{}{}:
		defer func() {
			<-validationSlots
		}()
	default:
		log.Printf(
			"[CONCURRENCY] Rejected IP: %s. Too many validations.",
			ip,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Reserve a click slot.
	mu.Lock()

	if validClicks+reservedClicks >= validClickLimit {
		mu.Unlock()

		log.Printf(
			"[LIMIT] Click limit reached. IP: %s. Sending 204.",
			ip,
		)

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

	reservationTransferred := false

	// If validation exits before creating a confirmation session,
	// release the reservation.
	defer func() {
		if !reservationTransferred {
			mu.Lock()
			reservedClicks--

			log.Printf(
				"[DISCARDED] IP: %s. Validation did not complete. completed=%d reserved=%d",
				ip,
				validClicks,
				reservedClicks,
			)

			mu.Unlock()
		}
	}()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send an initial invisible response and flush it.
	if flusher, ok := w.(http.Flusher); ok {
		_, _ = w.Write([]byte("<!-- validation started -->"))
		flusher.Flush()
	}

	// Wait 1 second.
	timer := time.NewTimer(validationTime)
	defer timer.Stop()

	select {
	case <-timer.C:
		// Validation completed.
	case <-r.Context().Done():
		log.Printf(
			"[EARLY EXIT] IP: %s left before 1-second validation completed.",
			ip,
		)
		return
	}

	// Read the actual page.
	page, err := readIndexHTML()
	if err != nil {
		log.Printf("[ERROR] Could not read index.html: %v", err)
		return
	}

	// Create a random confirmation token.
	tokenBytes := make([]byte, 32)

	if _, err := rand.Read(tokenBytes); err != nil {
		log.Printf("[ERROR] Could not create confirmation token: %v", err)
		return
	}

	token := hex.EncodeToString(tokenBytes)

	// Store the pending confirmation.
	sessions.Store(token, validationSession{
		ip:        ip,
		expiresAt: time.Now().Add(confirmationTimeout),
	})

	// The reservation now belongs to the confirmation session.
	reservationTransferred = true

	log.Printf(
		"[PAGE DELIVERED] Confirmation pending. completed=%d reserved=%d",
		validClicks,
		reservedClicks,
	)

	// Go automatically adds the confirmation JavaScript
	// to the page. No manual editing of index.html is needed.
	confirmationScript := `<script>
fetch('/confirm-click?token=` + token + `', {
	method: 'POST'
});
</script>`

	// Insert the confirmation script immediately before </body>.
	// If </body> isn't found, append the script to the page.
	if bytes.Contains(page, []byte("</body>")) {
		page = bytes.Replace(
			page,
			[]byte("</body>"),
			[]byte(confirmationScript+"</body>"),
			1,
		)
	} else {
		page = append(page, []byte(confirmationScript)...)
	}

	// Send the completed page.
	_, _ = w.Write(page)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// If the browser never confirms within 10 seconds,
	// discard the pending click.
	go func(token, ip string) {
		<-time.After(confirmationTimeout)

		if _, deleted := sessions.LoadAndDelete(token); !deleted {
			return
		}

		mu.Lock()
		reservedClicks--

		log.Printf(
			"[CONFIRMATION TIMEOUT] IP: %s. Click discarded. completed=%d reserved=%d",
			ip,
			validClicks,
			reservedClicks,
		)

		mu.Unlock()
	}(token, ip)
}

func confirmClickHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ip := getClientIP(r)
	token := r.URL.Query().Get("token")

	if token == "" {
		log.Printf(
			"[CONFIRM REQUEST] IP: %s missing token.",
			ip,
		)

		w.WriteHeader(http.StatusBadRequest)
		return
	}

	value, ok := sessions.Load(token)

	if !ok {
		log.Printf(
			"[CONFIRM REJECTED] IP: %s invalid or expired token.",
			ip,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	session := value.(validationSession)

	// Check expiration.
	if time.Now().After(session.expiresAt) {
		if _, deleted := sessions.LoadAndDelete(token); deleted {
			mu.Lock()
			reservedClicks--

			log.Printf(
				"[CONFIRMATION EXPIRED] IP: %s. Click discarded. completed=%d reserved=%d",
				ip,
				validClicks,
				reservedClicks,
			)

			mu.Unlock()
		}

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Confirmation must come from the same IP.
	if session.ip != ip {
		log.Printf(
			"[CONFIRM REJECTED] IP mismatch. Original=%s Confirming=%s",
			session.ip,
			ip,
		)

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A token can only be confirmed once.
	if _, deleted := sessions.LoadAndDelete(token); !deleted {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	mu.Lock()

	// Safety check in case the limit was reached.
	if validClicks >= validClickLimit {
		reservedClicks--

		log.Printf(
			"[LIMIT] Confirmation arrived after limit. IP: %s discarded.",
			ip,
		)

		mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Confirmation succeeded.
	reservedClicks--
	validClicks++

	current := validClicks

	log.Printf(
		"[CONFIRMED] Click successfully delivered to active browser! %d/%d | reserved=%d",
		current,
		validClickLimit,
		reservedClicks,
	)

	mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func readIndexHTML() ([]byte, error) {
	return os.ReadFile("public/index.html")
}

func main() {
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/status", statusHandler)
	http.HandleFunc("/precheck", verifyHandler)
	http.HandleFunc("/verify", verifyHandler)
	http.HandleFunc("/confirm-click", confirmClickHandler)
	http.HandleFunc("/index.html", verifyHandler)

	log.Println("Server running on port 8080")

	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal(err)
	}
}



