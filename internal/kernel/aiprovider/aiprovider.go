// Package aiprovider defines the one contract every AI text-generation
// backend this kernel can talk to implements — prompt + JSON Schema in,
// a parsed structured value out. internal/kernel/aiassist (self-hosted
// Ollama, the platform default), internal/kernel/claudeassist
// (Anthropic Messages API, customer-supplied key), and internal/kernel/
// openaiassist (OpenAI Chat Completions, customer-supplied key) each
// implement it against a completely different wire format underneath;
// a caller building a prompt and interpreting the result (csvimport.
// SuggestMappingAI, say) never needs to know which one it's actually
// talking to.
//
// Deliberately its own minimal package rather than living inside
// aiassist: aiassist's own doc comment scopes it specifically to
// Ollama, and every sibling AI-client package in this kernel
// (aiassist, speechassist) is already single-vendor, single-purpose —
// this package is the one exception that's intentionally generic,
// existing purely so the three concrete clients and their callers can
// share a type without aiassist growing a second, broader
// responsibility it was never scoped for.
//
// This is a plugin point, not a policy: which provider a given tenant
// actually uses (the platform's own self-hosted Ollama by default, or
// a customer-supplied Anthropic/OpenAI API key they've configured) is
// resolved per request by internal/api — see that package's
// aiProviderFor.
package aiprovider

import "context"

// Provider is satisfied by every concrete AI client this kernel builds
// (aiassist.Client, claudeassist.Client, openaiassist.Client — Go's
// implicit interface satisfaction means none of them import this
// package just to declare it). A nil-valued Provider (a typed nil
// pointer to a disabled concrete client) must still be safe to call
// Enabled() on and report false — see aiassist.Client.Enabled's own
// doc comment for why every concrete implementation holds to that
// contract, since callers rely on it to avoid a separate nil check at
// every call site.
type Provider interface {
	// Enabled reports whether this Provider is actually configured and
	// usable — false for a disabled/unconfigured client, never a panic
	// or a call that then fails downstream.
	Enabled() bool
	// GenerateJSON sends prompt to the model with schema as a JSON
	// Schema constraint on its output, then unmarshals the result into
	// out. Returns a plain error on any failure (network, timeout, a
	// response that doesn't match schema) — never panics, never
	// retries; the caller decides how to degrade.
	GenerateJSON(ctx context.Context, prompt string, schema any, out any) error
}
