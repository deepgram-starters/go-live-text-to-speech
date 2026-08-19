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

	// The Deepgram endpoint is not configured here: the SDK derives it from
	// ClientOptions (override the host with cOptions.Host if you need to).
	return Config{
		DeepgramAPIKey: apiKey,
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

// activeConnections maps each active client WebSocket connection to its
// teardown function, so graceful shutdown can close it through the same
// serialized, deadline-bounded write path the proxy itself uses.
var activeConnections sync.Map

// browserWriteTimeout bounds every write to the browser socket. A stalled
// browser (backgrounded tab, full TCP receive window) must not block the
// Deepgram client: the SDK fires its Close callback while holding its
// connection mutex, so an unbounded write from that callback would deadlock
// the SDK's read loop, every write to Deepgram, and the deferred Stop().
const browserWriteTimeout = 5 * time.Second

// deepgramDrainTimeout is a last-resort ceiling on the Close drain. The normal
// path ends when Deepgram closes its own socket, however long that takes.
const deepgramDrainTimeout = 30 * time.Second

// controlMessage is a bare Deepgram control frame. The SDK models this type
// internally but does not export it, so control messages it has no typed method
// for (Clear) are written through the raw WSClient.WriteJSON below.
type controlMessage struct {
	Type string `json:"type"`
}

// browserConn wraps the client WebSocket. Both the SDK's callback goroutine and
// the handler write to it, and gorilla panics on concurrent writes, so every
// write must go through these methods.
type browserConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// write serializes a single frame to the browser under a write deadline.
func (b *browserConn) write(msgType int, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.conn.SetWriteDeadline(time.Now().Add(browserWriteTimeout)); err != nil {
		return err
	}
	return b.conn.WriteMessage(msgType, data)
}

// sendJSON marshals a value and writes it to the browser as a text frame.
func (b *browserConn) sendJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("Failed to marshal TTS event: %v", err)
		return
	}
	if err := b.write(websocket.TextMessage, data); err != nil {
		log.Printf("Failed to forward TTS event to client: %v", err)
	}
}

// sendError emits the nested Error frame the live TTS contract requires:
// {"type":"Error","error":{"type","code","message"}} — see
// contracts/interfaces/live-text-to-speech/schema/error.json. `code` must be
// one of the contract's enum values.
func (b *browserConn) sendError(errType, code, message string) {
	b.sendJSON(map[string]any{
		"type": "Error",
		"error": map[string]any{
			"type":    errType,
			"code":    code,
			"message": message,
		},
	})
}

// close sends a close frame and closes the socket.
func (b *browserConn) close(code int, reason string) {
	if err := b.write(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason)); err != nil {
		log.Printf("Failed to send close frame to client: %v", err)
	}
	b.conn.Close()
}

// ttsCallback implements the Deepgram SDK SpeakMessageCallback interface and
// relays Live TTS events to the browser WebSocket: audio as binary frames and
// control/status messages as JSON text frames, preserving the wire format the
// frontend already expects.
type ttsCallback struct {
	client *browserConn
	// teardown closes the browser connection (with a close frame) so a
	// Deepgram-side close/error propagates to the client and unblocks the
	// handler's read loop. Safe to call multiple times.
	teardown func(code int, reason string)
	// closing is set once an intentional shutdown is under way; while set, the
	// transport-close "error" the SDK synthesizes from Deepgram's socket close
	// is not forwarded to the browser as a spurious Error frame.
	closing *atomic.Bool
}

