package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// helpFixtureIndex builds a small, genuine-looking fixture Index (two
// topics: an entity-typed one authz can gate, and a route-typed one it
// can't) — real content authored for this test, not the production
// manual (uc-infra#147-152 owns that; out of scope here per ADR-0023).
func helpFixtureIndex(t *testing.T) *help.Index {
	t.Helper()
	fsys := fstest.MapFS{
		"content/en/entity/Item.md": &fstest.MapFile{Data: []byte(
			"---\ntitle: Item Help\naudience: user\nmodule: inventory\norder: 1\n---\n# Item Help\n\nHow to manage warehouse items.\n")},
		"content/en/route/reports.md": &fstest.MapFile{Data: []byte(
			"---\ntitle: Reports Help\naudience: admin\nmodule: reporting\norder: 1\n---\n# Reports Help\n\nAdmin-only reporting guidance.\n")},
	}
	idx, err := help.BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("help.BuildIndexFrom fixture: %v", err)
	}
	return idx
}

// withHelpFixtureIndex points the package-level help.GetIndex() at the
// fixture above for the duration of one test, restoring the (empty,
// real) production index on cleanup — help.Index's own
// SetIndexForTesting seam (internal/help/index.go), exercised here the
// same way internal/e2e will exercise it for real-browser tests.
func withHelpFixtureIndex(t *testing.T) {
	t.Helper()
	restore := help.SetIndexForTesting(helpFixtureIndex(t))
	t.Cleanup(restore)
}

func TestAPI_HelpIndex_ListsVisibleTopics(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// No RBAC configured for this tenant at all — authz.Resolver.CanRead
	// defaults to true for everyone until a Permission row exists
	// (rulesExist == false), so every topic is visible.
	req := newRequest("GET", "/help", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Item Help") {
		t.Errorf("expected the entity/Item topic to be listed, got:\n%s", body)
	}
	if !strings.Contains(body, "Reports Help") {
		t.Errorf("expected the route/reports topic to be listed, got:\n%s", body)
	}
	if !strings.Contains(body, "uc-help-topic-admin") {
		t.Errorf("expected the admin-audience topic to carry the admin-badge class (marked, not restricted), got:\n%s", body)
	}
	if !strings.Contains(body, `id="uc-help-topics"`) || !strings.Contains(body, `id="uc-help-search"`) || !strings.Contains(body, `id="uc-help-detail"`) {
		t.Errorf("expected the stable uc-help-topics/uc-help-search/uc-help-detail ids, got:\n%s", body)
	}
}

// TestAPI_HelpIndex_OwnAffordanceEnabledWithRealContent (uc-infra#243):
// the manual's own "?" affordance (next to its own <h1>, wired to
// help.RouteTopicID("help")) is a real, enabled link now that
// internal/help/content/{locale}/route/help.md ships for real.
//
// Deliberately uses withHelpFixtureIndex, not the real embedded index —
// this test only needs GET /help to succeed and list some topics; it is
// NOT what makes the affordance enabled. help.HasContent (help.go) reads
// the real go:embed'd content tree directly, independent of whatever
// help.SetIndexForTesting has swapped GetIndex() to return (that seam
// only affects topic *listing*/authz, not this per-topic existence
// check) — so the affordance reflects real shipped content even under a
// fixture index, the same way every other route page's affordance
// already does in production once its own topic ships.
func TestAPI_HelpIndex_OwnAffordanceEnabledWithRealContent(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `uc-help-affordance" role="link" data-help-topic="route/help" href="/help/route/help"`) {
		t.Errorf("expected an enabled affordance linking to /help/route/help, got:\n%s", body)
	}
	if strings.Contains(body, `data-help-topic="route/help" aria-disabled="true"`) {
		t.Errorf("expected the affordance NOT disabled, since route/help content really ships now, got:\n%s", body)
	}
}

func TestAPI_HelpIndex_NoTopicSelectedShowsEmptyState(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "How to manage warehouse items") {
		t.Errorf("expected no topic body rendered when nothing is selected, got:\n%s", rec.Body.String())
	}
}

// TestAPI_HelpTopic_EmptyBodyTopicStillRendersAsSelected (independent
// review, uc-infra#144): the right pane used to gate on {{if
// .DetailHTML}}, a plain Go-template truthiness check — so a topic with
// real, valid front matter but an empty body (template.HTML("")) fell
// through to the "select a topic" empty state exactly as if nothing had
// been clicked at all, silently swallowing its own <h2> title. Clicking
// the topic looked like it did nothing.
func TestAPI_HelpTopic_EmptyBodyTopicStillRendersAsSelected(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	fsys := fstest.MapFS{
		"content/en/route/empty.md": &fstest.MapFile{Data: []byte(
			"---\ntitle: Empty Topic\naudience: user\nmodule: general\norder: 1\n---\n")},
	}
	idx, err := help.BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("help.BuildIndexFrom fixture: %v", err)
	}
	t.Cleanup(help.SetIndexForTesting(idx))
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/route/empty", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/route/empty = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Empty Topic") {
		t.Errorf("expected the selected topic's own title to render even though its body is empty, got:\n%s", body)
	}
	if strings.Contains(body, "uc-help-empty-state") {
		t.Errorf("expected the real detail panel, not the no-topic-selected empty state, got:\n%s", body)
	}
}

func TestAPI_HelpTopic_VisibleTopicRenders(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/entity/Item", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/entity/Item = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "How to manage warehouse items") {
		t.Errorf("expected the selected topic's rendered body, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "uc-help-topic-active") {
		t.Errorf("expected the selected topic's row to carry the active class, got:\n%s", rec.Body.String())
	}
}

