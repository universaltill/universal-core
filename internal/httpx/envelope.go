// Package httpx is the HTTP-layer glue shared across handlers: the
// {"data":…,"error":null} response envelope (CLAUDE.md's API-format
// rule) and request-scoped tenant/actor resolution. No SQL lives here
// (CLAUDE.md's repository-pattern rule) — this package only shapes HTTP
// in and out, it never talks to the database directly.
package httpx

import (
	"encoding/json"
	"net/http"
)

type envelope struct {
	Data  any     `json:"data"`
	Error *string `json:"error"`
}

// WriteJSON writes data wrapped in the standard envelope with status.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding errors here mean the response is already partially
	// written (WriteHeader already flushed the status line) — there is
	// nothing more useful to do with the error than what the client
	// already sees (a truncated/malformed body), so it's deliberately
	// not returned; every caller already logs failures at their own
	// layer if the underlying data/domain error is what actually matters.
	_ = json.NewEncoder(w).Encode(envelope{Data: data, Error: nil})
}

// WriteError writes msg as the envelope's error field with status, Data null.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Data: nil, Error: &msg})
}
