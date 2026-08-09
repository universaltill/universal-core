package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

func widgetFilterEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Widget", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "secret", Type: entity.FieldString},
		},
	}
}

// TestListPage_FilterAndSort drives the rendered list page: a filter box,
// sortable headers, and both preserved across each other's links.
func TestListPage_FilterAndSort(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, widgetFilterEntityDef(), &form.Definition{
		EntityType: "Widget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "secret"}}}},
	})

	eng := crud.NewEngine(db)
	for _, n := range []string{"Apple", "Banana", "Cherry"} {
		if _, err := eng.Create(ctx, widgetFilterEntityDef(), map[string]any{"name": n, "secret": "s"}, humanActor()); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// The page renders a filter box and sortable headers.
	body := getAs(t, mux, "/records/Widget", tenantID, "anyone").Body.String()
	if !strings.Contains(body, `class="uc-list-filter"`) {
		t.Fatalf("no filter box on the list page:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-sort"`) {
		t.Fatalf("column headers are not sort links:\n%s", body)
	}

	// Filtering to "an" matches Banana only.
	filtered := getAs(t, mux, "/records/Widget?filter=name&q=an", tenantID, "anyone").Body.String()
	if !strings.Contains(filtered, "Banana") || strings.Contains(filtered, "Apple") || strings.Contains(filtered, "Cherry") {
		t.Fatalf("filter=name q=an should show only Banana:\n%s", filtered)
	}

	// Sorting descending puts Cherry first (its row link appears before others).
	desc := getAs(t, mux, "/records/Widget?sort=name&dir=desc", tenantID, "anyone").Body.String()
	ci := strings.Index(desc, "Cherry")
	ai := strings.Index(desc, "Apple")
	if ci == -1 || ai == -1 || ci > ai {
		t.Fatalf("desc sort should put Cherry before Apple (positions C=%d A=%d)", ci, ai)
	}

	// A hidden field cannot be sorted or filtered by (RBAC oracle guard):
	// route a role that hides `secret` and confirm sort=secret is refused.
	seedFieldRule(t, db, "clerk", "user-clerk", "Widget", "secret")
	code := getAs(t, mux, "/records/Widget?sort=secret", tenantID, "user-clerk").Code
	// Not a crash: either the page renders with sort ignored (field not a
	// visible column, dropped by isVisibleColumn) OR the engine refuses.
	// Both are acceptable; a 500 is not.
	if code == http.StatusInternalServerError {
		t.Fatalf("sorting by a hidden field 500'd, should degrade cleanly: %d", code)
	}
	// The hidden field must not appear as a sortable column at all.
	clerkBody := getAs(t, mux, "/records/Widget", tenantID, "user-clerk").Body.String()
	if strings.Contains(clerkBody, "sort=secret") {
		t.Fatalf("hidden field offered as a sort link to a user who can't see it:\n%s", clerkBody)
	}
}

// TestListPage_ExplicitFilterFieldWithoutQuery_SurvivesNextSubmit is a
// regression test for uc-infra#129: a list page loaded with an explicit
// ?filter=X naming a non-default column but no ?q= yet (a bookmarked or
// shared link, before anyone has typed into the search box) must still
// render a hidden `filter` input carrying X — otherwise the next real
// form submit (typed value + Go) posts with no `filter=` param at all,
// and the handler silently falls back to the default column instead of
// the one the URL named.
func TestListPage_ExplicitFilterFieldWithoutQuery_SurvivesNextSubmit(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, widgetFilterEntityDef(), &form.Definition{
		EntityType: "Widget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "secret"}}}},
	})

	eng := crud.NewEngine(db)
	if _, err := eng.Create(ctx, widgetFilterEntityDef(), map[string]any{"name": "Apple", "secret": "alpha"}, humanActor()); err != nil {
		t.Fatalf("seed Apple: %v", err)
	}
	if _, err := eng.Create(ctx, widgetFilterEntityDef(), map[string]any{"name": "Banana", "secret": "beta"}, humanActor()); err != nil {
		t.Fatalf("seed Banana: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// A plain landing page (no ?filter= at all) renders no hidden filter
	// field — the implicit per-page default is re-derived by the server
	// on every request regardless, so there's nothing for a bare form
	// submit to lose. Asserted here as the boundary this fix must NOT
	// widen past.
	plain := getAs(t, mux, "/records/Widget", tenantID, "anyone").Body.String()
	if strings.Contains(plain, `name="filter"`) {
		t.Fatalf("plain landing page (no ?filter=) should render no hidden filter field:\n%s", plain)
	}

	// ?filter=secret with NO ?q= yet: "secret" is not the default column
	// (name is, being declared first) — the hidden field must name it
	// explicitly so a later submit doesn't silently revert to "name".
	loaded := getAs(t, mux, "/records/Widget?filter=secret", tenantID, "anyone").Body.String()
	if !strings.Contains(loaded, `name="filter" value="secret"`) {
		t.Fatalf("?filter=secret with no ?q= should render a hidden filter field naming \"secret\":\n%s", loaded)
	}

	// The actual next submit a browser would send once someone types into
	// that box: filter=secret carried forward via the (now-fixed) hidden
	// field, plus q=beta. Must match Banana (secret=beta) by the EXPLICIT
	// column, not silently fall back to filtering "name" by "beta" (which
	// would match nothing). NOTE: this specific assertion would also pass
	// against the pre-fix code (both filter and q are present, so it
	// exercises the untouched opts.FilterField path) — it's extra
	// coverage, not the #129 regression guard; the hidden-field assertion
	// above is.
	submitted := getAs(t, mux, "/records/Widget?filter=secret&q=beta", tenantID, "anyone").Body.String()
	if !strings.Contains(submitted, "Banana") || strings.Contains(submitted, "Apple") {
		t.Fatalf("filter=secret q=beta should match Banana (secret=beta) via the explicit column, not the default:\n%s", submitted)
	}
}

// TestListPage_ExplicitFilterFieldWithoutQuery_SurvivesSortClick is a
// regression test for the gap independent review found in uc-infra#129's
// own fix: the hidden form field alone isn't enough — every sort-header
// and pager link on the SAME page is built by keepQuery, which must also
// carry the explicit filter column forward. Before this, a page loaded
// via "?filter=secret" (no ?q= yet) rendered the hidden field correctly,
// but clicking a column header still linked to a bare "?sort=name" with
// no filter= at all — one click away from reproducing the exact bug the
// hidden field was fixed to prevent, just via a link instead of a submit.
func TestListPage_ExplicitFilterFieldWithoutQuery_SurvivesSortClick(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, widgetFilterEntityDef(), &form.Definition{
		EntityType: "Widget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "secret"}}}},
	})
	eng := crud.NewEngine(db)
	if _, err := eng.Create(ctx, widgetFilterEntityDef(), map[string]any{"name": "Apple", "secret": "alpha"}, humanActor()); err != nil {
		t.Fatalf("seed Apple: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	loaded := getAs(t, mux, "/records/Widget?filter=secret", tenantID, "anyone").Body.String()
	// Params come out "filter", "qregion", "sort" — url.Values.Encode()
	// sorts keys alphabetically. "qregion" (uc-infra#164 — deliberately
	// NOT "region", see qRegionParam's own doc comment) rides along
	// unconditionally; default locale/no-cookie region is en-GB.
	if !strings.Contains(loaded, `href="/records/Widget?filter=secret&amp;qregion=en-GB&amp;sort=name"`) {
		t.Fatalf("the \"name\" column's sort link should carry filter=secret forward from a ?filter=secret (no ?q=) page, not drop it:\n%s", loaded)
	}
	// No q= should be tacked on when there was never a value to carry —
	// only filter+qregion+sort, not a spurious empty q=. Parsed out of
	// each /records/Widget href's own query (net/url, same idiom
	// regionOptionValues uses elsewhere in this package), not a raw
	// substring match: uc-infra#164 inserted "qregion" between "filter"
	// and "sort" alphabetically, so the original `sort=name&q=` /
	// `q=&sort=name` substrings could never appear again regardless of
	// whether this bug was present (independent review — the guard had
	// gone silently unreachable), and a raw "[?&]q=" pattern is its own
	// hazard: this same page's unrelated reference-search JS builds a
	// literal "?q=" string that isn't a list-page href at all and would
	// false-positive the check (found by reproducing it).
	for _, m := range regexp.MustCompile(`href="(/records/Widget\?[^"]*)"`).FindAllStringSubmatch(loaded, -1) {
		href := html.UnescapeString(m[1])
		u, err := url.Parse(href)
		if err != nil {
			t.Fatalf("href %q did not parse: %v", href, err)
		}
		if q, ok := u.Query()["q"]; ok && (len(q) == 0 || q[0] == "") {
			t.Fatalf("sort link %q should not carry an empty q= when no filter value was ever typed", href)
		}
	}
}