// TestAPI_HelpTopic_SearchInputCarriesSelectedTopic (independent review,
// uc-infra#144): the left-pane search box's hx-get swaps only
// #uc-help-topic-results, which is re-rendered from scratch server-side
// — so unless the request tells the server which topic is currently
// open, buildHelpTopicListData has no way to know and the "active" row
// highlight silently disappears the instant a viewer types one
// character, even though the right pane still shows that same topic's
// content. The fix threads the currently-selected topic id through as
// an hx-vals payload the search input actually sends.
func TestAPI_HelpTopic_SearchInputCarriesSelectedTopic(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/entity/Item", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/entity/Item = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-vals`) || !strings.Contains(body, `"selected"`) || !strings.Contains(body, `entity/Item`) {
		t.Errorf("expected the search input's hx-vals to carry the currently-selected topic id (entity/Item) so a live search doesn't drop the active-row highlight, got:\n%s", body)
	}
}

func TestAPI_HelpTopic_UnknownTopicIs404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/entity/DoesNotExist", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /help/entity/DoesNotExist = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// A real HTML 404 page inside the normal shell, not the bare
	// {"data":null,"error":...} envelope (ADR-0023/uc-infra#144).
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("expected a real HTML not-found page, not the bare JSON error envelope, got:\n%s", rec.Body.String())
	}
}

// TestAPI_HelpTopic_DeniedTopicIs404NotVisible (ADR-0023's authz
// acceptance criterion): a viewer without read access to Item must
// neither see entity/Item in the topic list NOR be able to reach its
// content by guessing the URL — 404, not 403, so the response doesn't
// even reveal the topic exists.
func TestAPI_HelpTopic_DeniedTopicIs404NotVisible(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	seedEntityPermission(t, db, "restricted", "user-restricted", "Item", false, false)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Hidden from the list.
	listReq := newRequest("GET", "/help", tenantID, "user-restricted", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /help (restricted actor) = %d: %s", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), "Item Help") {
		t.Errorf("expected the Item topic hidden from an actor without read access to Item, got:\n%s", listRec.Body.String())
	}
	// The route/ topic is never authz-filtered — still visible.
	if !strings.Contains(listRec.Body.String(), "Reports Help") {
		t.Errorf("expected the route/reports topic (never authz-filtered) to remain visible, got:\n%s", listRec.Body.String())
	}

	// 404, not 403, when requested directly by URL.
	topicReq := newRequest("GET", "/help/entity/Item", tenantID, "user-restricted", nil)
	topicRec := httptest.NewRecorder()
	mux.ServeHTTP(topicRec, topicReq)
	if topicRec.Code != http.StatusNotFound {
		t.Fatalf("GET /help/entity/Item (denied actor) = %d, want 404: %s", topicRec.Code, topicRec.Body.String())
	}
	if strings.Contains(topicRec.Body.String(), "How to manage warehouse items") {
		t.Errorf("expected the denied topic's content to never render, got:\n%s", topicRec.Body.String())
	}
}

func TestAPI_HelpSearch_ReturnsFilteredFragment(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/search?q=warehouse", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/search?q=warehouse = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Item Help") {
		t.Errorf("expected the Item topic (body mentions warehouse) in the search fragment, got:\n%s", body)
	}
	if strings.Contains(body, "Reports Help") {
		t.Errorf("expected the non-matching Reports topic excluded, got:\n%s", body)
	}
	// A bare left-pane fragment, not a full shelled page.
	if strings.Contains(body, "<html") || strings.Contains(body, `id="uc-help-search"`) || strings.Contains(body, `id="uc-help-detail"`) {
		t.Errorf("expected only the topic-list fragment, not a full page, got:\n%s", body)
	}
}

func TestAPI_HelpSearch_EmptyQueryShowsFullList(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/search", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/search (no q) = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Item Help") || !strings.Contains(body, "Reports Help") {
		t.Errorf("expected an empty query to show every visible topic, got:\n%s", body)
	}
}

func TestAPI_HelpSearch_NoMatchesShowsEmptyMessage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/search?q=zzz-nothing-matches", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/search?q=zzz = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "Item Help") || strings.Contains(body, "Reports Help") {
		t.Errorf("expected no topics listed for a non-matching query, got:\n%s", body)
	}
	if !strings.Contains(body, "uc-help-empty") {
		t.Errorf("expected the empty-results message, got:\n%s", body)
	}
}

func TestAPI_HelpSearch_AuthzFiltersResultsToo(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	seedEntityPermission(t, db, "restricted", "user-restricted", "Item", false, false)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/search?q=item", tenantID, "user-restricted", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/search?q=item = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Item Help") {
		t.Errorf("expected the denied topic excluded from search results too, got:\n%s", rec.Body.String())
	}
}

// TestAPI_HelpSearchRouteWinsOverTopicWildcard confirms Go's ServeMux
// actually resolves GET /help/search to helpSearchFragment, not
// renderHelpTopic with topicID="search" — the exact ambiguity
// registering both "/help/search" and "/help/{topicID...}" invites, and
// the one net/http's own "more specific pattern wins" rule is relied on
// (not merely assumed) to resolve.
func TestAPI_HelpSearchRouteWinsOverTopicWildcard(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	withHelpFixtureIndex(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/help/search", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/search = %d, want 200 from helpSearchFragment (would 404 if it fell through to renderHelpTopic with topicID=%q): %s", rec.Code, "search", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<h1") {
		t.Errorf("expected the bare search fragment (no page shell/heading), got the full page instead:\n%s", rec.Body.String())
	}
}
