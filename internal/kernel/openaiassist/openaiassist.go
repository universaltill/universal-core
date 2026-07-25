// Package openaiassist is a thin client for OpenAI's Chat Completions
// API — a tenant-supplied-API-key BYOK plugin (internal/kernel/
// aiprovider.Provider), never the platform's own default. Same "this is
// the customer's own account, own cost, own opt-in" framing as
// internal/kernel/claudeassist's own doc comment; see that package's
// comment for the fuller reasoning, identical here.
//
// Request/response shape verified directly against OpenAI's current API
// docs (developers.openai.com/api/docs/guides/structured-outputs)
// before writing this. OpenAI's own "Sign in with ChatGPT" consumer-
// subscription login exists but, as of this package being written,
// ships only inside OpenAI's own Codex tooling — there is no supported
// way for a third-party app to bill a user's ChatGPT Plus/Pro
// subscription for API calls, only a real developer API key
// (platform.openai.com), which is what this package exclusively uses.
package openaiassist

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

// Client talks to the OpenAI Chat Completions API with one tenant-
// supplied API key. The zero value is unusable — construct via
// NewClient, same "empty means legitimately disabled" contract every
// Provider implementation in this kernel holds to.
type Client struct {
	apiKey string
	model  string
	http   *http.Client
}

// defaultTimeout matches every other Provider implementation's own
// reasoning — generous, overridable by a caller with a tighter
// interactive budget via a shorter-deadlined context.
const defaultTimeout = 90 * time.Second

const chatCompletionsURL = "https://api.openai.com/v1/chat/completions"

// jsonSchemaName is the required, purely-descriptive identifier OpenAI's
// response_format.json_schema.name field asks for — this package always
// requests exactly one shape per call, so a fixed generic name is fine;
// nothing reads it back.
const jsonSchemaName = "structured_response"

// NewClient builds a Client for apiKey (a real OpenAI developer API
// key, platform.openai.com — never a ChatGPT Plus/Pro consumer
// subscription login) and model (e.g. "gpt-5.6-mini" — no default
// applied, the tenant configuring this explicitly picks, since it
// drives their own API cost). apiKey == "" or model == "" returns nil —
// see Enabled's doc comment.
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

// Enabled reports whether c is usable — false for a nil *Client, same
// nil-receiver ergonomic every Provider implementation in this kernel
// establishes.
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != "" && c.model != ""
}

// chatRequest/chatResponse are the Chat Completions wire shapes — only
// the fields this package actually sends/reads.
type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseFormat is OpenAI's Structured Outputs contract — strict:true
// is required for OpenAI to actually guarantee schema conformance
// (without it, response_format.json_schema is only a best-effort hint),
// verified directly against the current docs before writing this.
type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema jsonSchemaSpec `json:"json_schema"`
}

type jsonSchemaSpec struct {
	Name   string `json:"name"`
	Schema any    `json:"schema"`
	Strict bool   `json:"strict"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

// GenerateJSON sends prompt to the model with schema as an OpenAI
// Structured Outputs constraint (response_format.json_schema, strict
// mode), then unmarshals the model's response into out. The structured
// JSON arrives as a string in choices[0].message.content — the same
// long-standing Chat Completions response envelope shape Structured
// Outputs doesn't change, just guarantees the content string's schema
// conformance.
func (c *Client) GenerateJSON(ctx context.Context, prompt string, schema any, out any) error {
	if !c.Enabled() {
		return fmt.Errorf("openaiassist: client not configured")
	}

	reqBody, err := json.Marshal(chatRequest{
		Model:    c.model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchemaSpec{
				Name:   jsonSchemaName,
				Schema: schema,
				Strict: true,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("openaiassist: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("openaiassist: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openaiassist: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB — same reasoning claudeassist's own cap gives
	if err != nil {
		return fmt.Errorf("openaiassist: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Never includes the API key in an error — it lives only in the
		// request header, never echoed into a body/err string here.
		return fmt.Errorf("openaiassist: server returned %d: %s", resp.StatusCode, string(body))
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return fmt.Errorf("openaiassist: unmarshal server response: %w", err)
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return fmt.Errorf("openaiassist: server response had no choices")
	}
	if err := json.Unmarshal([]byte(cr.Choices[0].Message.Content), out); err != nil {
		return fmt.Errorf("openaiassist: model output didn't match the requested schema: %w", err)
	}
	return nil
}
