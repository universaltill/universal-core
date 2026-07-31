package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

var (
	errShort = errors.New("webauth: sealed value too short")
	errOpen  = errors.New("webauth: decrypt failed")
)

// Session is the identity persisted in the browser cookie after a
// successful OIDC login — the real counterpart to httpx.RequestContext
// (see bridge.go), sealed (authenticated encryption) so the client can
// neither read nor tamper with it. TenantID is already resolved (from
// the id_token's Zitadel org claim, via data.TenantRepo.GetByZitadelOrgID)
// at login time — every later request reads it straight out of this
// sealed cookie, no DB lookup per request.
type Session struct {
	Subject  string `json:"sub"`
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	TenantID string `json:"tenant_id"`
	// TenantIDs is every tenant this login may act in — the id_token's
	// project-roles claim resolved against tenants.zitadel_org_id once,
	// at sign-in (#25, ADR-0011). TenantID (the active tenant) is always
	// either empty (choice pending, see NeedsChoice) or an element of
	// this set; /ui/auth/switch re-seals the cookie with a different
	// element, never a value outside it. Omitted from the cookie for the
	// common single-tenant login (nil set + non-empty TenantID means
	// exactly one accessible tenant: the active one) to keep the sealed
	// cookie small.
	TenantIDs []string  `json:"tenant_ids,omitempty"`
	Expiry    time.Time `json:"exp"`
}

// AccessibleTenantIDs is the set a switcher may offer: TenantIDs when
// carried, else the single active tenant.
func (s *Session) AccessibleTenantIDs() []string {
	if s == nil {
		return nil
	}
	if len(s.TenantIDs) > 0 {
		return s.TenantIDs
	}
	if s.TenantID != "" {
		return []string{s.TenantID}
	}
	return nil
}

// CanAccess reports whether tenantID is in this session's accessible
// set.
func (s *Session) CanAccess(tenantID string) bool {
	if s == nil || tenantID == "" {
		return false
	}
	return slices.Contains(s.AccessibleTenantIDs(), tenantID)
}

// NeedsChoice reports a real, authenticated multi-tenant login that
// hasn't picked its active tenant yet — Valid() is deliberately false
// for it (downstream handlers must never see an empty TenantID), and
// Guard routes it to /ui/auth/choose instead of back to /ui/login.
func (s *Session) NeedsChoice() bool {
	return s != nil && s.Subject != "" && s.TenantID == "" &&
		len(s.TenantIDs) > 0 && time.Now().Before(s.Expiry)
}

// Valid reports whether the session is present, unexpired, and actually
// resolved to a tenant — a user authenticated by Zitadel but not yet
// linked to any Universal Core tenant (no Zitadel org grant maps to a
// tenants row) has a Subject but no TenantID, and must not be treated as
// a valid session: every downstream handler assumes TenantID is always
// safe to trust once FromContext succeeds (see CLAUDE.md's multi-tenancy
// rule), so an empty one is a login failure, not "log in with no
// tenant".
func (s *Session) Valid() bool {
	return s != nil && s.Subject != "" && s.TenantID != "" && time.Now().Before(s.Expiry)
}

// sealer seals/opens Session values with NaCl secretbox (XSalsa20-Poly1305)
// under a 32-byte key. A fresh random nonce is prepended to every
// ciphertext.
type sealer struct {
	key [32]byte
}

// newSealer decodes a base64 (std or url, with/without padding) 32-byte
// key.
func newSealer(keyB64 string) (*sealer, error) {
	raw, err := decodeKey(keyB64)
	if err != nil {
		return nil, fmt.Errorf("webauth: cookie key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("webauth: cookie key must decode to 32 bytes, got %d", len(raw))
	}
	s := &sealer{}
	copy(s.key[:], raw)
	return s, nil
}

func decodeKey(k string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(k); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not valid base64")
}

func (s *sealer) seal(sess *Session) (string, error) {
	plaintext, err := json.Marshal(sess)
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

func (s *sealer) open(value string) (*Session, error) {
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
	var sess Session
	if err := json.Unmarshal(plaintext, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}
