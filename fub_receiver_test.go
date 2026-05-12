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
	"strings"
	"testing"

	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	testProject   = "test-project"
	testTopic     = "test-topic"
	testTopicName = "projects/" + testProject + "/topics/" + testTopic
)

// setupPubSubTestServer spins up an in-memory Pub/Sub fake, creates the test
// topic, and returns a connected client plus the fake server for assertions.
// All teardown is registered with t.Cleanup so callers don't have to remember.
func setupPubSubTestServer(t *testing.T, opts ...pstest.ServerReactorOption) (*pubsub.Client, *pstest.Server) {
	t.Helper()

	psTestServer := pstest.NewServer(opts...)
	t.Cleanup(func() { _ = psTestServer.Close() })

	conn, err := grpc.NewClient(psTestServer.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create grpc connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, testProject, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("failed to create pubsub client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: testTopicName}); err != nil {
		t.Fatalf("failed to create topic: %v", err)
	}

	return client, psTestServer
}

const testKey = "test_key"

// signedRequest builds an HTTP request with a valid FUB-Signature for the
// given body, matching the signing scheme used by validateSignature.
func signedRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(testKey))
	mac.Write([]byte(encoded))
	signature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("FUB-Signature", signature)
	return req
}

// Test fubReceiver for successful message publishing.
func TestFubReceiverSuccess(t *testing.T) {
	t.Parallel()

	client, psTestServer := setupPubSubTestServer(t)
	publisher := client.Publisher(testTopicName)
	t.Cleanup(publisher.Stop)

	srv := &server{
		xSystemKey: []byte("test_key"),
		publisher:  publisher,
	}

	body := []byte(`{"event": "peopleCreated"}`)
	req := signedRequest(t, "/?param1=value1&param2=value2", body)

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if got, want := rr.Body.String(), `{}`; got != want {
		t.Errorf("response body = %q, want %q", got, want)
	}

	messages := psTestServer.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(messages))
	}
	if got, want := messages[0].Attributes["event"], "peopleCreated"; got != want {
		t.Errorf("event attribute = %q, want %q", got, want)
	}

	want := map[string]any{
		"webhookData": map[string]any{"event": "peopleCreated"},
		"webhookArgs": map[string]any{"param1": "value1", "param2": "value2"},
	}
	var got map[string]any
	if err := json.Unmarshal(messages[0].Data, &got); err != nil {
		t.Fatalf("failed to unmarshal published message: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published message mismatch (-want +got):\n%s", diff)
	}
}

// Test fubReceiver for invalid signature.
func TestFubReceiverInvalidSignature(t *testing.T) {
	t.Parallel()

	client, _ := setupPubSubTestServer(t)
	publisher := client.Publisher(testTopicName)
	t.Cleanup(publisher.Stop)

	srv := &server{
		xSystemKey: []byte("test_key"),
		publisher:  publisher,
	}

	// Use a body whose computed signature will not match the header value.
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"key":"value"}`)))
	req.Header.Set("FUB-Signature", "invalid_signature")

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rr.Code)
	}
}

// Test fubReceiver for missing FUB-Signature header.
func TestFubReceiverMissingSignature(t *testing.T) {
	t.Parallel()

	client, _ := setupPubSubTestServer(t)
	publisher := client.Publisher(testTopicName)
	t.Cleanup(publisher.Stop)

	srv := &server{
		xSystemKey: []byte("test_key"),
		publisher:  publisher,
	}

	// Request without the FUB-Signature header.
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rr.Code)
	}
}

// Test fubReceiver for malformed JSON in the request body.
func TestFubReceiverInvalidJSON(t *testing.T) {
	t.Parallel()

	client, _ := setupPubSubTestServer(t)
	publisher := client.Publisher(testTopicName)
	t.Cleanup(publisher.Stop)

	srv := &server{
		xSystemKey: []byte("test_key"),
		publisher:  publisher,
	}

	// Invalid JSON (missing closing brace), but with a valid signature so we
	// exercise the JSON-parse failure path rather than the signature check.
	req := signedRequest(t, "/", []byte(`{"key": "value"`))

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rr.Code)
	}
}

// Test fubReceiver rejects bodies above the configured size limit.
func TestFubReceiverBodyTooLarge(t *testing.T) {
	t.Parallel()

	client, _ := setupPubSubTestServer(t)
	publisher := client.Publisher(testTopicName)
	t.Cleanup(publisher.Stop)

	srv := &server{
		xSystemKey: []byte("test_key"),
		publisher:  publisher,
	}

	body := []byte(`{"event":"` + strings.Repeat("a", maxBodyBytes+1) + `"}`)
	req := signedRequest(t, "/", body)

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status 413, got %d", rr.Code)
	}
}

// Test fubReceiver when publishing to Pub/Sub fails.
func TestFubReceiverPublishFailure(t *testing.T) {
	t.Parallel()

	client, _ := setupPubSubTestServer(t, pstest.WithErrorInjection("Publish", codes.PermissionDenied, "not allowed"))
	publisher := client.Publisher(testTopicName)
	t.Cleanup(publisher.Stop)

	srv := &server{
		xSystemKey: []byte("test_key"),
		publisher:  publisher,
	}

	req := signedRequest(t, "/", []byte(`{"key":"value"}`))

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rr.Code)
	}
}
