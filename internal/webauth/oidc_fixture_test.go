package webauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const testKeyID = "test-key"

// fakeOIDCProvider is a minimal real OIDC provider for tests — a
// discovery document, a JWKS endpoint, and a token endpoint — good
// enough for coreos/go-oidc's own provider discovery + real RS256
// signature verification to run against, unlike internal/svcauth's own
// fake (which only needs to fake token *introspection*, never verifies a
// JWT signature itself). Lets this package's New/handleCallback be
// tested against the real crypto path instead of only the pieces that
// don't need a live provider — this package's own doc comments used to
// say the full token exchange "needs a live Zitadel session" and was
// deliberately left to manual verification; this closes that gap.
type fakeOIDCProvider struct {
	*httptest.Server
	key      *rsa.PrivateKey
	signer   jose.Signer
	idToken  string // what the next /token call returns as id_token
	tokenErr int    // when nonzero, /token responds with this HTTP status instead
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", testKeyID),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	p := &fakeOIDCProvider{key: key, signer: signer}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/oauth/v2/authorize",
			"token_endpoint":                        p.URL + "/oauth/v2/token",
			"jwks_uri":                              p.URL + "/oauth/v2/keys",
			"end_session_endpoint":                  p.URL + "/oidc/v1/end_session",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/oauth/v2/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: testKeyID, Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		if p.tokenErr != 0 {
			w.WriteHeader(p.tokenErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     p.idToken,
		})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

// mintIDToken signs a set of claims with the provider's own key —
// callers supply iss/aud/exp/iat themselves via extra so each test can
// exercise a specific failure mode (wrong issuer, wrong audience,
// expired, wrong nonce, missing org claim, ...).
func (p *fakeOIDCProvider) mintIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	tok, err := jwt.Signed(p.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("mint id_token: %v", err)
	}
	return tok
}

// baseClaims returns a valid claim set for sub/aud/iss/exp/iat/nonce —
// the fields every test needs regardless of what else it's exercising.
func (p *fakeOIDCProvider) baseClaims(clientID, subject, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   p.URL,
		"aud":   clientID,
		"sub":   subject,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": nonce,
	}
}
