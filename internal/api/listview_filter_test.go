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
