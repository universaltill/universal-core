package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

// TestFlowStateSealRoundTrip mirrors TestSessionSealRoundTrip
// (session_test.go) for the OIDC flow cookie — the other value this
// package seals, carrying the PKCE verifier/state/nonce across the
// redirect to Zitadel and back.
func TestFlowStateSealRoundTrip(t *testing.T) {
	s := testSealer(t)
	in := &flowState{
		State: "state-1", Nonce: "nonce-1", Verifier: "verifier-1",
		ReturnTo: "/forms/Party/new", Expiry: time.Now().Add(10 * time.Minute),
	}
	sealed, err := s.sealFlow(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.openFlow(sealed)
	if err != nil {
		t.Fatalf("openFlow: %v", err)
	}
	if out.State != "state-1" || out.Nonce != "nonce-1" || out.Verifier != "verifier-1" || out.ReturnTo != "/forms/Party/new" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestFlowStateTamperRejected(t *testing.T) {
	s := testSealer(t)
	sealed, _ := s.sealFlow(&flowState{State: "s", Nonce: "n", Verifier: "v", Expiry: time.Now().Add(time.Minute)})
	raw, _ := base64.RawURLEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 0xFF
	if _, err := s.openFlow(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered flow state must not open")
	}
	other := testSealer(t)
	if _, err := other.openFlow(sealed); err == nil {
		t.Fatal("flow state sealed with a different key must not open")
	}
}

func TestOpenFlow_RejectsGarbageInput(t *testing.T) {
	s := testSealer(t)
	if _, err := s.openFlow("not valid base64!!"); err == nil {
		t.Fatal("expected an error for invalid base64")
	}
	// Valid base64 but shorter than the 24-byte nonce prefix.
	if _, err := s.openFlow(base64.RawURLEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected an error for input shorter than the nonce")
	}
}

// TestOpenFlow_RejectsCiphertextThatDecryptsToInvalidJSON mirrors
// session_test.go's equivalent test — the decrypt-succeeds-but-
// unmarshal-fails branch openFlow shares the same shape for.
func TestOpenFlow_RejectsCiphertextThatDecryptsToInvalidJSON(t *testing.T) {
	s := testSealer(t)
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	sealed := secretbox.Seal(nonce[:], []byte("not json"), &nonce, &s.key)
	if _, err := s.openFlow(base64.RawURLEncoding.EncodeToString(sealed)); err == nil {
		t.Fatal("expected an error for ciphertext that decrypts to invalid JSON")
	}
}

// TestRandToken confirms randToken produces distinct, correctly-sized
// (32 raw bytes, base64url-encoded) values each call — the property
// State/Nonce actually rely on for CSRF/replay protection.
func TestRandToken(t *testing.T) {
	a, b := randToken(), randToken()
	if a == b {
		t.Fatal("expected two calls to produce different tokens")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("expected valid base64url, got error: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("expected 32 raw bytes, got %d", len(raw))
	}
}