// TestListPage_FilterFormPreservesSort is a regression test for uc-infra#139,
// found during #129's own independent review: the filter FORM (as opposed
// to the sort-header/pager links keepQuery already covers) carried no
// hidden sort=/dir= at all, and its action attribute (FilterHref) was a
// bare "/records/{entityType}" with no query string. So loading a list
// page already sorted, then typing into the search box and submitting,
// silently reverted the ordering to the default (created_at) — the exact
// "in-page navigation silently discards URL state the viewer didn't ask
// to lose" family #129 fixed for the hidden filter field, just via the
// filter form instead.
func TestListPage_FilterFormPreservesSort(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, widgetFilterEntityDef(), &form.Definition{
		EntityType: "Widget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "secret"}}}},
	})
	eng := crud.NewEngine(db)
	for _, n := range []string{"Apple", "Banana", "Cherry"} {
		if _, err := eng.Create(ctx, widgetFilterEntityDef(), map[string]any{"name": n, "secret": "s"}, humanActor()); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// A plain landing page (no ?sort= at all) renders no hidden sort/dir
	// fields — nothing to preserve, and rendering empty ones would be
	// noise. Asserted as the boundary this fix must not widen past.
	plain := getAs(t, mux, "/records/Widget", tenantID, "anyone").Body.String()
	if strings.Contains(plain, `name="sort"`) || strings.Contains(plain, `name="dir"`) {
		t.Fatalf("plain landing page (no ?sort=) should render no hidden sort/dir fields:\n%s", plain)
	}

	// ?sort=name&dir=desc: the filter form must carry both forward as
	// hidden inputs so the NEXT submit doesn't silently drop them.
	descLoaded := getAs(t, mux, "/records/Widget?sort=name&dir=desc", tenantID, "anyone").Body.String()
	if !strings.Contains(descLoaded, `name="sort" value="name"`) {
		t.Fatalf("?sort=name&dir=desc should render a hidden sort field naming \"name\":\n%s", descLoaded)
	}
	if !strings.Contains(descLoaded, `name="dir" value="desc"`) {
		t.Fatalf("?sort=name&dir=desc should render a hidden dir field naming \"desc\":\n%s", descLoaded)
	}

	// Ascending sort (?sort=name, no ?dir=) carries the hidden sort field
	// but no dir field — dir=desc is the only explicit value, matching
	// keepQuery's own sortParams (ascending is the implicit default, never
	// written to the URL). A spurious dir="" would be indistinguishable
	// from "no sort active" to a later request.
	ascLoaded := getAs(t, mux, "/records/Widget?sort=name", tenantID, "anyone").Body.String()
	if !strings.Contains(ascLoaded, `name="sort" value="name"`) {
		t.Fatalf("?sort=name should render a hidden sort field naming \"name\":\n%s", ascLoaded)
	}
	if strings.Contains(ascLoaded, `name="dir"`) {
		t.Fatalf("?sort=name (ascending, no explicit dir) should render no hidden dir field:\n%s", ascLoaded)
	}

	// The actual next submit a browser would send once someone types into
	// the search box from a sorted page: sort=name&dir=desc carried forward
	// via the (now-fixed) hidden fields, plus the new filter=name&q=an.
	// This one narrows to a single row (Banana), so it doesn't itself prove
	// sort survived — the composed case just below does that.
	submitted := getAs(t, mux, "/records/Widget?sort=name&dir=desc&filter=name&q=an", tenantID, "anyone").Body.String()
	if !strings.Contains(submitted, "Banana") || strings.Contains(submitted, "Apple") || strings.Contains(submitted, "Cherry") {
		t.Fatalf("filter=name q=an should show only Banana regardless of sort:\n%s", submitted)
	}

	// Sort and filter actually COMPOSE: all three widgets share secret="s",
	// so filtering on it keeps every row, and the result must still be in
	// descending-name order (Cherry, Banana, Apple) — proving the carried-
	// forward sort is applied to the filtered set, not silently dropped in
	// favor of the filter (or vice versa).
	composed := getAs(t, mux, "/records/Widget?sort=name&dir=desc&filter=secret&q=s", tenantID, "anyone").Body.String()
	ci, bi, ai := strings.Index(composed, "Cherry"), strings.Index(composed, "Banana"), strings.Index(composed, "Apple")
	if ci == -1 || bi == -1 || ai == -1 || !(ci < bi && bi < ai) {
		t.Fatalf("sort=name&dir=desc&filter=secret&q=s should show all three rows in descending order (Cherry, Banana, Apple), got positions C=%d B=%d A=%d:\n%s", ci, bi, ai, composed)
	}

	// FilterHref (the form's action=) itself must not silently drop the
	// active sort either. A GET form's submit is built from the form's own
	// fields, NOT the action attribute's query string — a browser discards
	// whatever query string action= carries and reconstructs one from
	// scratch out of every named input/value pair. That's the actual reason
	// hidden inputs (not an action="...?sort=...") are the fix: an action
	// query string would be silently thrown away on every submit regardless.
	// This just confirms the action stays the plain list path, consistent
	// with the existing filter hidden field's own approach.
	if !strings.Contains(descLoaded, `action="/records/Widget"`) {
		t.Fatalf("filter form action should remain the plain list path:\n%s", descLoaded)
	}

	// The "Clear" link sits two lines below the search box in the same
	// form and was found, during independent review, silently dropping the
	// sort the same way the search box itself used to (uc-infra#139): it
	// must carry the active sort forward while dropping the filter this
	// link is meant to clear (unlike the hidden inputs above, which
	// preserve the filter, this is the one place in the form that must
	// NOT do so). Checked against `submitted` (sort=name&dir=desc AND an
	// active filter), not `descLoaded` — the Clear link only renders at
	// all when FilterValue is set, so a page with no active filter has no
	// Clear link to check. Params come out "dir", "qregion", "sort" —
	// url.Values.Encode() sorts keys alphabetically. "qregion"
	// (deliberately NOT "region", see qRegionParam's own doc comment)
	// rides along unconditionally since uc-infra#164 (default locale/
	// no-cookie region is en-GB) — the Clear link, like every other link
	// this handler generates, pins the region a stale "q" would be
	// interpreted under so following it later never depends on whatever
	// region cookie happens to be active at that point.
	if !strings.Contains(submitted, `href="/records/Widget?dir=desc&amp;qregion=en-GB&amp;sort=name"`) {
		t.Fatalf("Clear link should drop the filter but keep sort=name&dir=desc&qregion=en-GB:\n%s", submitted)
	}
	// No sort active: Clear still pins the region (uc-infra#164) even
	// though there's no sort to carry forward.
	filteredNoSort := getAs(t, mux, "/records/Widget?filter=name&q=an", tenantID, "anyone").Body.String()
	if !strings.Contains(filteredNoSort, `href="/records/Widget?qregion=en-GB"`) {
		t.Fatalf("Clear link with no active sort should still pin region=en-GB:\n%s", filteredNoSort)
	}
}

