// Package speechassist is a thin, generic client for a self-hosted
// Whisper ASR (speech-to-text) instance — the transcription half of the
// same "self-hosted AI only, no paid third-party APIs" rule
// internal/kernel/aiassist already follows for Ollama chat/completion.
// Deliberately its own package, not folded into aiassist: Ollama and
// Whisper ASR are different services with different wire protocols
// (JSON generate vs. multipart audio upload), and aiassist's own doc
// comment specifically scopes it to Ollama — conflating the two would
// blur what's actually a clean, single-purpose client each.
//
// Like aiassist, nothing in this package knows about any specific
// feature (the issue logger, or anything else that transcribes audio
// later) — it only ever does "send audio, get back text." A feature
// package builds the capture UI and decides what to do with the
// transcript; this package never does.
package speechassist

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Client talks to one self-hosted Whisper ASR server (the
// onerahmet/openai-whisper-asr-webservice API shape — see
// ../../../../unitill/homelab-k8s/kubernetes/apps/whisper/README.md for
// the reference deployment this was written and verified against). The
// zero value is unusable — construct via NewClient, which is also where
// "no STT configured" becomes a legitimate, always-safe state (see
// Enabled), mirroring aiassist.NewClient exactly.
type Client struct {
	baseURL string
	http    *http.Client
}

// defaultTimeout is generous for the same reason aiassist's is: the
// reference deployment runs on modest hardware (see that package's own
// doc comment on why aiassist's default is 90s, not a tight budget) —
// a voice note's transcription is a synchronous request-response, and a
// cold model/busy server can genuinely take tens of seconds.
const defaultTimeout = 90 * time.Second

// NewClient builds a Client for baseURL (e.g.
// "http://whisper.whisper.svc.cluster.local" in-cluster, or an ingress
// URL). baseURL == "" returns nil, not a Client with an empty URL — see
// Enabled's doc comment on why every caller can then treat "STT not
// configured" and "got back a nil *Client" as the exact same case.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// Enabled reports whether c is usable — false for a nil *Client, same
// nil-receiver ergonomic aiassist.Client.Enabled() already establishes
// in this kernel, so every call site can write `if stt.Enabled() { ... }`
// without a separate nil check.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// Transcribe uploads audio (as filename, e.g. "note.webm" — the server
// uses the extension/content to pick its decoder, via ffmpeg
// server-side, so the caller doesn't need to pre-convert whatever
// format the browser's MediaRecorder produced) to the Whisper server's
// POST /asr endpoint and returns the plain-text transcript.
//
// Deliberately returns a plain error, no retries — a network failure,
// context deadline, or server error are all real, expected outcomes a
// caller must treat as "transcription unavailable," the same contract
// aiassist.Client.GenerateJSON already holds callers to.
func (c *Client) Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("speechassist: client not configured")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("audio_file", filename)
	if err != nil {
		return "", fmt.Errorf("speechassist: build multipart body: %w", err)
	}
	if _, err := io.Copy(fw, audio); err != nil {
		return "", fmt.Errorf("speechassist: write audio to request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("speechassist: close multipart writer: %w", err)
	}

	// output=text: the server's own default is a downloadable text/plain
	// attachment either way (see its /asr route), this just makes the
	// choice explicit rather than relying on the server's own default
	// staying "txt" — task=transcribe (not "translate") since a voice
	// bug report should stay in whatever language it was spoken, not
	// get auto-translated to English.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/asr?output=text&task=transcribe", &body)
	if err != nil {
		return "", fmt.Errorf("speechassist: build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("speechassist: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB — a plain-text transcript of a short voice note is never legitimately larger; caps a misbehaving server rather than trusting Content-Length
	if err != nil {
		return "", fmt.Errorf("speechassist: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("speechassist: server returned %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}
