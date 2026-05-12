package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
)

// maxBodyBytes caps the size of incoming webhook payloads to protect against
// unauthenticated callers attempting to exhaust memory before the HMAC check.
const maxBodyBytes = 1 << 20 // 1 MiB

// server encapsulates all the dependencies required for handling HTTP requests.
type server struct {
	pubsubClient *pubsub.Client
	publisher    *pubsub.Publisher
	xSystemKey   []byte
	projectID    string
}

// newServer initializes a new server instance with the necessary configurations.
func newServer(ctx context.Context) (*server, error) {
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		return nil, errors.New("PROJECT_ID environment variable is not set")
	}
	topicID := os.Getenv("TOPIC")
	if topicID == "" {
		return nil, errors.New("TOPIC environment variable is not set")
	}
	xSystemKeyEnv := os.Getenv("X_SYSTEM_KEY")
	if xSystemKeyEnv == "" {
		return nil, errors.New("X_SYSTEM_KEY environment variable is not set")
	}

	pubsubClient, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pub/Sub client: %w", err)
	}

	return &server{
		pubsubClient: pubsubClient,
		publisher:    pubsubClient.Publisher(topicID),
		xSystemKey:   []byte(xSystemKeyEnv),
		projectID:    projectID,
	}, nil
}

// requestLogger returns a logger with the Cloud Logging trace field set if
// Cloud Run supplied an X-Cloud-Trace-Context header. Cloud Logging uses that
// field to group all log entries that share a trace ID into a single request
// view in the Logs Explorer.
func (s *server) requestLogger(r *http.Request) *slog.Logger {
	header := r.Header.Get("X-Cloud-Trace-Context")
	if header == "" {
		return slog.Default()
	}
	// Header format is "TRACE_ID/SPAN_ID;o=1"; we only need TRACE_ID.
	traceID := header
	if i := strings.IndexByte(traceID, '/'); i != -1 {
		traceID = traceID[:i]
	}
	if traceID == "" {
		return slog.Default()
	}
	return slog.Default().With(
		"logging.googleapis.com/trace",
		fmt.Sprintf("projects/%s/traces/%s", s.projectID, traceID),
	)
}

// validateSignature verifies the HMAC SHA256 signature of the incoming request body.
// Uses hmac.Equal for constant-time comparison to avoid leaking timing information.
func (s *server) validateSignature(body []byte, signature string) bool {
	encoded := base64.StdEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.xSystemKey)
	mac.Write([]byte(encoded))
	calculated := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(calculated))
}

// writeJSONError writes a JSON-formatted error response with the given status
// code. Used instead of http.Error so the Content-Type matches the body shape.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}

// fubReceiver handles incoming webhooks from FUB and publishes the data to Pub/Sub.
func (s *server) fubReceiver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := s.requestLogger(r)
	query := r.URL.RawQuery

	signature := r.Header.Get("FUB-Signature")
	if signature == "" {
		log.Warn("missing FUB-Signature header", "query", query)
		writeJSONError(w, http.StatusBadRequest, "Missing FUB-Signature header")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			log.Warn("request body too large", "query", query, "limit", maxBodyBytes)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		log.Error("failed to read request body", "error", err, "query", query)
		writeJSONError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if !s.validateSignature(bodyBytes, signature) {
		// Intentionally omit the body from logs; anyone can POST garbage here.
		log.Warn("signature mismatch", "query", query, "body_len", len(bodyBytes))
		writeJSONError(w, http.StatusBadRequest, "Invalid FUB-Signature header")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		log.Error("failed to unmarshal request body", "error", err, "query", query)
		writeJSONError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	eventAttribute, _ := payload["event"].(string)
	wrappedData := wrapWebhookData(r.URL.Query(), payload)

	if err := s.publishToPubSub(ctx, wrappedData, eventAttribute); err != nil {
		log.Error("failed to publish message", "error", err, "event", eventAttribute, "query", query)
		writeJSONError(w, http.StatusInternalServerError, "Internal error")
		return
	}

	log.Info("webhook processed", "event", eventAttribute, "body_len", len(bodyBytes))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

// wrapWebhookData processes the request data and query arguments into a structured format.
func wrapWebhookData(query url.Values, webhookData map[string]any) map[string]any {
	webhookArgs := make(map[string]string, len(query))
	for key, values := range query {
		if len(values) > 0 {
			webhookArgs[key] = values[0]
		}
	}
	return map[string]any{
		"webhookData": webhookData,
		"webhookArgs": webhookArgs,
	}
}

// publishToPubSub publishes the data to Pub/Sub and waits for the result. The
// publish context is detached from the caller's request context via
// context.WithoutCancel: if FUB disconnects mid-handler, pubsub's background
// goroutines may still deliver the message, and we don't want to return an
// error (causing a FUB retry -> duplicate) for a publish that actually
// succeeded. A hard timeout still bounds how long we'll wait.
func (s *server) publishToPubSub(ctx context.Context, wrappedData map[string]any, eventAttribute string) error {
	wrappedBytes, err := json.Marshal(wrappedData)
	if err != nil {
		return fmt.Errorf("failed to marshal wrapped data: %w", err)
	}

	msg := &pubsub.Message{Data: wrappedBytes}
	if eventAttribute != "" {
		msg.Attributes = map[string]string{"event": eventAttribute}
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if _, err := s.publisher.Publish(pubCtx, msg).Get(pubCtx); err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// cloudLoggingAttrs renames slog's default JSON keys to the ones Google Cloud
// Logging recognizes, so severity filtering and message rendering work in the
// Logs Explorer. slog's "WARN" is also mapped to Cloud Logging's "WARNING".
func cloudLoggingAttrs(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.LevelKey:
		level := a.Value.String()
		if level == "WARN" {
			level = "WARNING"
		}
		return slog.Attr{Key: "severity", Value: slog.StringValue(level)}
	case slog.MessageKey:
		return slog.Attr{Key: "message", Value: a.Value}
	case slog.TimeKey:
		return slog.Attr{Key: "timestamp", Value: a.Value}
	}
	return a
}

// run wires up the server, starts the HTTP listener, and blocks until a
// shutdown signal is received or the listener fails.
func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: cloudLoggingAttrs,
	})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := newServer(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize server: %w", err)
	}
	defer func() { _ = srv.pubsubClient.Close() }()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", srv.fubReceiver)

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("starting server", "port", port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("server error: %w", err)
		}
	}

	// Cloud Run gives ~10s after SIGTERM before SIGKILL; match that budget.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}
	srv.publisher.Stop()
	return nil
}
