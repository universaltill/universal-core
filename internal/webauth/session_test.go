package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

func testSealer(t *testing.T) *sealer {
	t.Helper()
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	s, err := newSealer(base64.StdEncoding.EncodeToString(key[:]))
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	return s
}

func TestSessionSealRoundTrip(t *testing.T) {
	s := testSealer(t)
	in := &Session{Subject: "u1", Name: "Ada", Email: "a@x.com",
		TenantID: "tenant-1", Expiry: time.Now().Add(time.Hour)}
	sealed, err := s.seal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if out.Subject != "u1" || out.Email != "a@x.com" || out.TenantID != "tenant-1" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	if !out.Valid() {
		t.Fatal("expected valid session")
	}
}

func TestSessionTamperRejected(t *testing.T) {
	s := testSealer(t)
	sealed, _ := s.seal(&Session{Subject: "u1", TenantID: "t1", Expiry: time.Now().Add(time.Hour)})
	raw, _ := base64.RawURLEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 0xFF // flip a ciphertext bit
	if _, err := s.open(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered session must not open")
	}
	// A different key must not open it either.
	other := testSealer(t)
	if _, err := other.open(sealed); err == nil {
		t.Fatal("session sealed with a different key must not open")
	}
}

func TestSessionExpired(t *testing.T) {
	s := testSealer(t)
	sealed, _ := s.seal(&Session{Subject: "u1", TenantID: "t1", Expiry: time.Now().Add(-time.Minute)})
	out, err := s.open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if out.Valid() {
		t.Fatal("expired session must be invalid")
	}
}

// TestSessionWithoutTenantIDIsInvalid is the Universal-Core-specific
// counterpart to ut-cloud's own Session.Valid — a real, successfully
// authenticated Zitadel user with no tenant link resolved has a Subject
// but no TenantID, and must never be treated as a usable session:
// internal/api's handlers trust FromContext's TenantID unconditionally
// once webauth.Guard lets a request through (CLAUDE.md's multi-tenancy
// rule), so this can't be "logged in with no tenant" — it has to be "not
// logged in".
func TestSessionWithoutTenantIDIsInvalid(t *testing.T) {
	s := testSealer(t)
	sealed, _ := s.seal(&Session{Subject: "u1", TenantID: "", Expiry: time.Now().Add(time.Hour)})
	out, err := s.open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if out.Valid() {
		t.Fatal("a session with no resolved tenant must be invalid")
	}
}

func TestOpen_RejectsGarbageInput(t *testing.T) {
	s := testSealer(t)
	if _, err := s.open("not valid base64!!"); err == nil {
		t.Fatal("expected an error for invalid base64")
	}
	if _, err := s.open(base64.RawURLEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected an error for input shorter than the nonce")
	}
}

func TestNewSealer_RejectsWrongLengthKey(t *testing.T) {
	if _, err := newSealer(base64.StdEncoding.EncodeToString([]byte("too-short"))); err == nil {
		t.Fatal("expected an error for a key that doesn't decode to 32 bytes")
	}
}

func TestNewSealer_AcceptsAnySupportedBase64Variant(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	for name, enc := range map[string]*base64.Encoding{
		"std": base64.StdEncoding, "raw-std": base64.RawStdEncoding,
		"url": base64.URLEncoding, "raw-url": base64.RawURLEncoding,
	} {
		if _, err := newSealer(enc.EncodeToString(key[:])); err != nil {
			t.Errorf("%s: expected newSealer to accept this encoding, got %v", name, err)
		}
	}
}

// TestOpen_RejectsCiphertextThatDecryptsToInvalidJSON drives the
// decrypt-succeeds-but-unmarshal-fails branch directly — sealing valid
// JSON through the normal seal() path can never produce this, so this
// bypasses it and seals garbage plaintext with the real key/nonce
// mechanics instead.
func TestOpen_RejectsCiphertextThatDecryptsToInvalidJSON(t *testing.T) {
	s := testSealer(t)
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	sealed := secretbox.Seal(nonce[:], []byte("not json"), &nonce, &s.key)
	if _, err := s.open(base64.RawURLEncoding.EncodeToString(sealed)); err == nil {
		t.Fatal("expected an error for ciphertext that decrypts to invalid JSON")
	}
}
