package httpx

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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

func TestDevAuth_PopulatesRequestContextWhenEnabled(t *testing.T) {
	withDevAuthEnabled(t, true)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "tenant-1")
	req.Header.Set("X-Actor-ID", "actor-1")
	rec := httptest.NewRecorder()

	DevAuth(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tenant_id":"tenant-1"`) || !strings.Contains(rec.Body.String(), `"actor_id":"actor-1"`) {
		t.Fatalf("expected the handler to see the headers via RequestContext, got %s", rec.Body.String())
	}
}