// TestListPage_FilterFieldHidden_RendersNoHiddenInput is a regression
// guard for the false-path of the condition uc-infra#129's fix
// introduced (filterFieldValid): an explicit ?filter= naming a column
// the viewer cannot see (RBAC-redacted) or that does not exist on the
// Definition at all must render NO hidden filter input — echoing the
// requested field name back into the DOM either way would be a direct
// RBAC oracle (the same class of leak isVisibleColumn/sortFilterableField
// exist to prevent for sort= elsewhere in this file).
func TestListPage_FilterFieldHidden_RendersNoHiddenInput(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, widgetFilterEntityDef(), &form.Definition{
		EntityType: "Widget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "secret"}}}},
	})
	if _, err := crud.NewEngine(db).Create(ctx, widgetFilterEntityDef(), map[string]any{"name": "Apple", "secret": "alpha"}, humanActor()); err != nil {
		t.Fatalf("seed Apple: %v", err)
	}
	seedFieldRule(t, db, "clerk", "user-clerk", "Widget", "secret")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// A role that cannot see "secret" must not have it echoed back as a
	// hidden field just because it named the column explicitly in the URL.
	hidden := getAs(t, mux, "/records/Widget?filter=secret", tenantID, "user-clerk").Body.String()
	if strings.Contains(hidden, `name="filter"`) {
		t.Fatalf("?filter=secret from a viewer who cannot see \"secret\" must render no hidden filter field (RBAC oracle):\n%s", hidden)
	}

	// A field name that doesn't exist on the Definition at all degrades
	// the same way — no panic, no hidden field.
	unknown := getAs(t, mux, "/records/Widget?filter=does_not_exist", tenantID, "anyone")
	if unknown.Code != http.StatusOK {
		t.Fatalf("?filter=<unknown field> should degrade to a normal 200 page, not %d", unknown.Code)
	}
	if strings.Contains(unknown.Body.String(), `name="filter"`) {
		t.Fatalf("?filter=<unknown field> must render no hidden filter field:\n%s", unknown.Body.String())
	}
}

