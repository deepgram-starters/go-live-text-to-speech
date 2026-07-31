/**
 * Go Live Text-to-Speech Starter - Backend Server
 *
 * Simple WebSocket proxy to Deepgram's Live TTS API.
 * Forwards all messages (JSON and binary) bidirectionally between client and Deepgram.
 *
 * Routes:
 *   GET  /api/session                - Issue JWT session token
 *   GET  /api/metadata               - Project metadata from deepgram.toml
 *   WS   /api/live-text-to-speech    - WebSocket proxy to Deepgram TTS (auth required)
 *   GET  /health                     - Health check
 */

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	speakmsg "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/speak/v1/websocket/interfaces"
	dginterfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	speak "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/speak"

	"github.com/BurntSushi/toml"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// ============================================================================
// CONFIGURATION
// ============================================================================

// Config holds the application configuration loaded from environment variables.
type Config struct {
	DeepgramAPIKey string
	DeepgramTTSURL string
	Port           string
	Host           string
	SessionSecret  string
}

// loadConfig reads configuration from environment variables with sensible defaults.
func loadConfig() Config {
	// Load .env file (optional, won't error if missing)
	_ = godotenv.Load()

	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	if apiKey == "" {
		log.Fatal("ERROR: DEEPGRAM_API_KEY environment variable is required\nPlease copy sample.env to .env and add your API key")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}

	sessionSecret := os.Getenv("SESSION_SECRET")
	if sessionSecret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			log.Fatal("Failed to generate session secret:", err)
		}
		sessionSecret = hex.EncodeToString(b)
	}

	return Config{
		DeepgramAPIKey: apiKey,
		DeepgramTTSURL: "wss://api.deepgram.com/v1/speak",
		Port:           port,
		Host:           host,
		SessionSecret:  sessionSecret,
	}
}

// ============================================================================
// SESSION AUTH - JWT tokens for production security
// ============================================================================

const jwtExpiry = time.Hour

// generateToken creates a signed JWT for session authentication.
func generateToken(secret string) (string, error) {
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtExpiry)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// validateToken verifies a JWT and returns an error if invalid.
func validateToken(tokenString, secret string) error {
	_, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	return err
}

// validateWsToken extracts and validates a JWT from WebSocket subprotocols.
// Returns the full protocol string (e.g., "access_token.<jwt>") if valid.
func validateWsToken(protocols []string, secret string) string {
	for _, p := range protocols {
		if strings.HasPrefix(p, "access_token.") {
			tokenStr := strings.TrimPrefix(p, "access_token.")
			if err := validateToken(tokenStr, secret); err == nil {
				return p
			}
		}
	}
	return ""
}

// ============================================================================
// METADATA
// ============================================================================

// DeepgramToml represents the parsed deepgram.toml structure.
type DeepgramToml struct {
	Meta map[string]interface{} `toml:"meta"`
}

// ============================================================================
// WEBSOCKET PROXY
// ============================================================================

// upgrader configures the WebSocket upgrader. CheckOrigin allows all origins.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// activeConnections tracks all active client WebSocket connections for graceful shutdown.
var activeConnections sync.Map

// ttsCallback implements the Deepgram SDK SpeakMessageCallback interface and
// relays Live TTS events to the browser WebSocket: audio as binary frames and
// control/status messages as JSON text frames, preserving the wire format the
// frontend already expects.
type ttsCallback struct {
	conn *websocket.Conn
	mu   *sync.Mutex
	// teardown closes the browser connection (with a close frame) so a
	// Deepgram-side close/error propagates to the client and unblocks the
	// handler's read loop. Safe to call multiple times.
	teardown func(code int, reason string)
	// closing is set once an intentional shutdown is under way; while set, the
	// transport-close "error" the SDK synthesizes from Deepgram's socket close
	// is not forwarded to the browser as a spurious Error frame.
	closing *atomic.Bool
}

// sendJSON marshals a Deepgram response and writes it as a text frame.
func (c *ttsCallback) sendJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("Failed to marshal TTS event: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Failed to forward TTS event to client: %v", err)
	}
}

