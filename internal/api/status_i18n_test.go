package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
)

// TestAPI_StatusName_ResolvesPerLocale is the end-to-end proof for
// uc-infra#214 (ADR-0030): foundation.Status's "name" field is now
// i18n_text, so /api/references/Status — the same endpoint every
// Status-targeting reference picker in the product actually calls —
// resolves a Status's label per the viewer's own locale, the same way
// it already does for any other i18n_text-labelled entity
// (TestAPI_I18nText_FormAssemblesObjectAndRoundTrips's MultiUnit).
//
// Seeds through the real production path (statusgraph.Seed, the same
// function every module's PublishStatuses calls) rather than a
// synthetic fixture, so this also proves the Phase 1 wrap
// (internal/kernel/statusgraph.Seed wrapping Spec.Name as {"en": name})
// produces a shape recordLabel actually resolves. The "tr" translation
// is added afterward via a direct Update, standing in for the phase-2
// follow-up (uc-infra#244) that will seed real per-locale content —
// this test proves the MECHANISM works today, independent of when real
// translations ship.
func TestAPI_StatusName_ResolvesPerLocale(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)

	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "test-setup"}
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)

	def := func(entityType string) *entity.Definition {
		t.Helper()
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	statusTypeDef, statusDef, transitionDef := def("StatusType"), def("Status"), def("StatusTransition")

	statusIDs, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"WidgetI18n", "widget_i18n_status", "Widget i18n Status",
		[]statusgraph.Spec{
			{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true},
		},
		nil, actor,
	)
	if err != nil {
		t.Fatalf("statusgraph.Seed: %v", err)
	}
	draftID := statusIDs["draft"]

	// English-only, as every module's PublishStatuses produces today
	// (Phase 1's own scope) — confirm this alone resolves correctly
	// before adding a second locale.
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/api/references/Status?lang=en", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/references/Status?lang=en: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != draftID || opts[0].Label != "Draft" {
		t.Fatalf("expected [{%s, Draft}] for lang=en, got %+v", draftID, opts)
	}

	// A locale with no translation yet (Phase 1's actual shipped state)
	// falls back to the catalog's fallback locale ("en") rather than
	// rendering blank or the raw id — the exact "no visible regression"
	// property ADR-0030 relies on.
	req = newRequest("GET", "/api/references/Status?lang=tr", tenantID, "farshid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts = decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].Label != "Draft" {
		t.Fatalf("expected an untranslated Status to fall back to \"Draft\" for lang=tr, got %+v", opts)
	}

	// Now add a real translation directly (standing in for uc-infra#244's
	// eventual PublishStatuses content) and confirm the SAME endpoint
	// picks it up for the matching locale, while an unrelated locale
	// still falls back.
	current, err := engine.Get(ctx, statusDef, draftID)
	if err != nil {
		t.Fatalf("Get draft status: %v", err)
	}
	updated := map[string]any{}
	for k, v := range current.Data {
		updated[k] = v
	}
	updated["name"] = map[string]any{"en": "Draft", "tr": "Taslak"}
	expectedVersion := current.Version
	if _, err := engine.Update(ctx, statusDef, draftID, updated, &expectedVersion, actor); err != nil {
		t.Fatalf("add tr translation: %v", err)
	}

	req = newRequest("GET", "/api/references/Status?lang=tr", tenantID, "farshid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts = decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].Label != "Taslak" {
		t.Fatalf("expected the tr translation \"Taslak\" for lang=tr once seeded, got %+v", opts)
	}

	req = newRequest("GET", "/api/references/Status?lang=ar", tenantID, "farshid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts = decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].Label != "Draft" {
		t.Fatalf("expected lang=ar (no ar translation seeded) to still fall back to \"Draft\", got %+v", opts)
	}
}
