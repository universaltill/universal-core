package httpx

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// withDevAuthEnabled sets INSECURE_DEV_AUTH for the duration of a test
// and restores whatever it was before — never leaks across tests, and
// never assumes the ambient environment's value.
func withDevAuthEnabled(t *testing.T, enabled bool) {
	t.Helper()
	prev, had := os.LookupEnv("INSECURE_DEV_AUTH")
	if enabled {
		os.Setenv("INSECURE_DEV_AUTH", "true")
	} else {
		os.Unsetenv("INSECURE_DEV_AUTH")
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("INSECURE_DEV_AUTH", prev)
		} else {
			os.Unsetenv("INSECURE_DEV_AUTH")
		}
	})
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := FromContext(r.Context())
		if !ok {
			WriteError(w, 500, "no request context")
			return
		}
		WriteJSON(w, 200, map[string]string{"tenant_id": rc.TenantID, "actor_id": rc.Actor.ID})
	})
}

// TestDevAuth_FailsClosedWhenDisabled is the load-bearing test for the
// whole stopgap's safety property: with INSECURE_DEV_AUTH unset (the
// default — a deployment that never sets it), every request must 401,
// never fall through to the handler regardless of what headers a caller
// sends.
func TestDevAuth_FailsClosedWhenDisabled(t *testing.T) {
	withDevAuthEnabled(t, false)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "tenant-1")
	req.Header.Set("X-Actor-ID", "actor-1")
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when INSECURE_DEV_AUTH is unset (even with valid-looking headers), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDevAuth_RejectsMissingHeaders(t *testing.T) {
	withDevAuthEnabled(t, true)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing X-Tenant-ID/X-Actor-ID, got %d", rec.Code)
	}
}

const testTenantID = "11111111-1111-1111-1111-111111111111"

func TestDevAuth_PopulatesRequestContextWhenEnabled(t *testing.T) {
	withDevAuthEnabled(t, true)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", testTenantID)
	req.Header.Set("X-Actor-ID", "actor-1")
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tenant_id":"`+testTenantID+`"`) || !strings.Contains(rec.Body.String(), `"actor_id":"actor-1"`) {
		t.Fatalf("expected the handler to see the headers via RequestContext, got %s", rec.Body.String())
	}
}

// TestDevAuth_RejectsMalformedTenantID is the regression test for the
// code-review finding that a non-UUID X-Tenant-ID reached a query as a
// malformed parameter, surfacing as a 500 with a raw Postgres driver
// error in the response. It's now rejected here, as a 401, before ever
// reaching a handler or a query.
func TestDevAuth_RejectsMalformedTenantID(t *testing.T) {
	withDevAuthEnabled(t, true)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "not-a-uuid")
	req.Header.Set("X-Actor-ID", "actor-1")
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a malformed X-Tenant-ID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDevAuth_PassesThroughWhenAlreadyAuthenticated is the regression
// test for the composability property internal/webauth's Guard depends
// on: a request that already carries a RequestContext (a preceding real-
// auth middleware already verified it) must reach the handler unchanged
// — DevAuth doesn't re-check headers, doesn't require INSECURE_DEV_AUTH
// to be set, and doesn't overwrite the already-attached context. This is
// what makes DevAuth's own headers genuinely inert once real login is
// configured for a deployment (Guard either populates the context first
// or redirects before DevAuth ever runs).
func TestDevAuth_PassesThroughWhenAlreadyAuthenticated(t *testing.T) {
	withDevAuthEnabled(t, false) // deliberately disabled — must not matter
	req := httptest.NewRequest("GET", "/", nil)
	ctx := WithRequestContext(req.Context(), RequestContext{
		TenantID: testTenantID,
		Actor:    audit.Actor{Type: audit.ActorHuman, ID: "real-user"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (pass-through), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"actor_id":"real-user"`) {
		t.Fatalf("expected the pre-existing RequestContext to survive unchanged, got %s", rec.Body.String())
	}
}

// TestDevAuth_TrimsWhitespaceOnlyHeaders confirms a whitespace-only
// header is treated the same as a missing one, not as a non-empty
// (garbage) tenant/actor id.
func TestDevAuth_TrimsWhitespaceOnlyHeaders(t *testing.T) {
	withDevAuthEnabled(t, true)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "   ")
	req.Header.Set("X-Actor-ID", "actor-1")
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a whitespace-only X-Tenant-ID, got %d: %s", rec.Code, rec.Body.String())
	}
}