// TestListPage_SortFieldHidden_RendersNoHiddenInput is the hidden-`sort`-
// input counterpart to TestListPage_FilterFieldHidden_RendersNoHiddenInput
// above — found missing during uc-infra#139's independent review. The new
// hidden sort input (this file's own TestListPage_FilterFormPreservesSort)
// echoes opts.SortField back into the DOM the same way the hidden filter
// input already does, so it needs the identical RBAC-oracle guard:
// opts.SortField is only ever set (renderRecordList, ?sort= handling) after
// isVisibleColumn && sortFilterableField both pass, so a field the viewer
// can't see or that doesn't exist must never be echoed back, confirming the
// DOM itself (not just the response code TestListPage_FilterAndSort already
// checks for a hidden-field sort attempt) stays silent.
func TestListPage_SortFieldHidden_RendersNoHiddenInput(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, widgetFilterEntityDef(), &form.Definition{
		EntityType: "Widget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "secret"}}}},
	})
	if _, err := crud.NewEngine(db).Create(ctx, widgetFilterEntityDef(), map[string]any{"name": "Apple", "secret": "alpha"}, humanActor()); err != nil {
		t.Fatalf("seed Apple: %v", err)
	}
	seedFieldRule(t, db, "clerk", "user-clerk", "Widget", "secret")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// A role that cannot see "secret" must not have it echoed back as a
	// hidden sort field just because it named the column explicitly in the
	// URL — the same RBAC oracle the hidden filter field already guards
	// against, now for sort=.
	hidden := getAs(t, mux, "/records/Widget?sort=secret", tenantID, "user-clerk").Body.String()
	if strings.Contains(hidden, `name="sort"`) || strings.Contains(hidden, `name="dir"`) {
		t.Fatalf("?sort=secret from a viewer who cannot see \"secret\" must render no hidden sort/dir field (RBAC oracle):\n%s", hidden)
	}

	// A field name that doesn't exist on the Definition at all degrades
	// the same way — no panic, no hidden field.
	unknown := getAs(t, mux, "/records/Widget?sort=does_not_exist", tenantID, "anyone")
	if unknown.Code != http.StatusOK {
		t.Fatalf("?sort=<unknown field> should degrade to a normal 200 page, not %d", unknown.Code)
	}
	if strings.Contains(unknown.Body.String(), `name="sort"`) {
		t.Fatalf("?sort=<unknown field> must render no hidden sort field:\n%s", unknown.Body.String())
	}
}