func (c *ttsCallback) Open(or *speakmsg.OpenResponse) error { return nil }
func (c *ttsCallback) Metadata(md *speakmsg.MetadataResponse) error {
	c.sendJSON(md)
	return nil
}
func (c *ttsCallback) Binary(byMsg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.WriteMessage(websocket.BinaryMessage, byMsg); err != nil {
		log.Printf("Failed to forward TTS audio to client: %v", err)
	}
	return nil
}
func (c *ttsCallback) Flush(fl *speakmsg.FlushedResponse) error {
	c.sendJSON(fl)
	return nil
}
func (c *ttsCallback) Clear(cl *speakmsg.ClearedResponse) error {
	c.sendJSON(cl)
	return nil
}
func (c *ttsCallback) Close(cr *speakmsg.CloseResponse) error {
	// Deepgram closed the connection; close the browser session so the read
	// loop returns instead of blocking until the browser happens to disconnect.
	c.teardown(websocket.CloseNormalClosure, "")
	return nil
}
func (c *ttsCallback) Warning(wr *speakmsg.WarningResponse) error {
	c.sendJSON(wr)
	return nil
}
func (c *ttsCallback) Error(er *speakmsg.ErrorResponse) error {
	// During an intentional shutdown the SDK reports Deepgram's socket close as
	// an error; don't forward that as a data frame. A real mid-session error
	// (closing not set) is still surfaced, then the session is torn down.
	if c.closing.Load() {
		return nil
	}
	c.sendJSON(er)
	c.teardown(websocket.CloseInternalServerErr, "Deepgram error")
	return nil
}
func (c *ttsCallback) UnhandledEvent(byData []byte) error { return nil }

// handleLiveTTSProxy proxies WebSocket messages between the client and Deepgram's Live TTS API.
func handleLiveTTSProxy(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Validate JWT from subprotocol
		protocols := websocket.Subprotocols(r)
		validProto := validateWsToken(protocols, cfg.SessionSecret)
		if validProto == "" {
			log.Println("WebSocket auth failed: invalid or missing token")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Upgrade with the accepted subprotocol
		responseHeader := http.Header{}
		responseHeader.Set("Sec-WebSocket-Protocol", validProto)
		clientConn, err := upgrader.Upgrade(w, r, responseHeader)
		if err != nil {
			log.Printf("WebSocket upgrade failed: %v", err)
			return
		}
		defer clientConn.Close()

		log.Println("Client connected to /api/live-text-to-speech")
		activeConnections.Store(clientConn, true)
		defer activeConnections.Delete(clientConn)

		// Parse query parameters from the WebSocket URL
		query := r.URL.Query()
		model := query.Get("model")
		if model == "" {
			model = "aura-asteria-en"
		}
		encoding := query.Get("encoding")
		if encoding == "" {
			encoding = "linear16"
		}
		sampleRate := query.Get("sample_rate")
		if sampleRate == "" {
			sampleRate = "24000"
		}
		// Build Deepgram Live TTS options from the forwarded query params.
		tOptions := &dginterfaces.WSSpeakOptions{
			Model:    model,
			Encoding: encoding,
		}
		if sr, err := strconv.Atoi(sampleRate); err == nil {
			tOptions.SampleRate = sr
		}
		// The reference frontend sends container=none (Deepgram's default), which
		// this path preserves. WSSpeakOptions does not model `container`, so a
		// non-default container request cannot be forwarded through the typed
		// SDK options — surface that instead of dropping it silently.
		if container := query.Get("container"); container != "" && container != "none" {
			log.Printf("Warning: container=%q requested but the SDK's WSSpeakOptions does not support a container field; using Deepgram's default (none)", container)
		}

		log.Printf("Connecting to Deepgram TTS: model=%s, encoding=%s, sample_rate=%s", model, encoding, sampleRate)

		// Serialize all writes to the browser connection (callback + close frames).
		writeMu := &sync.Mutex{}
		closeToClient := func(code int, msg string) {
			writeMu.Lock()
			defer writeMu.Unlock()
			clientConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg))
		}

		// teardown tears down the browser session exactly once: send a close
		// frame, close the connection (which unblocks clientConn.ReadMessage in
		// the pump below), and signal `done`. Invoked by the SDK callback on a
		// Deepgram close/error, or after a client Close is drained.
		done := make(chan struct{})
		closing := &atomic.Bool{}
		var closeOnce sync.Once
		teardown := func(code int, reason string) {
			closeOnce.Do(func() {
				closing.Store(true)
				closeToClient(code, reason)
				clientConn.Close()
				close(done)
			})
		}

		// Connect to Deepgram Live TTS using the official Go SDK (speak WebSocket).
		cOptions := &dginterfaces.ClientOptions{}
		callback := &ttsCallback{conn: clientConn, mu: writeMu, teardown: teardown, closing: closing}

		dgClient, err := speak.NewWSUsingCallback(context.Background(), cfg.DeepgramAPIKey, cOptions, tOptions, callback)
		if err != nil {
			log.Printf("Failed to create Deepgram TTS client: %v", err)
			closeToClient(websocket.CloseInternalServerErr, "Deepgram connection failed")
			return
		}

		if !dgClient.Connect() {
			log.Printf("Deepgram TTS connection failed")
			closeToClient(websocket.CloseInternalServerErr, "Deepgram connection failed")
			return
		}
		defer dgClient.Stop()

		log.Println("Connected to Deepgram TTS API")

		// Pump control messages (Speak / Flush) from the browser to Deepgram.
		for {
			msgType, data, err := clientConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					log.Printf("Client read error: %v", err)
				} else {
					log.Println("Client disconnected")
				}
				break
			}
			if msgType != websocket.TextMessage {
				continue
			}

			var ctrl struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(data, &ctrl); err != nil {
				log.Printf("Ignoring non-JSON control message: %v", err)
				continue
			}

			switch ctrl.Type {
			case "Speak":
				if err := dgClient.SpeakWithText(ctrl.Text); err != nil {
					log.Printf("SpeakWithText failed: %v", err)
				}
			case "Flush":
				if err := dgClient.Flush(); err != nil {
					log.Printf("Flush failed: %v", err)
				}
			case "Close", "CloseStream":
				// Let Deepgram flush any in-flight audio before closing: Stop()
				// sends the close control message and waits for the server to
				// finish, during which the final audio frames are delivered to
				// the browser via the Binary callback. The Close callback then
				// tears down; a bounded wait guards against a missing close.
				closing.Store(true)
				go dgClient.Stop()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					log.Println("Timed out waiting for Deepgram to finalize after Close")
					teardown(websocket.CloseNormalClosure, "")
				}
				return
			}
		}

		log.Println("WebSocket proxy session ended")
	}
}

