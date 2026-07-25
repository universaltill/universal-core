// Package claudeassist is a thin client for Anthropic's Messages API —
// a tenant-supplied-API-key BYOK plugin (internal/kernel/aiprovider.
// Provider), never the platform's own default (see internal/kernel/
// aiassist's doc comment on why that stays self-hosted Ollama). A
// tenant who configures their own Anthropic API key is spending their
// own money against their own account, on their own explicit opt-in —
// this package has no knowledge of tenants, billing, or how a key got
// configured; internal/api owns that, this package only ever does "send
// a prompt plus a JSON Schema, get back a value matching that schema,"
// same as every Provider implementation in this kernel.
//
// Request/response shapes verified directly against Anthropic's current
// API docs (platform.claude.com/docs/en/api/messages and .../build-with-
// claude/structured-outputs) before writing this, not assumed from
// training-time familiarity with an older API version — Anthropic
// banned third-party use of consumer Claude.ai/Claude Pro/Max
// *subscription* OAuth tokens in third-party tools (fully enforced
// 2026-04-04); this package only ever uses a real developer API key
// (console.anthropic.com), the supported, stable integration path.
package claudeassist

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

var _ aiprovider.Provider = (*Client)(nil)

// Client talks to the Anthropic Messages API with one tenant-supplied
// API key. The zero value is unusable — construct via NewClient, same
// "empty key means legitimately disabled, not a Client that will error
// on first use" contract every Provider implementation in this kernel
// holds to.
type Client struct {
	apiKey string
	model  string
	http   *http.Client
}

// defaultTimeout matches aiassist's own reasoning: a synchronous
// generate call can genuinely take tens of seconds, this is a ceiling
// a caller with a tighter interactive budget can still override via a
// shorter-deadlined context.
const defaultTimeout = 90 * time.Second

const messagesURL = "https://api.anthropic.com/v1/messages"

// anthropicVersion is a required header, not a request body field —
// pinned to the version verified against the current API docs.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens bounds a single structured-output response — a
// mapping suggestion or similar short structured answer never
// legitimately needs more than this; bounding it caps runaway cost on
// the tenant's own API key, not just runaway response size.
const defaultMaxTokens = 4096

// NewClient builds a Client for apiKey (a real Anthropic developer API
// key, console.anthropic.com — never a Claude.ai/Claude Pro/Max
// subscription token, which Anthropic's own terms now forbid using in
// third-party tools) and model (e.g. "claude-haiku-4-5" — no default is
// applied here; the tenant configuring this explicitly chooses, since
// model choice directly drives their own API cost). apiKey == "" or
// model == "" returns nil, not a half-configured Client — see Enabled's
// doc comment.
func NewClient(apiKey, model string) *Client {
	if apiKey == "" || model == "" {
		return nil
	}
	return &Client{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: defaultTimeout},
	}
}

// Enabled reports whether c is usable — false for a nil *Client, the
// same nil-receiver ergonomic every Provider implementation in this
// kernel establishes.
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != "" && c.model != ""
}

// messagesRequest/messagesResponse are the Messages API's wire shapes —
// only the fields this package actually sends/reads; the real response
// carries more (usage, stop_reason, id) that no caller here needs.
type messagesRequest struct {
	Model        string               `json:"model"`
	MaxTokens    int                  `json:"max_tokens"`
	Messages     []messagesMsg        `json:"messages"`
	OutputConfig messagesOutputConfig `json:"output_config"`
}

type messagesMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messagesOutputConfig is Anthropic's current, documented-recommended
// structured-output mechanism (superseding the older forced-tool-use
// pattern, which this package deliberately doesn't use).
type messagesOutputConfig struct {
	Type   string `json:"type"`
	Schema any    `json:"schema"`
}

type messagesResponse struct {
	Content []messagesContentBlock `json:"content"`
}

type messagesContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// GenerateJSON sends prompt to the model with schema as Anthropic's
// output_config JSON Schema constraint, then unmarshals the model's
// response text into out. The response's structured JSON is documented
// to arrive as a string inside content[0].text (a text content block),
// not a separate typed field — verified against Anthropic's own
// structured-outputs example response before writing this.
func (c *Client) GenerateJSON(ctx context.Context, prompt string, schema any, out any) error {
	if !c.Enabled() {
		return fmt.Errorf("claudeassist: client not configured")
	}

	reqBody, err := json.Marshal(messagesRequest{
		Model:     c.model,
		MaxTokens: defaultMaxTokens,
		Messages:  []messagesMsg{{Role: "user", Content: prompt}},
		OutputConfig: messagesOutputConfig{
			Type:   "json_schema",
			Schema: schema,
		},
	})
	if err != nil {
		return fmt.Errorf("claudeassist: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("claudeassist: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("claudeassist: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB — same reasoning aiassist's own cap gives: a structured response is never legitimately larger, this bounds a misbehaving server rather than trusting Content-Length
	if err != nil {
		return fmt.Errorf("claudeassist: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Never includes the API key in an error message reaching a
		// caller — the key lives only in the request header, never
		// echoed into body/err strings this function builds.
		return fmt.Errorf("claudeassist: server returned %d: %s", resp.StatusCode, string(body))
	}

	var mr messagesResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return fmt.Errorf("claudeassist: unmarshal server response: %w", err)
	}
	if len(mr.Content) == 0 || mr.Content[0].Text == "" {
		return fmt.Errorf("claudeassist: server response had no text content block")
	}
	if err := json.Unmarshal([]byte(mr.Content[0].Text), out); err != nil {
		return fmt.Errorf("claudeassist: model output didn't match the requested schema: %w", err)
	}
	return nil
}