// referenceFirstEntityDef declares "category" (a Reference) as the FIRST
// field, so the no-explicit-filter default (renderRecordList's own loop
// over columns) lands on it — the exact shape board #49 flagged: for an
// entity type where the first visible column is a reference, does typing
// a label into the single filter box actually find anything, or does it
// silently match against the raw target id?
func referenceFirstEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Gadget", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "category", Type: entity.FieldReference, Target: "Category"},
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

func categoryEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Category", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

// TestListPage_DefaultFilterOnReferenceColumn_ResolvesByLabel is a
// regression test for board #49: when no explicit ?filter= is given, the
// single filter box targets the first visible column, and for ~16 of ~34
// entity types today that column is a Reference. Confirms the box still
// finds the record when the user types the TARGET's label (what the cell
// actually displays), not the stored id — this already works end-to-end
// via the same label-resolution path #24 added for an explicit
// ?filter=<reference field>, but nothing exercised it for the DEFAULT
// (no ?filter=) case, so a regression here would have shipped silently.
func TestListPage_DefaultFilterOnReferenceColumn_ResolvesByLabel(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, categoryEntityDef(), &form.Definition{
		EntityType: "Category", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}}}},
	})
	publishEntityAndForm(t, db, referenceFirstEntityDef(), &form.Definition{
		EntityType: "Gadget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "category"}, {Name: "name"}}}},
	})

	eng := crud.NewEngine(db)
	widgets, err := eng.Create(ctx, categoryEntityDef(), map[string]any{"name": "Widgets"}, humanActor())
	if err != nil {
		t.Fatalf("create Widgets category: %v", err)
	}
	gizmos, err := eng.Create(ctx, categoryEntityDef(), map[string]any{"name": "Gizmos"}, humanActor())
	if err != nil {
		t.Fatalf("create Gizmos category: %v", err)
	}
	if _, err := eng.Create(ctx, referenceFirstEntityDef(), map[string]any{"category": widgets.ID, "name": "Sprocket"}, humanActor()); err != nil {
		t.Fatalf("seed Sprocket: %v", err)
	}
	if _, err := eng.Create(ctx, referenceFirstEntityDef(), map[string]any{"category": gizmos.ID, "name": "Doohickey"}, humanActor()); err != nil {
		t.Fatalf("seed Doohickey: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// No ?filter= at all: the box defaults to "category" (first column),
	// a Reference. Typing the target's own label ("Widgets") must find
	// Sprocket and must NOT match on the raw stored category id.
	body := getAs(t, mux, "/records/Gadget?q=Widgets", tenantID, "anyone").Body.String()
	if !strings.Contains(body, "Sprocket") {
		t.Fatalf("default filter on a reference column should resolve %q by label and find Sprocket:\n%s", "Widgets", body)
	}
	if strings.Contains(body, "Doohickey") {
		t.Fatalf("default filter q=Widgets should not also match Doohickey (Gizmos category):\n%s", body)
	}

	// The hidden input carrying the resolved default field must actually
	// be "category" — asserting the mechanism, not just the outcome.
	if !strings.Contains(body, `name="filter" value="category"`) {
		t.Fatalf("expected the default filter field to resolve to the reference column \"category\":\n%s", body)
	}

	// A label matching NO category must show NO rows — not every row.
	// referenceIDsMatching returns a non-nil-but-empty slice for "no
	// target matched" specifically so ListPageFiltered's FilterIn takes
	// the "match these ids" branch with an empty id list, rather than
	// its FilterIn == nil branch ("no id-based filter at all"), which
	// would silently widen this into every Gadget in the tenant.
	noMatch := getAs(t, mux, "/records/Gadget?q=Nonexistent", tenantID, "anyone").Body.String()
	if strings.Contains(noMatch, "Sprocket") || strings.Contains(noMatch, "Doohickey") {
		t.Fatalf("default filter q=Nonexistent (matches no category) should show no rows, not every row:\n%s", noMatch)
	}
}

// TestListPage_DefaultFilterOnReferenceColumn_UnlicensedTarget is a
// regression test for the referenceIDsMatching degradation branch
// (listview.go's data.ErrNotFound case): a reference field's target
// entity type can be legitimately absent from the tenant's registry (its
// module isn't licensed here — crm referencing Item/SalesOrder is the
// real-world case), and filtering by it must resolve to "no matches",
// not 500 the whole list page. Gadget is published; Category is not.
func TestListPage_DefaultFilterOnReferenceColumn_UnlicensedTarget(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	// Deliberately NOT publishing Category — Gadget's "category" reference
	// target is absent from this tenant's registry.
	publishEntityAndForm(t, db, referenceFirstEntityDef(), &form.Definition{
		EntityType: "Gadget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "category"}, {Name: "name"}}}},
	})
	if _, err := crud.NewEngine(db).Create(ctx, referenceFirstEntityDef(), map[string]any{"category": "no-such-id", "name": "Sprocket"}, humanActor()); err != nil {
		t.Fatalf("seed Sprocket: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	resp := getAs(t, mux, "/records/Gadget?q=Widgets", tenantID, "anyone")
	if resp.Code != http.StatusOK {
		t.Fatalf("filtering by an unlicensed reference target should degrade to a normal page, not %d:\n%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "Sprocket") {
		t.Fatalf("q=Widgets should match nothing when the target type isn't licensed (no target to resolve a label against):\n%s", resp.Body.String())
	}
}

// TestListPage_DefaultFilterOnReferenceColumn_UnreadableTarget is a
// regression test for uc-infra#89: referenceIDsMatching's data.ErrNotFound
// degrade branch (exercised above) has a sibling gap — a reference
// target that IS licensed/published but that THIS VIEWER may not read
// (entity-level RBAC, ADR-0006) used to propagate authz.ErrDenied out of
// ts.crud.ListPageFiltered, through writeCrudPageError, into a 403 for
// the whole Gadget list page — even though the viewer can read Gadget
// itself and is simply typing into the filter box. Every neighbouring
// resolution path already degrades for this actor (the cell just shows
// the raw id); the filter path must degrade the same way: zero matches,
// not a page-wide access-denied.
func TestListPage_DefaultFilterOnReferenceColumn_UnreadableTarget(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, categoryEntityDef(), &form.Definition{
		EntityType: "Category", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}}}},
	})
	publishEntityAndForm(t, db, referenceFirstEntityDef(), &form.Definition{
		EntityType: "Gadget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "category"}, {Name: "name"}}}},
	})

	eng := crud.NewEngine(db)
	widgets, err := eng.Create(ctx, categoryEntityDef(), map[string]any{"name": "Widgets"}, humanActor())
	if err != nil {
		t.Fatalf("create Widgets category: %v", err)
	}
	if _, err := eng.Create(ctx, referenceFirstEntityDef(), map[string]any{"category": widgets.ID, "name": "Sprocket"}, humanActor()); err != nil {
		t.Fatalf("seed Sprocket: %v", err)
	}

	// clerk can read Gadget but is explicitly denied read on Category.
	// This row is what makes the difference from "no rule at all": RBAC
	// is opt-in per entity type (authz.go), so a Permission row naming
	// Category for ANY role switches Category to deny-unless-granted,
	// and clerk's roles grant it nothing there.
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Gadget", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "Category", "can_read": false, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Sanity check: clerk can see the Gadget list at all (Gadget itself
	// is granted). The reference cell degrades to the raw id, same as
	// every other resolution path when the target isn't readable.
	plain := getAs(t, mux, "/records/Gadget", tenantID, "user-clerk")
	if plain.Code != http.StatusOK {
		t.Fatalf("clerk should be able to view the Gadget list at all: %d:\n%s", plain.Code, plain.Body.String())
	}

	// Typing into the (default, reference) filter box must degrade to
	// "no matches" — not turn a page clerk can otherwise view into a 403.
	// Assert the mechanism, not just the outcome: the hidden input must
	// confirm the filter actually landed on "category" (the reference
	// column) — otherwise a 200-with-no-Sprocket could just as easily
	// mean the default filter silently stopped resolving to a reference
	// column at all, which would pass this assertion for the wrong
	// reason (same guard TestListPage_DefaultFilterOnReferenceColumn_ResolvesByLabel
	// uses).
	resp := getAs(t, mux, "/records/Gadget?q=Widgets", tenantID, "user-clerk")
	if resp.Code != http.StatusOK {
		t.Fatalf("filtering by a reference target the viewer can't read should degrade to a normal page, not %d:\n%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `name="filter" value="category"`) {
		t.Fatalf("expected the default filter field to still resolve to the reference column \"category\":\n%s", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "Sprocket") {
		t.Fatalf("q=Widgets should match nothing when the viewer can't read the target type:\n%s", resp.Body.String())
	}

	// Negative control: an actor with NO roles at all has no Permission
	// rows applying to them, but RBAC's opt-in is per entity type, not
	// per actor — Category already has a rule (naming clerk), so it is
	// NOT open to a roleless actor either. Prove the denial path is what
	// the assertions above actually exercise by using a genuinely
	// unrestricted setup instead: a fresh tenant with no RBAC configured
	// for Category at all, where the same filter DOES match.
	unrestrictedTenantID, unrestrictedDB := newTestTenant(t, router)
	if err := foundation.Publish(ctx, unrestrictedDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish (unrestricted tenant): %v", err)
	}
	publishEntityAndForm(t, unrestrictedDB, categoryEntityDef(), &form.Definition{
		EntityType: "Category", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}}}},
	})
	publishEntityAndForm(t, unrestrictedDB, referenceFirstEntityDef(), &form.Definition{
		EntityType: "Gadget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "category"}, {Name: "name"}}}},
	})
	unrestrictedEng := crud.NewEngine(unrestrictedDB)
	unrestrictedWidgets, err := unrestrictedEng.Create(ctx, categoryEntityDef(), map[string]any{"name": "Widgets"}, humanActor())
	if err != nil {
		t.Fatalf("create Widgets category (unrestricted tenant): %v", err)
	}
	if _, err := unrestrictedEng.Create(ctx, referenceFirstEntityDef(), map[string]any{"category": unrestrictedWidgets.ID, "name": "Sprocket"}, humanActor()); err != nil {
		t.Fatalf("seed Sprocket (unrestricted tenant): %v", err)
	}
	unrestrictedMux := http.NewServeMux()
	testHandler(t, router).Routes(unrestrictedMux)
	control := getAs(t, unrestrictedMux, "/records/Gadget?q=Widgets", unrestrictedTenantID, "anyone")
	if control.Code != http.StatusOK {
		t.Fatalf("control tenant (no RBAC on Category): expected 200, got %d:\n%s", control.Code, control.Body.String())
	}
	if !strings.Contains(control.Body.String(), "Sprocket") {
		t.Fatalf("control tenant (no RBAC on Category) should still find Sprocket by label — this proves the denial in the main case above is what's actually being exercised:\n%s", control.Body.String())
	}
}