// ============================================================================
// HTTP HANDLERS
// ============================================================================

// handleSession issues a signed JWT for session authentication.
func handleSession(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := generateToken(cfg.SessionSecret)
		if err != nil {
			http.Error(w, `{"error":"INTERNAL_SERVER_ERROR","message":"Failed to generate token"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

// handleHealth returns a simple health check response.
// GET /health
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleMetadata returns project metadata from deepgram.toml.
func handleMetadata(w http.ResponseWriter, r *http.Request) {
	var cfg DeepgramToml
	if _, err := toml.DecodeFile("deepgram.toml", &cfg); err != nil {
		log.Printf("Error reading deepgram.toml: %v", err)
		http.Error(w, `{"error":"INTERNAL_SERVER_ERROR","message":"Failed to read metadata from deepgram.toml"}`, http.StatusInternalServerError)
		return
	}
	if cfg.Meta == nil {
		http.Error(w, `{"error":"INTERNAL_SERVER_ERROR","message":"Missing [meta] section in deepgram.toml"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg.Meta)
}

// corsMiddleware adds CORS headers to all responses.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// MAIN
// ============================================================================

func main() {
	cfg := loadConfig()

	// Initialize the Deepgram Go SDK.
	speak.InitWithDefault()

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/session", handleSession(cfg))
	mux.HandleFunc("GET /api/metadata", handleMetadata)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("/api/live-text-to-speech", handleLiveTTSProxy(cfg))

	// Wrap with CORS middleware
	handler := corsMiddleware(mux)

	server := &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: handler,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigChan
		log.Printf("\n%s signal received: starting graceful shutdown...", sig)

		// Close all active WebSocket connections
		count := 0
		activeConnections.Range(func(key, value interface{}) bool {
			conn := key.(*websocket.Conn)
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "Server shutting down"))
			conn.Close()
			count++
			return true
		})
		log.Printf("Closed %d active WebSocket connection(s)", count)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
		log.Println("Shutdown complete")
	}()

	log.Println(strings.Repeat("=", 70))
	log.Printf("Backend API Server running at http://localhost:%s", cfg.Port)
	log.Println("")
	log.Println("GET  /api/session")
	log.Println("WS   /api/live-text-to-speech (auth required)")
	log.Println("GET  /api/metadata")
	log.Println("GET  /health")
	log.Println(strings.Repeat("=", 70))

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}
}
