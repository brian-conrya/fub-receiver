package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// Set up the test server and Pub/Sub client for testing.
func setupPubSubTestServer(t *testing.T, opts ...pstest.ServerReactorOption) (*pubsub.Client, *pstest.Server, func()) {
	psTestServer := pstest.NewServer(opts...)

	conn, err := grpc.NewClient(psTestServer.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create grpc connection: %v", err)
	}

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, "test-project", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("failed to create pubsub client: %v", err)
	}

	topic, err := client.CreateTopic(ctx, "test-topic")
	if err != nil {
		t.Fatalf("failed to create topic: %v", err)
	}

	return client, psTestServer, func() {
		topic.Stop()
		client.Close()
		conn.Close()
		psTestServer.Close()
	}
}

// Test fubReceiver for successful message publishing.
func TestFubReceiverSuccess(t *testing.T) {
	client, psTestServer, cleanup := setupPubSubTestServer(t)
	defer cleanup()

	topic := client.Topic("test-topic")

	srv := &server{
		xSystemKey: []byte("test_key"),
		topic:      topic,
	}

	body := []byte(`{"event": "peopleCreated"}`)
	encodedBody := base64.StdEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, srv.xSystemKey)
	mac.Write([]byte(encodedBody))
	validSignature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/?param1=value1&param2=value2", bytes.NewReader(body))
	req.Header.Set("FUB-Signature", validSignature)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(srv.fubReceiver)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", rr.Code)
	}

	expected := `{}`
	if rr.Body.String() != expected {
		t.Errorf("expected response body %s, got %s", expected, rr.Body.String())
	}

	messages := psTestServer.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message to be published, got %d", len(messages))
	}

	if eventValue, ok := messages[0].Attributes["event"]; !ok {
		t.Errorf("expected 'event' attribute to be present in the message attributes")
	} else if eventValue != "peopleCreated" {
		t.Errorf("expected 'event' attribute to have value 'peopleCreated', got %s", eventValue)
	}

	msgData := string(messages[0].Data)

	expectedMessage := map[string]interface{}{
		"webhookData": string(body),
		"webhookArgs": map[string]interface{}{
			"param1": "value1",
			"param2": "value2",
		},
	}

	var actualMessage map[string]interface{}
	if err := json.Unmarshal([]byte(msgData), &actualMessage); err != nil {
		t.Fatalf("failed to unmarshal published message: %v", err)
	}

	if !reflect.DeepEqual(expectedMessage, actualMessage) {
		t.Errorf("expected published message %v, got %v", expectedMessage, actualMessage)
	}
}

// Test fubReceiver for invalid signature.
func TestFubReceiverInvalidSignature(t *testing.T) {
	client, _, cleanup := setupPubSubTestServer(t)
	defer cleanup()

	topic := client.Topic("test-topic")

	srv := &server{
		xSystemKey: []byte("test_key"),
		topic:      topic,
	}

	// Create a request body with an invalid signature.
	body := []byte(`{"key": "value"}`)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("FUB-Signature", "invalid_signature")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(srv.fubReceiver)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rr.Code)
	}
}

// Test fubReceiver for missing FUB-Signature header.
func TestFubReceiverMissingSignature(t *testing.T) {
	client, _, cleanup := setupPubSubTestServer(t)
	defer cleanup()

	topic := client.Topic("test-topic")

	srv := &server{
		xSystemKey: []byte("test_key"),
		topic:      topic,
	}

	// Create an HTTP request without the FUB-Signature header.
	req := httptest.NewRequest("POST", "/", nil)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(srv.fubReceiver)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rr.Code)
	}
}

func TestFubReceiverInvalidJSON(t *testing.T) {
	client, _, cleanup := setupPubSubTestServer(t)
	defer cleanup()

	topic := client.Topic("test-topic")

	srv := &server{
		xSystemKey: []byte("test_key"),
		topic:      topic,
	}

	// Create an invalid JSON request body (missing closing brace)
	invalidJSON := `{"key": "value"`

	encodedBody := base64.StdEncoding.EncodeToString([]byte(invalidJSON))
	mac := hmac.New(sha256.New, srv.xSystemKey)
	mac.Write([]byte(encodedBody))
	validSignature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(invalidJSON)))
	req.Header.Set("FUB-Signature", validSignature)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(srv.fubReceiver)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rr.Code)
	}
}

// Test fubReceiver when publishing to Pub/Sub fails.
func TestFubReceiverPublishFailure(t *testing.T) {
	opts := []pstest.ServerReactorOption{
		pstest.WithErrorInjection("Publish", codes.PermissionDenied, "not allowed"),
	}

	client, _, cleanup := setupPubSubTestServer(t, opts...)
	defer cleanup()

	topic := client.Topic("test-topic")

	srv := &server{
		xSystemKey: []byte("test_key"),
		topic:      topic,
	}

	body := []byte(`{"key": "value"}`)
	encodedBody := base64.StdEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, srv.xSystemKey)
	mac.Write([]byte(encodedBody))
	validSignature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("FUB-Signature", validSignature)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(srv.fubReceiver)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500 Internal Server Error, got %d", rr.Code)
	}
}
