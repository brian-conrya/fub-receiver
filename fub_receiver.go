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
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
)

// maxBodyBytes caps the size of incoming webhook payloads to protect against
// unauthenticated callers attempting to exhaust memory before the HMAC check.
const maxBodyBytes = 1 << 20 // 1 MiB

// messagePublisher is the slice of *pubsub.Publisher this service uses.
//
// ResumePublish is part of the contract rather than an implementation detail:
// with ordered publishing a failed key latches and rejects every subsequent
// message until it is resumed, so any publisher used here has to offer a way
// to clear that state.
type messagePublisher interface {
	Publish(ctx context.Context, msg *pubsub.Message) *pubsub.PublishResult
	ResumePublish(orderingKey string)
	Stop()
}

// server encapsulates all the dependencies required for handling HTTP requests.
type server struct {
	pubsubClient *pubsub.Client
	publisher    messagePublisher
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

	publisher := pubsubClient.Publisher(topicID)
	// Required before publishing anything with an OrderingKey; Publish rejects
	// keyed messages otherwise. Messages published without a key are unaffected
	// and still travel unordered.
	publisher.EnableMessageOrdering = true

	return &server{
		pubsubClient: pubsubClient,
		publisher:    publisher,
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
	account := r.URL.Query().Get("account")
	if account == "" {
		// Not fatal here, but worth surfacing: subscribers key their work on it,
		// and an ordering key without one would serialise unrelated accounts
		// that happen to share a person id. In practice it means the webhook
		// was registered in FUB without ?account=.
		log.Warn("webhook has no account query parameter", "query", query)
	}

	// Fan out to one message per resource. Subscribers can then be given an
	// ordering key per person, which is what lets them process a single
	// person's events strictly in sequence while still running many people
	// concurrently. Events that name no resource publish once, unkeyed.
	ids := resourceIDs(payload)
	messages := make([]*pubsub.Message, 0, max(len(ids), 1))

	if len(ids) == 0 {
		wrapped, err := marshalWrapped(r.URL.Query(), payload)
		if err != nil {
			log.Error("failed to marshal payload", "error", err, "query", query)
			writeJSONError(w, http.StatusInternalServerError, "Internal error")
			return
		}
		messages = append(messages, newMessage(wrapped, eventAttribute, ""))
	} else {
		for _, id := range ids {
			wrapped, err := marshalWrapped(r.URL.Query(), withSingleResourceID(payload, id))
			if err != nil {
				log.Error("failed to marshal payload", "error", err, "resource_id", id, "query", query)
				writeJSONError(w, http.StatusInternalServerError, "Internal error")
				return
			}
			// account:personID -- person ids are only unique within an account,
			// so keying on the id alone would serialise unrelated accounts
			// against each other.
			messages = append(messages, newMessage(wrapped, eventAttribute, account+":"+id))
		}
	}

	if err := s.publishAll(ctx, messages); err != nil {
		log.Error("failed to publish message", "error", err, "event", eventAttribute, "query", query)
		writeJSONError(w, http.StatusInternalServerError, "Internal error")
		return
	}

	log.Info("webhook processed", "event", eventAttribute, "messages", len(messages), "body_len", len(bodyBytes))

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

// resourceIDs extracts webhookData.resourceIds as strings.
//
// encoding/json decodes numbers into float64, so these are formatted back to
// integers rather than stringified directly -- otherwise the ordering key would
// read "123" on one path and "1.23e+02" on another and stop matching.
func resourceIDs(payload map[string]any) []string {
	raw, ok := payload["resourceIds"].([]any)
	if !ok {
		return nil
	}

	ids := make([]string, 0, len(raw))
	for _, value := range raw {
		switch v := value.(type) {
		case float64:
			if v != math.Trunc(v) {
				continue // not an id
			}
			ids = append(ids, strconv.FormatInt(int64(v), 10))
		case string:
			if v != "" {
				ids = append(ids, v)
			}
		}
	}
	return ids
}

// withSingleResourceID copies the payload with resourceIds narrowed to one id.
// The copy matters: mutating the shared payload in a loop would leave every
// published message carrying whichever id happened to be written last.
func withSingleResourceID(payload map[string]any, id string) map[string]any {
	single := make(map[string]any, len(payload))
	for k, v := range payload {
		single[k] = v
	}
	single["resourceIds"] = []any{id}
	return single
}

func marshalWrapped(query url.Values, payload map[string]any) ([]byte, error) {
	wrapped, err := json.Marshal(wrapWebhookData(query, payload))
	if err != nil {
		return nil, fmt.Errorf("failed to marshal wrapped data: %w", err)
	}
	return wrapped, nil
}

// publishTimeout scales the publish budget with the size of the fan-out.
//
// The client keeps one bundler per ordering key, so messages with distinct keys
// are never batched together: a webhook naming N people costs N Publish RPCs,
// not one. A fixed budget would therefore fail bulk webhooks (a mass stage
// change in FUB) no matter how many times FUB retried them, turning a large
// webhook into a permanently poisoned one.
//
// The ceiling stays under the server's WriteTimeout so the response is still
// written rather than cut off mid-flight.
func publishTimeout(messages int) time.Duration {
	const (
		base    = 10 * time.Second
		perItem = 15 * time.Millisecond
		ceiling = 25 * time.Second
	)
	timeout := base + time.Duration(messages)*perItem
	return min(timeout, ceiling)
}

func newMessage(data []byte, eventAttribute, orderingKey string) *pubsub.Message {
	msg := &pubsub.Message{Data: data, OrderingKey: orderingKey}
	if eventAttribute != "" {
		msg.Attributes = map[string]string{"event": eventAttribute}
	}
	return msg
}

// publishAll publishes every message and waits for all of them. The publish
// context is detached from the caller's request context via
// context.WithoutCancel: if FUB disconnects mid-handler, pubsub's background
// goroutines may still deliver the message, and we don't want to return an
// error (causing a FUB retry -> duplicate) for a publish that actually
// succeeded. A hard timeout still bounds how long we'll wait.
//
// Every message is handed to Publish before any result is awaited, so a webhook
// naming many people costs roughly one round trip rather than one per person.
func (s *server) publishAll(ctx context.Context, messages []*pubsub.Message) error {
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout(len(messages)))
	defer cancel()

	results := make([]*pubsub.PublishResult, 0, len(messages))
	for _, msg := range messages {
		results = append(results, s.publisher.Publish(pubCtx, msg))
	}

	var firstErr error
	for i, result := range results {
		if _, err := result.Get(pubCtx); err != nil {
			// Ordered publishing latches on failure: once a key errors, every
			// later publish for that key fails immediately and permanently
			// until it is resumed. Without this one transient error would wedge
			// that person's events for the lifetime of the process.
			if key := messages[i].OrderingKey; key != "" {
				s.publisher.ResumePublish(key)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to publish message: %w", err)
			}
		}
	}
	return firstErr
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
