// Handler-level tests for path-prefix tenancy (/t/{slug}/…,
// universal-core#25, ADR-0011 — tenanturl.go): the /t/ subtree's
// resolve/cross-check/re-dispatch, guardTenantHeader's X-UC-Tenant
// cross-check, and canonicalPage's webauth-only gate. All through
// DevAuth (X-Tenant-ID/X-Actor-ID) against a real mux, mirroring
// members_test.go's harness shape: the control *sql.DB is built inline
// so data.NewTenantRepo can wrap the same database the router does. The
// webauth-session paths (auto-switch on GET, the canonical redirect
// itself) are internal/e2e's territory, not faked here.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// tenantURLHarness: two provisioned tenants (same name, so their slugs
// exercise the -2 uniqueness suffix in passing) with the foundation
// published in the first, and a Handler whose SetTenantRepo is wired —
// without which the whole /t/ subtree 404s unconditionally.
type tenantURLHarness struct {
	t         *testing.T
	mux       *http.ServeMux
	tenantID  string
	slug      string
	tenant2ID string
	slug2     string
}

func newTenantURLHarness(t *testing.T) *tenantURLHarness {
	t.Helper()
	ctx := context.Background()

	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	if err := foundation.Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	// Same fixture name as the first tenant — newTestTenant always
	// provisions "Test Tenant", so this one's slug lands on the -2 suffix.
	tenant2ID, _ := newTestTenant(t, router)

	h := testHandler(t, router)
	tenants := data.NewTenantRepo(control)
	h.SetTenantRepo(tenants)
	mux := http.NewServeMux()
	h.Routes(mux)

	slug, err := tenants.SlugByID(ctx, tenantID)
	if err != nil {
		t.Fatalf("SlugByID(first tenant): %v", err)
	}
	slug2, err := tenants.SlugByID(ctx, tenant2ID)
	if err != nil {
		t.Fatalf("SlugByID(second tenant): %v", err)
	}
	if slug == slug2 {
		t.Fatalf("expected distinct slugs for the two tenants, both got %q", slug)
	}

	return &tenantURLHarness{
		t: t, mux: mux,
		tenantID: tenantID, slug: slug,
		tenant2ID: tenant2ID, slug2: slug2,
	}
}

