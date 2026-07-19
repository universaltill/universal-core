package httpx

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, map[string]string{"status": "ok"})

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response isn't valid JSON: %v", err)
	}
	if _, hasError := got["error"]; !hasError {
		t.Fatal(`expected an "error" key to be present (even if null) — {"data":...,"error":null} is the documented shape`)
	}
	if got["error"] != nil {
		t.Fatalf("expected error to be null on success, got %v", got["error"])
	}
	data, ok := got["data"].(map[string]any)
	if !ok || data["status"] != "ok" {
		t.Fatalf("expected data.status == ok, got %+v", got["data"])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
}

func TestWriteError_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, 404, "not found")

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response isn't valid JSON: %v", err)
	}
	if got["data"] != nil {
		t.Fatalf("expected data to be null on error, got %v", got["data"])
	}
	if got["error"] != "not found" {
		t.Fatalf("expected error message to round-trip, got %v", got["error"])
	}
	if rec.Code != 404 {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
}
