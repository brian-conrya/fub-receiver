package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"cloud.google.com/go/pubsub"
)

// server encapsulates all the dependencies required for handling HTTP requests.
type server struct {
	pubsubClient *pubsub.Client
	topic        *pubsub.Topic
	xSystemKey   []byte
}

// newServer initializes a new server instance with the necessary configurations.
func newServer(ctx context.Context) (*server, error) {
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		return nil, fmt.Errorf("PROJECT_ID environment variable is not set")
	}
	topicID := os.Getenv("TOPIC")
	if topicID == "" {
		return nil, fmt.Errorf("TOPIC environment variable is not set")
	}
	xSystemKeyEnv := os.Getenv("X_SYSTEM_KEY")
	if xSystemKeyEnv == "" {
		return nil, fmt.Errorf("X_SYSTEM_KEY environment variable is not set")
	}
	xSystemKey := []byte(xSystemKeyEnv)

	pubsubClient, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pub/Sub client: %w", err)
	}
	topic := pubsubClient.Topic(topicID)

	return &server{
		pubsubClient: pubsubClient,
		topic:        topic,
		xSystemKey:   xSystemKey,
	}, nil
}

// validateSignature verifies the HMAC SHA256 signature of the incoming request body.
func (s *server) validateSignature(r *http.Request, body []byte, signature string) bool {
	encoded := base64.StdEncoding.EncodeToString(body)

	mac := hmac.New(sha256.New, s.xSystemKey)
	mac.Write([]byte(encoded))
	calculated := hex.EncodeToString(mac.Sum(nil))

	if signature != calculated {
		slog.Warn("signature mismatch",
			"expected", calculated,
			"found", signature,
			"query", r.URL.Query(),
			"body", body,
		)
		return false
	}
	return true
}

// fubReceiver handles incoming webhooks from FUB and publishes the data to Pub/Sub.
func (s *server) fubReceiver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	signature := r.Header.Get("FUB-Signature")
	if signature == "" {
		slog.Error("missing FUB-Signature header", "headers", r.Header, "query", r.URL.Query())
		http.Error(w, `{"message":"Missing FUB-Signature header"}`, http.StatusBadRequest)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("failed to read request body", "error", err, "headers", r.Header, "query", r.URL.Query())
		http.Error(w, `{"message":"Internal error"}`, http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	if !s.validateSignature(r, bodyBytes, signature) {
		http.Error(w, `{"message":"Invalid FUB-Signature header"}`, http.StatusBadRequest)
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		slog.Error("failed to unmarshal request body", "error", err, "query", r.URL.Query(), "body", bodyBytes)
		http.Error(w, `{"message":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	var eventAttribute string
	if event, ok := payload["event"].(string); ok {
		eventAttribute = event
	}

	wrappedData := wrapWebhookData(r, string(bodyBytes))

	if err := s.publishToPubSub(ctx, wrappedData, eventAttribute); err != nil {
		slog.Error("failed to publish message", "error", err, "wrappedData", wrappedData)
		http.Error(w, `{"message":"Internal error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

// wrapWebhookData processes the request data and query arguments into a structured format.
func wrapWebhookData(r *http.Request, webhookData string) map[string]interface{} {
	webhookArgs := make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			webhookArgs[key] = values[0]
		}
	}
	return map[string]interface{}{
		"webhookData": webhookData,
		"webhookArgs": webhookArgs,
	}
}

// publishToPubSub publishes the data to Pub/Sub and waits for the result.
func (s *server) publishToPubSub(ctx context.Context, wrappedData map[string]interface{}, eventAttribute string) error {
	wrappedBytes, err := json.Marshal(wrappedData)
	if err != nil {
		return fmt.Errorf("failed to marshal wrapped data: %w", err)
	}

	msg := &pubsub.Message{
		Data: wrappedBytes,
	}

	if eventAttribute != "" {
		msg.Attributes = map[string]string{
			"event": eventAttribute,
		}
	}

	result := s.topic.Publish(ctx, msg)

	_, err = result.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}
	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx := context.Background()

	srv, err := newServer(ctx)
	if err != nil {
		slog.Error("failed to initialize server", "error", err)
		return
	}
	defer srv.pubsubClient.Close()

	http.HandleFunc("/", srv.fubReceiver)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	slog.Info("starting server", "port", port)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		slog.Error("failed to start server", "error", err)
	}
}
