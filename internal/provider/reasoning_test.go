package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordingEndpoint answers with a valid completion and records what it was
// asked, so a test can assert on the request rather than on the reply.
type recordingEndpoint struct {
	server *httptest.Server

	// bodies is every request received, decoded.
	bodies []map[string]any

	// reject is a field name this endpoint answers 400 to, as a server that
	// does not know it would.
	reject string
}

func newRecordingEndpoint(t *testing.T) *recordingEndpoint {
	t.Helper()

	endpoint := &recordingEndpoint{}

	endpoint.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		endpoint.bodies = append(endpoint.bodies, body)

		if endpoint.reject != "" {
			if _, present := body[endpoint.reject]; present {
				// The shape a server uses for a field it does not know.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: ` +
					endpoint.reject + `"}}`))
				return
			}
		}

		reply := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(endpoint.server.Close)

	return endpoint
}

func (e *recordingEndpoint) client(t *testing.T) *Client {
	t.Helper()

	client, err := NewClient(&Provider{
		Name: "recording", Kind: KindLLM, Type: TypeOpenAICompatible,
		BaseURL: e.server.URL + "/v1", APIKey: "test", Model: "test-model", Enabled: true,
	}, ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// An empty effort is not sent at all.
//
// The distinction matters: "leave the endpoint's default alone" and "ask for
// the lowest effort it supports" are different requests, and a client that
// conflated them would silently change the behaviour of every model that has
// the field.
func TestReasoningEffortIsOmittedWhenUnset(t *testing.T) {
	endpoint := newRecordingEndpoint(t)

	if _, err := endpoint.client(t).Chat(context.Background(), ChatRequest{User: "hi"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if len(endpoint.bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(endpoint.bodies))
	}
	if _, present := endpoint.bodies[0]["reasoning_effort"]; present {
		t.Errorf("reasoning_effort was sent for an unset request: %v", endpoint.bodies[0])
	}
}

func TestReasoningEffortIsSentWhenSet(t *testing.T) {
	endpoint := newRecordingEndpoint(t)

	if _, err := endpoint.client(t).Chat(context.Background(), ChatRequest{
		User: "hi", ReasoningEffort: "low",
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if got := endpoint.bodies[0]["reasoning_effort"]; got != "low" {
		t.Errorf("reasoning_effort = %v, want \"low\"", got)
	}
}

// An endpoint that does not know the field gets a second request without it.
//
// This is what keeps the setting from turning a working provider into a broken
// one: the field is sent optimistically, and a server that refuses it is
// remembered rather than asked again on every request of a translation run.
func TestReasoningEffortIsDroppedWhenTheEndpointRejectsIt(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	endpoint.reject = "reasoning_effort"

	client := endpoint.client(t)

	reply, err := client.Chat(context.Background(), ChatRequest{User: "hi", ReasoningEffort: "low"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply.Content != "ok" {
		t.Errorf("content = %q", reply.Content)
	}

	if len(endpoint.bodies) != 2 {
		t.Fatalf("requests = %d, want 2 (the refusal and the correction)", len(endpoint.bodies))
	}
	if _, present := endpoint.bodies[1]["reasoning_effort"]; present {
		t.Errorf("the correction still carried the field: %v", endpoint.bodies[1])
	}

	// And it stays dropped. A translation run makes hundreds of requests, and
	// renegotiating on each one would double the cost of every one of them.
	before := len(endpoint.bodies)
	if _, err := client.Chat(context.Background(), ChatRequest{User: "again", ReasoningEffort: "low"}); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if len(endpoint.bodies) != before+1 {
		t.Errorf("the field was renegotiated on a later request: %d requests, want %d",
			len(endpoint.bodies), before+1)
	}
}
