// Package aiassist is a thin, generic client for a self-hosted Ollama
// instance — the platform's own default in-product AI (no paid third-
// party AI APIs *required*; Ollama/custom models only, same rule as
// Universal Till). A tenant may additionally opt into their own
// Anthropic/OpenAI API key as a BYOK plugin (internal/kernel/
// claudeassist, internal/kernel/openaiassist — see aiprovider.Provider,
// the shared interface all three implement) — that's the customer's own
// account, own cost, own data-sharing consent, never the product itself
// depending on or paying for a third-party AI vendor. Nothing in this
// package knows about any specific feature (import mapping, issue
// triage, ...) — it only ever does "send a prompt plus a JSON Schema,
// get back a value matching that schema," the same discipline every
// generic engine in this kernel already follows (CLAUDE.md's kernel-
// boundary rule): a feature package (internal/kernel/csvimport, say)
// builds the prompt and interprets the result, this package never does.
//
// A *Client is advisory infrastructure, never a source of truth: every
// caller must treat a nil/disabled Client, a timeout, or a malformed
// response as "AI assistance unavailable" and fall back to whatever
// non-AI behaviour existed before this package did — never an error
// that blocks the feature it's assisting.
package aiassist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/universaltill/universal-core/internal/kernel/aiprovider"
)

// Compile-time proof *Client actually satisfies aiprovider.Provider —
// nothing else in this package depends on aiprovider at all, this line
// exists purely so a signature drift here fails the build immediately
// instead of surfacing as a confusing type error wherever a *Client is
// first passed somewhere expecting the interface.
var _ aiprovider.Provider = (*Client)(nil)

// Client talks to one Ollama server. The zero value is unusable —
// construct via NewClient, which is also where "no AI configured"
// becomes a legitimate, always-safe state (see Enabled).
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// defaultTimeout is generous, not stingy: this platform's own reference
// deployment (homelab-k8s's ollama app) deliberately runs a small model
// (llama3.2:3b) on 2-node Raspberry-Pi-class hardware specifically so it
// stays cheap to run — a cold or under-load response can genuinely take
// tens of seconds. A caller with a tighter budget can still pass a
// shorter-deadlined context; this is only the ceiling.
const defaultTimeout = 90 * time.Second

// NewClient builds a Client for baseURL (e.g.
// "http://ollama.ollama.svc.cluster.local" in-cluster, or an ingress
// URL) and model (e.g. "llama3.2:3b"). baseURL == "" deliberately
// returns nil, not a Client with an empty URL — see Enabled's doc
// comment for why every caller can then treat "AI not configured" and
// "got back a nil *Client" as the exact same case, with no separate
// "is this even set up" check needed anywhere else in the kernel.
func NewClient(baseURL, model string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{
		baseURL: baseURL,
		model:   model,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// Enabled reports whether c is usable — false for a nil *Client (Go
// allows calling a method on a nil pointer receiver as long as the
// method itself never dereferences it on that path), so every call site
// can write `if ai.Enabled() { ... }` without a separate nil check, the
// same ergonomic a nil *sql.Tx or nil *log.Logger convention would give.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// generateRequest/generateResponse are Ollama's /api/generate wire
// shapes (https://github.com/ollama/ollama/blob/main/docs/api.md) —
// only the fields this package actually uses; Ollama's real response
// carries more (timing stats, token counts) that no caller here needs.
type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Format any    `json:"format"`
	Stream bool   `json:"stream"`
}

type generateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// GenerateJSON sends prompt to the model with schema as Ollama's
// "format" constraint (a JSON Schema object — Ollama itself enforces
// the model's output matches it; confirmed against the live
// homelab-k8s Ollama, server version 0.32.0, which supports schema-
// constrained structured output, not just the older bare "format":
// "json"), then unmarshals the model's response text into out.
//
// Deliberately returns a plain error rather than panicking or retrying
// — network failure, context deadline, and a schema-violating response
// (a small model can still occasionally emit something that doesn't
// parse) are all real, expected outcomes a caller must treat as
// "no AI suggestion this time," not something this package tries to
// paper over with retries a caller didn't ask for.
func (c *Client) GenerateJSON(ctx context.Context, prompt string, schema any, out any) error {
	if !c.Enabled() {
		return fmt.Errorf("aiassist: client not configured")
	}

	reqBody, err := json.Marshal(generateRequest{
		Model:  c.model,
		Prompt: prompt,
		Format: schema,
		Stream: false,
	})
	if err != nil {
		return fmt.Errorf("aiassist: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/generate", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("aiassist: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("aiassist: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB — a structured mapping/summary response is never legitimately larger; caps a misbehaving server rather than trusting Content-Length
	if err != nil {
		return fmt.Errorf("aiassist: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("aiassist: server returned %d: %s", resp.StatusCode, string(body))
	}

	var gr generateResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return fmt.Errorf("aiassist: unmarshal server response: %w", err)
	}
	if !gr.Done {
		return fmt.Errorf("aiassist: server response marked incomplete")
	}
	if err := json.Unmarshal([]byte(gr.Response), out); err != nil {
		return fmt.Errorf("aiassist: model output didn't match the requested schema: %w", err)
	}
	return nil
}
