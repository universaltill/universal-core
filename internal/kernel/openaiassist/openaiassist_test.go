package openaiassist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient_EmptyAPIKeyIsDisabled(t *testing.T) {
	c := NewClient("", "gpt-5.6-mini")
	if c.Enabled() {
		t.Fatal("expected a nil Client (empty apiKey) to report Enabled() == false")
	}
}

func TestNewClient_EmptyModelIsDisabled(t *testing.T) {
	c := NewClient("sk-fake", "")
	if c.Enabled() {
		t.Fatal("expected a nil Client (empty model) to report Enabled() == false")
	}
}

func TestClient_Enabled(t *testing.T) {
	c := NewClient("sk-fake", "gpt-5.6-mini")
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

// rewriteHostTransport redirects every request to target's host,
// preserving path/query — lets a test point Client.http at an
// httptest.Server without changing this package's hardcoded, real
// OpenAI endpoint constant. Same helper claudeassist's own tests use.
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

// TestGenerateJSON_SendsVerifiedRequestShape confirms this package's
// request matches OpenAI's documented Structured Outputs contract
// exactly (endpoint path, Bearer auth, model/messages/response_format
// body shape with strict:true) — verified directly from OpenAI's
// current docs before writing any code here.
func TestGenerateJSON_SendsVerifiedRequestShape(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"greeting":"hi"}`}},
			},
		})
	}))
	defer srv.Close()

	c := NewClient("sk-fake-key", "gpt-5.6-mini")
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

	if gotPath != "/v1/chat/completions" {
		t.Errorf("expected POST /v1/chat/completions, got %s", gotPath)
	}
	if gotAuth != "Bearer sk-fake-key" {
		t.Errorf("expected Authorization %q, got %q", "Bearer sk-fake-key", gotAuth)
	}
	if gotBody["model"] != "gpt-5.6-mini" {
		t.Errorf("expected model %q, got %v", "gpt-5.6-mini", gotBody["model"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %v", gotBody["messages"])
	}
	firstMsg, _ := msgs[0].(map[string]any)
	if firstMsg["role"] != "user" || firstMsg["content"] != "say hi" {
		t.Errorf("expected a user message with the prompt, got %v", firstMsg)
	}
	rf, _ := gotBody["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("expected response_format.type=json_schema, got %v", rf)
	}
	js, _ := rf["json_schema"].(map[string]any)
	if js["strict"] != true {
		t.Errorf("expected json_schema.strict=true (required for OpenAI to actually enforce the schema), got %v", js["strict"])
	}
	if js["schema"] == nil {
		t.Error("expected json_schema.schema to carry the JSON Schema")
	}
}

func TestGenerateJSON_ServerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid api key"))
	}))
	defer srv.Close()

	c := NewClient("sk-fake", "gpt-5.6-mini")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}

// TestGenerateJSON_NoChoicesIsAnError guards the real shape of the Chat
// Completions response envelope — an empty choices array must be
// reported as an error, not silently unmarshal a zero-value out.
func TestGenerateJSON_NoChoicesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{}})
	}))
	defer srv.Close()

	c := NewClient("sk-fake", "gpt-5.6-mini")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the response has no choices")
	}
}

func TestGenerateJSON_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "{}"}}},
		})
	}))
	defer srv.Close()

	c := NewClient("sk-fake", "gpt-5.6-mini")
	c.http = &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var out map[string]any
	if err := c.GenerateJSON(ctx, "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the context deadline is shorter than the server's response time")
	}
}
