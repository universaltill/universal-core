package speechassist

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient_EmptyBaseURLIsDisabled(t *testing.T) {
	c := NewClient("")
	if c.Enabled() {
		t.Fatal("expected a nil Client (empty baseURL) to report Enabled() == false")
	}
}

func TestClient_Enabled(t *testing.T) {
	c := NewClient("http://example.invalid")
	if !c.Enabled() {
		t.Fatal("expected a Client with a non-empty baseURL to report Enabled() == true")
	}
}

func TestTranscribe_DisabledClientErrors(t *testing.T) {
	var c *Client
	if _, err := c.Transcribe(context.Background(), strings.NewReader("fake audio"), "note.webm", "en"); err == nil {
		t.Fatal("expected an error calling Transcribe on a disabled (nil) client")
	}
}

// TestTranscribe_SendsMultipartAudioFile confirms the request this
// package sends actually matches the Whisper ASR server's documented
// POST /asr contract (multipart audio_file field, output=text,
// task=transcribe, language query params) — a wire-format test, not
// just that some HTTP call happens.
func TestTranscribe_SendsMultipartAudioFile(t *testing.T) {
	var gotPath, gotQuery, gotFilename, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		file, header, err := r.FormFile("audio_file")
		if err != nil {
			t.Fatalf("read audio_file field: %v", err)
		}
		defer file.Close()
		gotFilename = header.Filename
		content, _ := io.ReadAll(file)
		gotContent = string(content)

		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	transcript, err := c.Transcribe(context.Background(), strings.NewReader("fake audio bytes"), "note.webm", "ar")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if transcript != "hello world" {
		t.Fatalf("expected transcript %q, got %q", "hello world", transcript)
	}
	if gotPath != "/asr" {
		t.Errorf("expected POST /asr, got %s", gotPath)
	}
	if !strings.Contains(gotQuery, "output=text") || !strings.Contains(gotQuery, "task=transcribe") {
		t.Errorf("expected output=text&task=transcribe in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "language=ar") {
		t.Errorf("expected the supplied language to be forwarded as language=ar, got %q", gotQuery)
	}
	if gotFilename != "note.webm" {
		t.Errorf("expected filename %q, got %q", "note.webm", gotFilename)
	}
	if gotContent != "fake audio bytes" {
		t.Errorf("expected audio content %q, got %q", "fake audio bytes", gotContent)
	}
}

// TestTranscribe_EmptyLanguageOmitsTheParameterEntirely confirms a
// caller with no language hint gets the server's own auto-detect
// behavior (no language param at all), not a literal "language=" empty
// value — the server's OpenAPI schema gives that parameter no default,
// so an empty string isn't the same as omitting it.
func TestTranscribe_EmptyLanguageOmitsTheParameterEntirely(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = r.ParseMultipartForm(1 << 20)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.Transcribe(context.Background(), strings.NewReader("audio"), "note.webm", ""); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if strings.Contains(gotQuery, "language") {
		t.Errorf("expected no language parameter at all for an empty language hint, got query %q", gotQuery)
	}
}

func TestTranscribe_ServerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("model not loaded"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.Transcribe(context.Background(), strings.NewReader("audio"), "note.webm", "en"); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}

// TestTranscribe_RespectsContextDeadline mirrors aiassist's own
// equivalent test — a caller assisting an interactive HTTP request (the
// issue logger, say) needs to bound how long a user waits for
// transcription, independent of this package's own generous ceiling.
func TestTranscribe_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte("too slow"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Transcribe(ctx, strings.NewReader("audio"), "note.webm", "en"); err == nil {
		t.Fatal("expected an error when the context deadline is shorter than the server's response time")
	}
}