func (h *tenantURLHarness) do(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func partyBody() []byte {
	return []byte(`{"party_type":"organization","name":"Acme Textiles"}`)
}

// errorEnvelope decodes the standard {"data":…,"error":…} JSON envelope
// and returns the error string ("" when null).
func errorEnvelope(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Data  any     `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal error envelope from %q: %v", body, err)
	}
	if env.Error == nil {
		return ""
	}
	return *env.Error
}

// TestTenantPrefix_MatchingSlug_RedispatchesToSameResponse: a /t/{slug}/
// request whose slug names the session's own tenant strips the prefix
// and re-dispatches into the same mux — byte-identical JSON to the
// un-prefixed route, including a real record created beforehand.
func TestTenantPrefix_MatchingSlug_RedispatchesToSameResponse(t *testing.T) {
	h := newTenantURLHarness(t)

	created := h.do(newRequest("POST", "/api/records/Party", h.tenantID, "farshid", partyBody()))
	if created.Code != http.StatusCreated {
		t.Fatalf("create Party: expected 201, got %d: %s", created.Code, created.Body.String())
	}

	plain := h.do(newRequest("GET", "/api/records/Party", h.tenantID, "farshid", nil))
	if plain.Code != http.StatusOK {
		t.Fatalf("un-prefixed list: expected 200, got %d: %s", plain.Code, plain.Body.String())
	}
	prefixed := h.do(newRequest("GET", "/t/"+h.slug+"/api/records/Party", h.tenantID, "farshid", nil))
	if prefixed.Code != http.StatusOK {
		t.Fatalf("prefixed list: expected 200, got %d: %s", prefixed.Code, prefixed.Body.String())
	}
	if prefixed.Body.String() != plain.Body.String() {
		t.Fatalf("prefixed and un-prefixed responses differ:\nprefixed:   %s\nun-prefixed: %s",
			prefixed.Body.String(), plain.Body.String())
	}
	if !strings.Contains(prefixed.Body.String(), "Acme Textiles") {
		t.Fatalf("expected the created record in the prefixed response, got %s", prefixed.Body.String())
	}
	if ct := prefixed.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected a JSON content type, got %q", ct)
	}
}

// TestTenantPrefix_UnknownSlug_NotFound: a slug no tenant owns 404s.
func TestTenantPrefix_UnknownSlug_NotFound(t *testing.T) {
	h := newTenantURLHarness(t)

	rec := h.do(newRequest("GET", "/t/unknown-slug/api/records/Party", h.tenantID, "farshid", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown slug, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTenantPrefix_OtherTenantsSlug_GetIs404: a GET for another REAL
// tenant's slug, with no webauth session to auto-switch, must be
// indistinguishable from a slug that doesn't exist — the URL space must
// not confirm which tenants exist (ADR-0011).
func TestTenantPrefix_OtherTenantsSlug_GetIs404(t *testing.T) {
	h := newTenantURLHarness(t)

	other := h.do(newRequest("GET", "/t/"+h.slug2+"/api/records/Party", h.tenantID, "farshid", nil))
	if other.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's slug, got %d: %s", other.Code, other.Body.String())
	}
	unknown := h.do(newRequest("GET", "/t/unknown-slug/api/records/Party", h.tenantID, "farshid", nil))
	if other.Code != unknown.Code || other.Body.String() != unknown.Body.String() {
		t.Fatalf("existing-but-inaccessible slug must be indistinguishable from an unknown one:\nother:   %d %s\nunknown: %d %s",
			other.Code, other.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// TestTenantPrefix_OtherTenantsSlug_PostIs409: a mutation whose URL
// names a different tenant than the session is refused with the
// localized conflict envelope — never silently applied cross-tenant,
// and never a 404 (a non-GET has no auto-switch to hide behind).
func TestTenantPrefix_OtherTenantsSlug_PostIs409(t *testing.T) {
	h := newTenantURLHarness(t)

	rec := h.do(newRequest("POST", "/t/"+h.slug2+"/api/records/Party", h.tenantID, "farshid", partyBody()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a cross-tenant POST, got %d: %s", rec.Code, rec.Body.String())
	}
	msg := errorEnvelope(t, rec.Body.Bytes())
	if !strings.Contains(msg, "different tenant") {
		t.Fatalf("expected the tenanturl.mismatch text mentioning \"different tenant\", got %q", msg)
	}

	// An UNKNOWN slug on a non-GET gets the exact same 409, not a 404 —
	// otherwise the status split is a tenant-name oracle (slugs derive
	// from customer names; independent review, 2026-07-31, caught the
	// original 404 here leaking exactly that).
	unknown := h.do(newRequest("POST", "/t/no-such-tenant/api/records/Party", h.tenantID, "farshid", partyBody()))
	if unknown.Code != rec.Code || unknown.Body.String() != rec.Body.String() {
		t.Fatalf("non-GET unknown slug must be indistinguishable from an inaccessible one:\ninaccessible: %d %s\nunknown:      %d %s",
			rec.Code, rec.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// TestGuardTenantHeader_CrossChecksClaim: X-UC-Tenant (the page's
// stamp, layout.go) equal to the resolved tenant passes untouched; a
// different value is refused 409 before the mutation runs; no header
// means no claim — identical outcome to the matching case.
func TestGuardTenantHeader_CrossChecksClaim(t *testing.T) {
	h := newTenantURLHarness(t)

	post := func(header string) *httptest.ResponseRecorder {
		req := newRequest("POST", "/api/records/Party", h.tenantID, "farshid", partyBody())
		if header != "" {
			req.Header.Set("X-UC-Tenant", header)
		}
		return h.do(req)
	}

	noHeader := post("")
	if noHeader.Code != http.StatusCreated {
		t.Fatalf("no header: expected 201, got %d: %s", noHeader.Code, noHeader.Body.String())
	}

	matching := post(h.tenantID)
	if matching.Code == http.StatusConflict {
		t.Fatalf("matching header must not 409: %s", matching.Body.String())
	}
	if matching.Code != noHeader.Code {
		t.Fatalf("matching header must behave like no header: got %d, want %d: %s",
			matching.Code, noHeader.Code, matching.Body.String())
	}

	mismatched := post(h.tenant2ID)
	if mismatched.Code != http.StatusConflict {
		t.Fatalf("mismatched header: expected 409, got %d: %s", mismatched.Code, mismatched.Body.String())
	}
	msg := errorEnvelope(t, mismatched.Body.Bytes())
	if !strings.Contains(msg, "different tenant") {
		t.Fatalf("expected the tenanturl.mismatch text mentioning \"different tenant\", got %q", msg)
	}
}

// TestCanonicalPage_DevAuthGetIsNotRedirected: canonicalization is
// gated on a REAL webauth session (h.auth.Enabled()) — DevAuth header
// traffic keeps its un-prefixed contract, so a page GET renders 200
// where a browser session would get the 302 to /t/{slug}/…
// (internal/e2e covers that side).
func TestCanonicalPage_DevAuthGetIsNotRedirected(t *testing.T) {
	h := newTenantURLHarness(t)

	rec := h.do(newRequest("GET", "/records/Party", h.tenantID, "farshid", nil))
	if rec.Code == http.StatusFound || rec.Code == http.StatusMovedPermanently {
		t.Fatalf("DevAuth page GET must not be canonicalized, got %d -> %q",
			rec.Code, rec.Header().Get("Location"))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for the un-prefixed page, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("expected no Location header, got %q", loc)
	}
}

// TestTenantPrefix_NoTrailingSlash_Redirects: "/t/{slug}/" is a
// subtree pattern, so Go's ServeMux itself redirects the slash-less
// form onto it — pinned so a future route reshuffle doesn't silently
// turn /t/{slug} into a 404. The status is the mux's own choice and
// changed across toolchains (301 through Go 1.25, 307 from Go 1.26 —
// this machine's go1.26.5 emits 307), so either permanent-ish redirect
// is accepted; the Location is what actually matters.
func TestTenantPrefix_NoTrailingSlash_Redirects(t *testing.T) {
	h := newTenantURLHarness(t)

	rec := h.do(newRequest("GET", "/t/"+h.slug, h.tenantID, "farshid", nil))
	if rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected a 301/307 redirect for /t/{slug} without trailing slash, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/t/"+h.slug+"/" {
		t.Fatalf("expected redirect to %q, got %q", "/t/"+h.slug+"/", loc)
	}
}
