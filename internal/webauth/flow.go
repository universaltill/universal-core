package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

// flowState is the short-lived per-login state carried across the
// redirect to Zitadel and back, sealed into a cookie so the client
// cannot read or forge it. It ties the callback to this browser
// (State), binds the id_token to this flow (Nonce), carries the PKCE
// verifier, and remembers where to send the user afterwards.
type flowState struct {
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	ReturnTo string    `json:"r"`
	Expiry   time.Time `json:"e"`
}

func (s *sealer) sealFlow(fs *flowState) (string, error) {
	plaintext, err := json.Marshal(fs)
	if err != nil {
		return "", err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nonce[:], plaintext, &nonce, &s.key)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *sealer) openFlow(value string) (*flowState, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 24 {
		return nil, errShort
	}
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	plaintext, ok := secretbox.Open(nil, sealed[24:], &nonce, &s.key)
	if !ok {
		return nil, errOpen
	}
	var fs flowState
	if err := json.Unmarshal(plaintext, &fs); err != nil {
		return nil, err
	}
	return &fs, nil
}

// randToken returns a URL-safe 256-bit random string for state/nonce
// values.
func randToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
