package api

import (
	"context"
	"net/http"
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
	if !strings.Contains(loaded, `href="/records/Widget?filter=secret&amp;sort=name"`) {
		t.Fatalf("the \"name\" column's sort link should carry filter=secret forward from a ?filter=secret (no ?q=) page, not drop it:\n%s", loaded)
	}
	// No q= should be tacked on when there was never a value to carry —
	// only filter+sort, not a spurious empty q=.
	if strings.Contains(loaded, `sort=name&amp;q=`) || strings.Contains(loaded, `q=&amp;sort=name`) {
		t.Fatalf("sort link should not carry an empty q= when no filter value was ever typed:\n%s", loaded)
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