// TestListPage_DefaultFilterOnReferenceColumn_HiddenLabelField is a
// regression test for the SECOND, distinct source of authz.ErrDenied
// referenceIDsMatching's degrade branch now catches (independent
// review, uc-infra#89): rejectHiddenSortFilter refuses to sort/filter by
// a FIELD the viewer can't read, even when the viewer CAN read the
// target entity type overall. This fires when the target's own label
// field (the column referenceIDsMatching filters by) is hidden via
// FieldPermission, not when the whole entity type is RBAC-denied — a
// different rule (authz.go's rejectHiddenSortFilter) tripping the same
// ErrDenied sentinel. Same required outcome: degrade to no-match, not a
// page-wide 403, and for the same "widening to unfiltered would be
// worse" reason listview.go's ErrDenied branch comment now documents.
func TestListPage_DefaultFilterOnReferenceColumn_HiddenLabelField(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, categoryEntityDef(), &form.Definition{
		EntityType: "Category", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}}}},
	})
	publishEntityAndForm(t, db, referenceFirstEntityDef(), &form.Definition{
		EntityType: "Gadget", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "category"}, {Name: "name"}}}},
	})

	eng := crud.NewEngine(db)
	widgets, err := eng.Create(ctx, categoryEntityDef(), map[string]any{"name": "Widgets"}, humanActor())
	if err != nil {
		t.Fatalf("create Widgets category: %v", err)
	}
	if _, err := eng.Create(ctx, referenceFirstEntityDef(), map[string]any{"category": widgets.ID, "name": "Sprocket"}, humanActor()); err != nil {
		t.Fatalf("seed Sprocket: %v", err)
	}

	// clerk can read Category as an entity type — the entity-level rule
	// this test must NOT be exercising — but Category.name (the label
	// field referenceIDsMatching filters by) is hidden from clerk via
	// FieldPermission, which is a field-level rule. Field hiding applies
	// only when EVERY role the user holds hides the field (authz.go), so
	// this must be ONE role carrying both the entity-level grant and the
	// field-level hide — composing seedRBAC + seedFieldRule would create
	// TWO "clerk" roles, and the second (no FieldPermission row) would
	// implicitly see Category.name, defeating the hide.
	roleIDs := seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Gadget", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "Category", "can_read": true, "can_write": false},
		},
	)
	if _, err := crud.NewEngine(db).Create(ctx, foundation.FieldPermission(), map[string]any{
		"role_id": roleIDs["clerk"], "entity_type": "Category", "field_name": "name", "hidden": true,
	}, humanActor()); err != nil {
		t.Fatalf("create FieldPermission: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	resp := getAs(t, mux, "/records/Gadget?q=Widgets", tenantID, "user-clerk")
	if resp.Code != http.StatusOK {
		t.Fatalf("filtering by a reference target whose label field is hidden should degrade to a normal page, not %d:\n%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "Sprocket") {
		t.Fatalf("q=Widgets should match nothing when the viewer can't read the target's label field:\n%s", resp.Body.String())
	}
}
