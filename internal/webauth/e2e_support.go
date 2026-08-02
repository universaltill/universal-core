package webauth

import (
	"testing"

	"github.com/universaltill/universal-core/internal/data"
)

// This file exists solely so internal/e2e can drive the tenant
// picker/switcher (choose.go, #25/ADR-0011) against a real, sealed
// session cookie in a real browser (universaltill/uc-infra#52).
// internal/webauth's own tests build an Authenticator directly against
// its unexported fields (see choose_test.go's chooseAuthenticator) —
// a different package's tests can't do that, so NewForE2ETests and
// SealSessionForE2ETests are the equivalent surface, exported. Never
// called by cmd/universal-core's real wiring (webauth.New, which always
// performs OIDC discovery against a live provider) — nothing outside
// another package's own tests has a reason to call these.

// NewForE2ETests builds an Authenticator with the picker/switcher routes
// enabled against cookieKeyB64 and tenants — the same two dependencies a
// real deployment's Config carries — without New's OIDC discovery call,
// so it needs no live provider to reach. Register still mounts
// /ui/login, /ui/auth/callback and /ui/logout (Enabled() reports true),
// but this Authenticator never performs a real OIDC login: a caller
// seeds a session directly with SealSessionForE2ETests instead of
// driving that flow, so a test using this must never navigate to those
// three routes — and Guard itself will send a request with a missing or
// expired session cookie to /ui/login regardless of what the caller
// intended to navigate to, which would panic on the nil a.oauth this
// constructor never sets up. testing.Testing() makes that mistake
// impossible to make outside a test binary in the first place, since
// this is exported, production-reachable API surface even though
// nothing in cmd/ or non-test code has a reason to call it.
func NewForE2ETests(cookieKeyB64 string, tenants *data.TenantRepo) (*Authenticator, error) {
	if !testing.Testing() {
		panic("webauth: NewForE2ETests must only be called from a test binary")
	}
	seal, err := newSealer(cookieKeyB64)
	if err != nil {
		return nil, err
	}
	return &Authenticator{sealer: seal, enabled: true, tenants: tenants}, nil
}

// SealSessionForE2ETests seals sess exactly as a real login would
// (writeSession) — internal/e2e uses the result to mint a cookie value
// a real browser can carry, set under SessionCookieName.
func (a *Authenticator) SealSessionForE2ETests(sess *Session) (string, error) {
	return a.sealer.seal(sess)
}

// SessionCookieName is the cookie name a sealed session must be set
// under (see writeSession's own Name: sessionCookie), exported so
// internal/e2e doesn't duplicate the literal.
const SessionCookieName = sessionCookie
