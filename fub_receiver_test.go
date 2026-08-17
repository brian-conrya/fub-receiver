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

// --- fan-out and ordering keys ---

func TestResourceIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    []string
	}{
		// encoding/json decodes numbers as float64, so ids must be formatted
		// back to integers -- stringifying directly would yield "1.23e+02" and
		// the ordering key would stop matching between paths.
		{"numeric ids", `{"resourceIds":[123,456]}`, []string{"123", "456"}},
		{"large id", `{"resourceIds":[9007199254740992]}`, []string{"9007199254740992"}},
		{"string ids", `{"resourceIds":["abc"]}`, []string{"abc"}},
		{"missing key", `{"event":"peopleCreated"}`, nil},
		{"empty list", `{"resourceIds":[]}`, nil},
		{"wrong type", `{"resourceIds":"nope"}`, nil},
		{"non-integral dropped", `{"resourceIds":[1.5,2]}`, []string{"2"}},
		{"nulls dropped", `{"resourceIds":[null,7]}`, []string{"7"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			got := resourceIDs(payload)
			if len(got) != len(tc.want) {
				t.Fatalf("resourceIDs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("resourceIDs[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// orderedTestServer builds a server whose publisher has ordering enabled.
// Publishing a keyed message without that flag fails outright.
func orderedTestServer(t *testing.T, opts ...pstest.ServerReactorOption) (*server, *pstest.Server, *recordingPublisher) {
	t.Helper()
	client, psTestServer := setupPubSubTestServer(t, opts...)
	publisher := client.Publisher(testTopicName)
	publisher.EnableMessageOrdering = true
	t.Cleanup(publisher.Stop)

	rec := &recordingPublisher{inner: publisher}
	return &server{xSystemKey: []byte(testKey), publisher: rec}, psTestServer, rec
}

// recordingPublisher delegates to a real publisher (so results behave exactly
// as in production) while recording which ordering keys were resumed.
type recordingPublisher struct {
	inner   *pubsub.Publisher
	keys    []string
	resumed []string
}

func (p *recordingPublisher) Publish(ctx context.Context, msg *pubsub.Message) *pubsub.PublishResult {
	p.keys = append(p.keys, msg.OrderingKey)
	return p.inner.Publish(ctx, msg)
}

func (p *recordingPublisher) ResumePublish(orderingKey string) {
	p.resumed = append(p.resumed, orderingKey)
	p.inner.ResumePublish(orderingKey)
}

func (p *recordingPublisher) Stop() { p.inner.Stop() }

func TestFubReceiverFansOutPerResource(t *testing.T) {
	t.Parallel()

	srv, psTestServer, rec := orderedTestServer(t)

	body := []byte(`{"event":"peopleUpdated","resourceIds":[11,22,33]}`)
	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, signedRequest(t, "/?account=alpha", body))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	messages := psTestServer.Messages()
	if len(messages) != 3 {
		t.Fatalf("expected 3 published messages, got %d", len(messages))
	}

	// All three must be handed to Publish before any result is awaited, so a
	// webhook naming many people costs one round trip, not one per person.
	if len(rec.keys) != 3 {
		t.Errorf("published keys = %v, want 3", rec.keys)
	}

	// Each message must carry exactly its own id: building them from a shared
	// map without copying would leave every message holding the last id.
	got := map[string]string{}
	for _, msg := range messages {
		var decoded map[string]any
		if err := json.Unmarshal(msg.Data, &decoded); err != nil {
			t.Fatalf("failed to unmarshal published message: %v", err)
		}
		webhookData, _ := decoded["webhookData"].(map[string]any)
		ids, _ := webhookData["resourceIds"].([]any)
		if len(ids) != 1 {
			t.Fatalf("message carries %d resourceIds, want 1", len(ids))
		}
		got[msg.OrderingKey], _ = ids[0].(string)

		if msg.Attributes["event"] != "peopleUpdated" {
			t.Errorf("event attribute = %q", msg.Attributes["event"])
		}
		args, _ := decoded["webhookArgs"].(map[string]any)
		if args["account"] != "alpha" {
			t.Errorf("webhookArgs lost during fan-out: %v", args)
		}
	}

	want := map[string]string{"alpha:11": "11", "alpha:22": "22", "alpha:33": "33"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ordering key -> resourceId mismatch (-want +got):\n%s", diff)
	}
}

func TestFubReceiverPublishesUnkeyedWhenNoResources(t *testing.T) {
	t.Parallel()

	srv, psTestServer, _ := orderedTestServer(t)

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, signedRequest(t, "/?account=alpha", []byte(`{"event":"somethingElse"}`)))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	messages := psTestServer.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(messages))
	}
	if messages[0].OrderingKey != "" {
		t.Errorf("expected no ordering key, got %q", messages[0].OrderingKey)
	}
}

func TestWithSingleResourceIDDoesNotMutateTheOriginal(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"event": "peopleUpdated", "resourceIds": []any{1.0, 2.0}}
	single := withSingleResourceID(payload, "1")

	if diff := cmp.Diff([]any{"1"}, single["resourceIds"]); diff != "" {
		t.Errorf("copy has wrong resourceIds (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]any{1.0, 2.0}, payload["resourceIds"]); diff != "" {
		t.Errorf("original payload was mutated (-want +got):\n%s", diff)
	}
}

// A failed ordered publish latches that key: every later message for it is
// rejected immediately and permanently until ResumePublish clears the state.
// Without this, one transient error wedges that person's events for the
// lifetime of the process.
func TestFubReceiverResumesFailedOrderingKeys(t *testing.T) {
	t.Parallel()

	// A non-retryable code on purpose: a retryable one keeps the publisher
	// retrying in the background until its own 60s deadline, and Stop() then
	// blocks draining it.
	srv, _, rec := orderedTestServer(t,
		pstest.WithErrorInjection("Publish", codes.PermissionDenied, "not allowed"))

	rr := httptest.NewRecorder()
	srv.fubReceiver(rr, signedRequest(t, "/?account=alpha",
		[]byte(`{"event":"peopleUpdated","resourceIds":[11,22]}`)))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rr.Code)
	}
	if len(rec.resumed) == 0 {
		t.Fatal("no ordering key was resumed; the key stays wedged for good")
	}
	for _, key := range rec.resumed {
		if key == "" {
			t.Error("resumed an unkeyed message")
		}
	}
}
