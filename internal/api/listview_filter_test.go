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