func (c *ttsCallback) Open(or *speakmsg.OpenResponse) error { return nil }
func (c *ttsCallback) Metadata(md *speakmsg.MetadataResponse) error {
	// Frames are hand-built rather than re-marshaled from the SDK struct: every
	// field on these types is tagged `omitempty`, so a direct marshal silently
	// drops contract-required keys whose value happens to be the zero value.
	//
	// KNOWN GAP: the contract's MetadataEvent also requires model_name,
	// model_version and model_uuid, and Deepgram does send all three, but the
	// SDK's MetadataResponse models only type and request_id — the rest are
	// discarded during unmarshal, before this callback runs, so they cannot be
	// recovered here. Closing it needs those fields on the SDK struct.
	c.client.sendJSON(map[string]any{
		"type":       "Metadata",
		"request_id": md.RequestID,
	})
	return nil
}
func (c *ttsCallback) Binary(byMsg []byte) error {
	if err := c.client.write(websocket.BinaryMessage, byMsg); err != nil {
		log.Printf("Failed to forward TTS audio to client: %v", err)
	}
	return nil
}
func (c *ttsCallback) Flush(fl *speakmsg.FlushedResponse) error {
	// sequence_id is contract-required and Deepgram's first ack in a session is
	// 0, which `omitempty` on the SDK struct would drop.
	c.client.sendJSON(map[string]any{
		"type":        "Flushed",
		"sequence_id": fl.SequenceID,
	})
	return nil
}
func (c *ttsCallback) Clear(cl *speakmsg.ClearedResponse) error {
	c.client.sendJSON(map[string]any{
		"type":        "Cleared",
		"sequence_id": cl.SequenceID,
	})
	return nil
}
func (c *ttsCallback) Close(cr *speakmsg.CloseResponse) error {
	// Deepgram closed the connection; close the browser session so the read
	// loop returns instead of blocking until the browser happens to disconnect.
	c.teardown(websocket.CloseNormalClosure, "")
	return nil
}
func (c *ttsCallback) Warning(wr *speakmsg.WarningResponse) error {
	// Deepgram's Warning frame is {type, description, code}, but the SDK's
	// WarningResponse (an alias for DeepgramWarning) has no json:"type" tag and
	// tags its code field json:"warn_code", which never matches the wire key —
	// so the code is already gone by the time this callback runs and cannot be
	// forwarded. Closing that gap needs a json:"code" field on the SDK struct.
	c.client.sendJSON(map[string]any{
		"type":        "Warning",
		"description": wr.Description,
	})
	return nil
}
func (c *ttsCallback) Error(er *speakmsg.ErrorResponse) error {
	// During an intentional shutdown the SDK reports Deepgram's socket close as
	// an error; don't forward that as a data frame. A real mid-session error
	// (closing not set) is still surfaced, then the session is torn down.
	if c.closing.Load() {
		return nil
	}
	// Deepgram's streaming /v1/speak API has no Error event, so everything that
	// reaches this callback is a transport failure the SDK synthesized from a
	// socket error — which is what CONNECTION_FAILED means in the contract's
	// error enum. The upstream code is unavailable either way: ErrorResponse
	// aliases DeepgramError, whose code field is tagged json:"err_code".
	message := er.Description
	if message == "" {
		message = er.ErrMsg
	}
	if message == "" {
		message = "Deepgram connection error"
	}
	c.client.sendError("connection_error", "CONNECTION_FAILED", message)
	c.teardown(websocket.CloseInternalServerErr, "Deepgram error")
	return nil
}
func (c *ttsCallback) UnhandledEvent(byData []byte) error {
	// Stay transparent, as the pre-SDK byte-relay proxy was: message types the
	// SDK does not model reach this callback with their original bytes, so
	// forward them verbatim instead of swallowing them.
	if err := c.client.write(websocket.TextMessage, byData); err != nil {
		log.Printf("Failed to forward unmodeled TTS event to client: %v", err)
	}
	return nil
}

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

		// Every write to the browser goes through this wrapper.
		client := &browserConn{conn: clientConn}

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
		// Reject an unparseable sample_rate instead of leaving SampleRate at 0:
		// omitting it makes Deepgram apply its 24 kHz default while the frontend
		// keeps decoding at the rate it asked for, which plays back at the wrong
		// speed with no error anywhere.
		sr, err := strconv.Atoi(sampleRate)
		if err != nil {
			log.Printf("Rejecting connection: invalid sample_rate=%q", sampleRate)
			client.sendError("invalid_request", "CONNECTION_FAILED",
				fmt.Sprintf("invalid sample_rate %q: expected an integer", sampleRate))
			client.close(websocket.CloseUnsupportedData, "invalid sample_rate")
			return
		}
		// Build Deepgram Live TTS options from the forwarded query params.
		tOptions := &dginterfaces.WSSpeakOptions{
			Model:      model,
			Encoding:   encoding,
			SampleRate: sr,
		}
		// The reference frontend sends container=none, the only meaningful value
		// here: Deepgram's streaming /v1/speak API has no container concept, so
		// it always returns bare audio. Surface a non-default request instead of
		// dropping it silently.
		if container := query.Get("container"); container != "" && container != "none" {
			log.Printf("Warning: ignoring container=%q — Deepgram's streaming /v1/speak API has no container parameter and always returns raw %s audio", container, encoding)
		}

		log.Printf("Connecting to Deepgram TTS: model=%s, encoding=%s, sample_rate=%d", model, encoding, sr)

		// teardown tears down the browser session exactly once: send a close
		// frame, close the connection (which unblocks clientConn.ReadMessage in
		// the pump below), and signal `done`. Invoked by the SDK callback on a
		// Deepgram close/error, by graceful shutdown, or after a client Close is
		// drained.
		done := make(chan struct{})
		closing := &atomic.Bool{}
		var closeOnce sync.Once
		teardown := func(code int, reason string) {
			closeOnce.Do(func() {
				closing.Store(true)
				client.close(code, reason)
				close(done)
			})
		}

		// Register the teardown, not the raw connection: graceful shutdown must
		// not write to this socket without holding the write mutex.
		activeConnections.Store(clientConn, teardown)
		defer activeConnections.Delete(clientConn)

		// Connect to Deepgram Live TTS using the official Go SDK (speak WebSocket).
		dgCtx, dgCancel := context.WithCancel(context.Background())
		defer dgCancel()
		cOptions := &dginterfaces.ClientOptions{}
		callback := &ttsCallback{client: client, teardown: teardown, closing: closing}

		dgClient, err := speak.NewWSUsingCallbackWithCancel(dgCtx, dgCancel, cfg.DeepgramAPIKey, cOptions, tOptions, callback)
		if err != nil {
			log.Printf("Failed to create Deepgram TTS client: %v", err)
			client.sendError("connection_error", "CONNECTION_FAILED", "Deepgram connection failed")
			client.close(websocket.CloseInternalServerErr, "Deepgram connection failed")
			return
		}

		// One attempt, not the SDK's default of three: the usual causes here (a
		// bad API key, an unknown model) are not retryable, and the SDK's 2s
		// backoff would stall the browser for ~4s before reporting the same
		// failure. The upstream HTTP status is only logged by the SDK itself, at
		// LogLevelElevated — see the Init call in main().
		if !dgClient.ConnectWithCancel(dgCtx, dgCancel, 1) {
			log.Printf("Deepgram TTS connection failed: check DEEPGRAM_API_KEY and the model=%s / encoding=%s / sample_rate=%d combination", model, encoding, sr)
			client.sendError("connection_error", "CONNECTION_FAILED", "Deepgram connection failed")
			client.close(websocket.CloseInternalServerErr, "Deepgram connection failed")
			return
		}
		defer dgClient.Stop()

		log.Println("Connected to Deepgram TTS API")

		// Pump control messages (Speak / Flush / Clear / Close) from the browser
		// to Deepgram.
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
			case "Clear":
				// Forwarded verbatim rather than via the SDK's Reset(), which
				// emits {"type":"Reset"} — a message the Live TTS API does not
				// model, so barge-in would never cancel the queued audio.
				if err := dgClient.WSClient.WriteJSON(controlMessage{Type: "Clear"}); err != nil {
					log.Printf("Clear failed: %v", err)
				}
			case "Close", "CloseStream":
				// Deepgram's Close drains the text buffer, delivers the
				// remaining audio and then closes the socket, so the drain ends
				// when that close arrives (SDK Close callback -> teardown ->
				// done) — not on a local timer. dgClient.Stop() cannot be used
				// here: it closes the socket itself after two 100ms sleeps,
				// truncating anything still in flight.
				if err := dgClient.WSClient.WriteJSON(controlMessage{Type: "Close"}); err != nil {
					log.Printf("Close failed: %v", err)
					teardown(websocket.CloseInternalServerErr, "Deepgram close failed")
					return
				}
				select {
				case <-done:
				case <-time.After(deepgramDrainTimeout):
					log.Println("Timed out waiting for Deepgram to finalize after Close")
					teardown(websocket.CloseNormalClosure, "")
				}
				return
			default:
				// Stay transparent: forward control messages this proxy does not
				// model straight through rather than discarding them.
				log.Printf("Forwarding unrecognized control message type %q verbatim", ctrl.Type)
				if err := dgClient.WSClient.WriteJSON(json.RawMessage(data)); err != nil {
					log.Printf("Forwarding %q failed: %v", ctrl.Type, err)
				}
			}
		}

		// The browser is gone, so the close frames the deferred Stop() triggers
		// are not worth reporting to it as Error frames.
		closing.Store(true)
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

	// Initialize the Deepgram Go SDK. Raise this to speak.LogLevelElevated to see
	// the SDK log the upstream HTTP status when a connection is rejected (a 401
	// for a bad API key, for example); it is chatty, so the default stays here.
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

		// Close all active WebSocket connections. Each connection's teardown is
		// used rather than a direct WriteMessage: writing here without the
		// connection's write mutex races the SDK's audio callback, and gorilla's
		// concurrent-write detection would panic this goroutine (crashing the
		// process) instead of shutting down cleanly.
		count := 0
		activeConnections.Range(func(key, value interface{}) bool {
			teardown, ok := value.(func(code int, reason string))
			if !ok {
				return true
			}
			teardown(websocket.CloseGoingAway, "Server shutting down")
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
