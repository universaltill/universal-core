package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestNewForE2ETests_RejectsBadKey(t *testing.T) {
	if _, err := NewForE2ETests("not valid base64!!", nil); err == nil {
		t.Fatal("expected an error for a malformed cookie key")
	}
}

// TestNewForE2ETests_BuildsAnEnabledAuthenticatorSealingRealSessions
// proves the constructor internal/e2e actually depends on: an enabled
// Authenticator whose SealSessionForE2ETests round-trips through the
// same sealer.open the real login/picker/switcher paths use.
func TestNewForE2ETests_BuildsAnEnabledAuthenticatorSealingRealSessions(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	a, err := NewForE2ETests(base64.StdEncoding.EncodeToString(key[:]), nil)
	if err != nil {
		t.Fatalf("NewForE2ETests: %v", err)
	}
	if !a.Enabled() {
		t.Fatal("expected an enabled Authenticator")
	}
	sess := &Session{Subject: "u1", TenantID: "t1", Expiry: time.Now().Add(time.Hour)}
	sealed, err := a.SealSessionForE2ETests(sess)
	if err != nil {
		t.Fatalf("SealSessionForE2ETests: %v", err)
	}
	opened, err := a.sealer.open(sealed)
	if err != nil {
		t.Fatalf("open the sealed session: %v", err)
	}
	if opened.Subject != "u1" || opened.TenantID != "t1" {
		t.Fatalf("round-trip mismatch: %+v", opened)
	}
}

func TestSessionCookieName_MatchesRealSessionCookie(t *testing.T) {
	if SessionCookieName != sessionCookie {
		t.Fatalf("SessionCookieName = %q, want %q", SessionCookieName, sessionCookie)
	}
}
