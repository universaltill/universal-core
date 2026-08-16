package api

import (
	"context"
	"encoding/json"
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

	// The exact regression the first ADR-0030 attempt shipped (reverted
	// before landing, see uc-infra#245/#249): a Status picker's own
	// type-ahead search must still be REAL, not the permanently-degraded
	// unsorted/unfiltered list `Status`'s ~77-row count would otherwise
	// force. Search against the tr translation just seeded above.
	req = newRequest("GET", "/api/references/Status?lang=tr&q=Tasl", tenantID, "farshid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/references/Status?q=Tasl: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts = decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != draftID || opts[0].Label != "Taslak" {
		t.Fatalf("expected q=Tasl to find the tr-translated status by its own locale's text, got %+v", opts)
	}
	req = newRequest("GET", "/api/references/Status?lang=tr&q=zzz-no-such-status", tenantID, "farshid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts = decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 0 {
		t.Fatalf("expected a non-matching q= to filter the result OUT (proving q= actually filters, not just tolerates), got %+v", opts)
	}
}

// TestAPI_StatusName_LegacyPreBackfillRowStillLabelsAndIsFindable is the
// second exact regression the first ADR-0030 attempt shipped: a Status
// row written before v2's backfill runs still holds a plain string, not
// yet {"en": ...}. recordLabel must fall through to that plain string
// (not the raw record id — uc-infra#245's own fix), and the picker's
// search must still find it by that plain string (the same
// canSortFilter/FilterI18nText legacy-scalar OR-branch uc-infra#249
// shipped), same as the pinned generic i18n_text_test.go coverage but
// exercised against the real, production Status Definition rather than a
// synthetic one — the entity this whole bump is actually about.
func TestAPI_StatusName_LegacyPreBackfillRowStillLabelsAndIsFindable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)

	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "test-setup"}
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	entityDefs := data.NewEntityDefinitionRepo(db)

	statusTypeDef, err := func() (*entity.Definition, error) {
		v, err := entityDefs.GetPublished(ctx, "StatusType")
		if err != nil {
			return nil, err
		}
		return entity.Unmarshal(v.Definition)
	}()
	if err != nil {
		t.Fatalf("get StatusType definition: %v", err)
	}
	engine := crud.NewEngine(db)
	statusType, err := engine.Create(ctx, statusTypeDef, map[string]any{
		"entity_type": "LegacyWidget", "code": "legacy_widget_status", "name": "Legacy Widget Status",
	}, actor)
	if err != nil {
		t.Fatalf("create StatusType: %v", err)
	}

	// A raw INSERT, bypassing crud.Engine entirely — exactly the shape a
	// real pre-v2 tenant's Status row is: a plain string "name", not the
	// {"en": ...} object v2's Definition now declares. crud.Engine.Create
	// would reject this shape outright (that's the whole point of the
	// Version bump), which is why insertLegacyPurchaseOrder's sibling
	// technique (cmd/sync-tenant-modules' own main_test.go) is needed
	// here too, one layer down at the raw records table.
	raw, err := json.Marshal(map[string]any{
		"status_type_id": statusType.ID, "code": "legacy", "name": "Legacy Draft", "sequence": float64(1),
	})
	if err != nil {
		t.Fatalf("marshal legacy Status: %v", err)
	}
	var legacyID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO records (entity_type, data) VALUES ('Status', $1) RETURNING id`, raw,
	).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy Status row: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Label: falls through to the plain string, not the raw id.
	req := newRequest("GET", "/api/references/Status?lang=en", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/references/Status: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	foundIdx := -1
	for i := range opts {
		if opts[i].ID == legacyID {
			foundIdx = i
		}
	}
	if foundIdx < 0 {
		t.Fatalf("expected the legacy pre-backfill row in the unfiltered list, got %+v", opts)
	}
	if got := opts[foundIdx].Label; got != "Legacy Draft" {
		t.Fatalf("expected the legacy row to label as its own plain string \"Legacy Draft\", not the raw id or something else, got %q", got)
	}

	// Search: still findable by its own (legacy, plain-string) text.
	req = newRequest("GET", "/api/references/Status?lang=en&q=Legacy", tenantID, "farshid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts = decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != legacyID || opts[0].Label != "Legacy Draft" {
		t.Fatalf("expected q=Legacy to find the legacy pre-backfill row by its own plain-string text, got %+v", opts)
	}
}
