```go
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	validationTime     = 1 * time.Second
	validClickLimit    = 10
	maxConcurrentWaits = 50
)

type Token struct {
	Cancelled bool
}

var (
	tokens = make(map[string]*Token)
	mu     sync.Mutex

	validClicks int

	waitSemaphore = make(chan struct{}, maxConcurrentWaits)

	secretKey = []byte("change-this-to-a-long-random-secret")
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type validationResponse struct {
	Status    string `json:"status"`
	Signature string `json:"signature"`
}

func createToken() string {
	b := make([]byte, 32)

	_, _ = rand.Read(b)

	return base64.RawURLEncoding.EncodeToString(b)
}

func createSignature(token string) string {
	hash := sha256.Sum256(
		append(secretKey, []byte(token)...),
	)

	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func verifySignature(token string, signature string) bool {
	expected := createSignature(token)

	if len(expected) != len(signature) {
		return false
	}

	return subtle.ConstantTimeCompare(
		[]byte(expected),
		[]byte(signature),
	) == 1
}

func precheck(w http.ResponseWriter, r *http.Request) {
	token := createToken()

	mu.Lock()
	tokens[token] = &Token{}
	mu.Unlock()

	w.Header().Set("Content-Type", "text/html")

	fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head></head>
<body>

<script>

const token = "%s";

let validationComplete = false;

const wsProtocol =
	window.location.protocol === "https:"
		? "wss://"
		: "ws://";

const ws = new WebSocket(
	wsProtocol +
	window.location.host +
	"/ws?token=" +
	encodeURIComponent(token)
);

ws.onmessage = function(event) {

	const data = JSON.parse(event.data);

	if (data.status === "valid") {

		validationComplete = true;

		window.location =
			"/?token=" +
			encodeURIComponent(token) +
			"&signature=" +
			encodeURIComponent(data.signature);
	}
};

</script>

</body>
</html>
`, token)
}

func cancelHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	if token == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	mu.Lock()

	if t, exists := tokens[token]; exists {
		t.Cancelled = true
	}

	mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func wsHandler(w http.ResponseWriter, r *http.Request) {

	select {

	case waitSemaphore <- struct{}{}:

		defer func() {
			<-waitSemaphore
		}()

	default:

		w.WriteHeader(http.StatusNoContent)
		return
	}

	token := r.URL.Query().Get("token")

	if token == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	mu.Lock()

	_, exists := tokens[token]

	mu.Unlock()

	if !exists {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		return
	}

	defer conn.Close()

	disconnected := make(chan struct{})

	go func() {

		defer close(disconnected)

		for {

			_, _, err := conn.ReadMessage()

			if err != nil {
				return
			}
		}

	}()

	timer := time.NewTimer(validationTime)

	defer timer.Stop()

	select {

	case <-disconnected:

		// WebSocket closed before validation completed.
		// Mark this token cancelled immediately.

		mu.Lock()

		if t, exists := tokens[token]; exists {
			t.Cancelled = true
		}

		mu.Unlock()

		return

	case <-timer.C:

		mu.Lock()

		t, exists := tokens[token]

		if !exists || t.Cancelled {
			mu.Unlock()
			return
		}

		if validClicks >= validClickLimit {
			mu.Unlock()
			return
		}

		validClicks++

		mu.Unlock()

		signature := createSignature(token)

		response := validationResponse{
			Status:    "valid",
			Signature: signature,
		}

		message, err := json.Marshal(response)

		if err != nil {
			return
		}

		_ = conn.WriteMessage(
			websocket.TextMessage,
			message,
		)
	}
}

func landing(w http.ResponseWriter, r *http.Request) {

	token := r.URL.Query().Get("token")
	signature := r.URL.Query().Get("signature")

	if token == "" ||
		signature == "" ||
		!verifySignature(token, signature) {

		w.WriteHeader(http.StatusNoContent)
		return
	}

	mu.Lock()

	t, exists := tokens[token]

	valid := exists && !t.Cancelled

	mu.Unlock()

	if !valid {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/html")

	fmt.Fprint(w, `
<!DOCTYPE html>
<html>
<body>

<h1>Landing Page</h1>

</body>
</html>
`)
}

func main() {

	http.HandleFunc(
		"/precheck",
		precheck,
	)

	http.HandleFunc(
		"/ws",
		wsHandler,
	)

	http.HandleFunc(
		"/cancel",
		cancelHandler,
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
		panic(err)
	}
}
```




