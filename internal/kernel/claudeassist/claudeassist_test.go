package claudeassist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient_EmptyAPIKeyIsDisabled(t *testing.T) {
	c := NewClient("", "claude-haiku-4-5")
	if c.Enabled() {
		t.Fatal("expected a nil Client (empty apiKey) to report Enabled() == false")
	}
}

func TestNewClient_EmptyModelIsDisabled(t *testing.T) {
	c := NewClient("sk-ant-fake", "")
	if c.Enabled() {
		t.Fatal("expected a nil Client (empty model) to report Enabled() == false")
	}
}

func TestClient_Enabled(t *testing.T) {
	c := NewClient("sk-ant-fake", "claude-haiku-4-5")
	if !c.Enabled() {
		t.Fatal("expected a fully configured Client to report Enabled() == true")
	}
}

func TestGenerateJSON_DisabledClientErrors(t *testing.T) {
	var c *Client
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error calling GenerateJSON on a disabled (nil) client")
	}
}

// TestGenerateJSON_SendsVerifiedRequestShape confirms this package's
// request matches Anthropic's documented Messages API contract exactly
// (endpoint path reachable, X-Api-Key/anthropic-version headers,
// model/max_tokens/messages/output_config body shape) — the wire format
// this package was built against, verified directly from Anthropic's
// current docs before writing any code here.
func TestGenerateJSON_SendsVerifiedRequestShape(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"greeting":"hi"}`}},
		})
	}))
	defer srv.Close()

	// messagesURL is a package const pointing at the real Anthropic
	// endpoint — this test can't override it without changing the
	// package's exported surface just for testing, so it confirms the
	// *shape* (headers, body) against a local server on a different
	// path/host by constructing the client directly and calling the
	// unexported request-building logic indirectly through a swapped
	// http.Client transport instead.
	c := NewClient("sk-ant-fake-key", "claude-haiku-4-5")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}

	schema := map[string]any{"type": "object", "properties": map[string]any{"greeting": map[string]any{"type": "string"}}}
	var out struct {
		Greeting string `json:"greeting"`
	}
	if err := c.GenerateJSON(context.Background(), "say hi", schema, &out); err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if out.Greeting != "hi" {
		t.Fatalf("expected Greeting %q, got %q", "hi", out.Greeting)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("expected POST /v1/messages, got %s", gotPath)
	}
	if gotAPIKey != "sk-ant-fake-key" {
		t.Errorf("expected X-Api-Key %q, got %q", "sk-ant-fake-key", gotAPIKey)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("expected anthropic-version %q, got %q", anthropicVersion, gotVersion)
	}
	if gotBody["model"] != "claude-haiku-4-5" {
		t.Errorf("expected model %q, got %v", "claude-haiku-4-5", gotBody["model"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %v", gotBody["messages"])
	}
	firstMsg, _ := msgs[0].(map[string]any)
	if firstMsg["role"] != "user" || firstMsg["content"] != "say hi" {
		t.Errorf("expected a user message with the prompt, got %v", firstMsg)
	}
	outputConfig, _ := gotBody["output_config"].(map[string]any)
	if outputConfig["type"] != "json_schema" {
		t.Errorf("expected output_config.type=json_schema, got %v", outputConfig)
	}
	if outputConfig["schema"] == nil {
		t.Error("expected output_config.schema to carry the JSON Schema")
	}
}

// rewriteHostTransport redirects every request to target's host,
// preserving path/query — lets a test point Client.http at an
// httptest.Server without changing this package's hardcoded, real
// Anthropic endpoint constant.
type rewriteHostTransport struct {
	target string
}

func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	targetURL, err := req.URL.Parse(t.target)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme = targetURL.Scheme
	req.URL.Host = targetURL.Host
	req.Host = targetURL.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestGenerateJSON_ServerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid x-api-key"))
	}))
	defer srv.Close()

	c := NewClient("sk-ant-fake", "claude-haiku-4-5")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}

// TestGenerateJSON_NoTextContentBlockIsAnError guards the real shape of
// Anthropic's response envelope (content is an array of typed blocks,
// not a flat field) — an empty content array must be reported as an
// error, not silently unmarshal a zero-value out.
func TestGenerateJSON_NoTextContentBlockIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{}})
	}))
	defer srv.Close()

	c := NewClient("sk-ant-fake", "claude-haiku-4-5")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the response has no text content block")
	}
}

func TestGenerateJSON_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{{"type": "text", "text": "{}"}}})
	}))
	defer srv.Close()

	c := NewClient("sk-ant-fake", "claude-haiku-4-5")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var out map[string]any
	if err := c.GenerateJSON(ctx, "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the context deadline is shorter than the server's response time")
	}
}
