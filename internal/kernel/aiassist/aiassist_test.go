package aiassist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient_EmptyBaseURLIsDisabled(t *testing.T) {
	c := NewClient("", "llama3.2:3b")
	if c.Enabled() {
		t.Fatal("expected a nil Client (empty baseURL) to report Enabled() == false")
	}
}

func TestClient_Enabled(t *testing.T) {
	c := NewClient("http://example.invalid", "llama3.2:3b")
	if !c.Enabled() {
		t.Fatal("expected a Client with a non-empty baseURL to report Enabled() == true")
	}
}

func TestGenerateJSON_DisabledClientErrors(t *testing.T) {
	var c *Client
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error calling GenerateJSON on a disabled (nil) client")
	}
}

// TestGenerateJSON_SendsModelPromptAndSchema confirms the request this
// package sends actually matches Ollama's documented /api/generate
// contract (model/prompt/format/stream) — a caller-facing test of the
// wire format, not just that some HTTP call happens.
func TestGenerateJSON_SendsModelPromptAndSchema(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("expected POST /api/generate, got %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": `{"greeting":"hi"}`,
			"done":     true,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "llama3.2:3b")
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

	if gotBody["model"] != "llama3.2:3b" {
		t.Errorf("expected model %q in request, got %v", "llama3.2:3b", gotBody["model"])
	}
	if gotBody["prompt"] != "say hi" {
		t.Errorf("expected prompt %q in request, got %v", "say hi", gotBody["prompt"])
	}
	if gotBody["stream"] != false {
		t.Errorf("expected stream=false in request, got %v", gotBody["stream"])
	}
	if _, ok := gotBody["format"].(map[string]any); !ok {
		t.Errorf("expected format to be the JSON Schema object, got %v", gotBody["format"])
	}
}

func TestGenerateJSON_ServerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("model not found"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "llama3.2:3b")
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}

// TestGenerateJSON_ResponseNotMatchingSchema confirms a model output
// that isn't even valid JSON (a small model can genuinely do this) is
// reported as an error, not silently accepted as a zero-value out —
// the caller needs to know the suggestion didn't happen, not get a
// falsely-empty-but-"successful" result.
func TestGenerateJSON_ResponseNotMatchingSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": "not valid json at all",
			"done":     true,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "llama3.2:3b")
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the model's response isn't valid JSON")
	}
}

func TestGenerateJSON_IncompleteResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": `{}`,
			"done":     false,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "llama3.2:3b")
	var out map[string]any
	if err := c.GenerateJSON(context.Background(), "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the server reports done=false")
	}
}

// TestGenerateJSON_RespectsContextDeadline confirms a caller-supplied
// short deadline actually aborts the call rather than waiting out
// defaultTimeout — important since a caller assisting an interactive
// HTTP request (the import wizard, say) needs to bound how long a user
// waits for an AI suggestion, independent of this package's own
// generous ceiling.
func TestGenerateJSON_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response": `{}`, "done": true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "llama3.2:3b")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var out map[string]any
	if err := c.GenerateJSON(ctx, "prompt", map[string]any{}, &out); err == nil {
		t.Fatal("expected an error when the context deadline is shorter than the server's response time")
	}
}
