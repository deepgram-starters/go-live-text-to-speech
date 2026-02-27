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
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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
		container := query.Get("container")
		if container == "" {
			container = "none"
		}

		// Build Deepgram WebSocket URL with query parameters
		deepgramURL, _ := url.Parse(cfg.DeepgramTTSURL)
		q := deepgramURL.Query()
		q.Set("model", model)
		q.Set("encoding", encoding)
		q.Set("sample_rate", sampleRate)
		q.Set("container", container)
		deepgramURL.RawQuery = q.Encode()

		log.Printf("Connecting to Deepgram TTS: model=%s, encoding=%s, sample_rate=%s", model, encoding, sampleRate)

		// Create WebSocket connection to Deepgram
		dgHeader := http.Header{}
		dgHeader.Set("Authorization", "Token "+cfg.DeepgramAPIKey)

		deepgramConn, resp, err := websocket.DefaultDialer.Dial(deepgramURL.String(), dgHeader)
		if err != nil {
			if resp != nil {
				log.Printf("Deepgram rejected connection (%d)", resp.StatusCode)
			} else {
				log.Printf("Deepgram connection failed: %v", err)
			}
			clientConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "Deepgram connection failed"))
			return
		}
		defer deepgramConn.Close()

		log.Println("Connected to Deepgram TTS API")

		// done channel to coordinate goroutine shutdown
		done := make(chan struct{})
		var once sync.Once
		closeDone := func() { once.Do(func() { close(done) }) }

		// Forward messages: Deepgram -> Client
		go func() {
			defer closeDone()
			for {
				msgType, data, err := deepgramConn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						log.Println("Deepgram connection closed normally")
					} else {
						log.Printf("Deepgram read error: %v", err)
					}
					// Forward close to client
					clientConn.WriteMessage(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Deepgram disconnected"))
					return
				}

				if err := clientConn.WriteMessage(msgType, data); err != nil {
					log.Printf("Error forwarding to client: %v", err)
					return
				}
			}
		}()

		// Forward messages: Client -> Deepgram
		go func() {
			defer closeDone()
			for {
				msgType, data, err := clientConn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						log.Println("Client disconnected normally")
					} else {
						log.Printf("Client read error: %v", err)
					}
					// Forward close to Deepgram
					deepgramConn.WriteMessage(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client disconnected"))
					return
				}

				if err := deepgramConn.WriteMessage(msgType, data); err != nil {
					log.Printf("Error forwarding to Deepgram: %v", err)
					return
				}
			}
		}()

		// Wait for either goroutine to finish
		<-done
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
