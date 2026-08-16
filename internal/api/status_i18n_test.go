package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
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

// TestAPI_StatusName_RealModuleTranslationsResolvePerLocale is the
// phase-2 (uc-infra#244) proof TestAPI_StatusName_ResolvesPerLocale's own
// doc comment says is still owed: that PROMISE was "the MECHANISM works
// today, independent of when real translations ship" — this test proves
// the real translations have now shipped, seeded through the actual
// production path (purchasing/sales/crm.PublishStatuses ->
// statusgraph.Seed, not a manual Update standing in for one), and that
// three different modules' real content resolves correctly, not just
// purchasing's.
//
// source_entity_type/source_field auto-scopes each query to its own
// StatusType (uc-infra#222) — necessary here because, unlike the
// single-StatusType widget fixtures above, this test publishes three
// real modules at once, so an unscoped /api/references/Status would
// return every module's Status rows mixed together.
func TestAPI_StatusName_RealModuleTranslationsResolvePerLocale(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)

	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "test-setup"}
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}
	if err := sales.Publish(ctx, db, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}
	if err := crm.Publish(ctx, db, actor); err != nil {
		t.Fatalf("crm.Publish: %v", err)
	}
	if err := crm.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("crm.PublishStatuses: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	type refOpt struct{ ID, Label string }
	get := func(sourceEntityType, sourceField, locale string) []refOpt {
		t.Helper()
		req := newRequest("GET",
			"/api/references/Status?source_entity_type="+sourceEntityType+"&source_field="+sourceField+"&lang="+locale,
			tenantID, "farshid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET references for %s.%s lang=%s: expected 200, got %d: %s", sourceEntityType, sourceField, locale, rec.Code, rec.Body.String())
		}
		decoded := decodeRefOptions(t, rec.Body.Bytes())
		out := make([]refOpt, len(decoded))
		for i, o := range decoded {
			out[i] = refOpt{ID: o.ID, Label: o.Label}
		}
		return out
	}
	findLabel := func(t *testing.T, opts []refOpt, wantIDSet map[string]string, code string) string {
		t.Helper()
		id, ok := wantIDSet[code]
		if !ok {
			t.Fatalf("test setup error: no id recorded for code %q", code)
		}
		for _, o := range opts {
			if o.ID == id {
				return o.Label
			}
		}
		t.Fatalf("expected to find option id=%s (code=%s) among %+v", id, code, opts)
		return ""
	}

	// Collect ids by code for purchase_order_status via the English
	// (fallback) listing, which every code resolves against regardless
	// of locale.
	poEN := get("PurchaseOrder", "status_id", "en")
	poIDByLabel := map[string]string{}
	for _, o := range poEN {
		poIDByLabel[o.Label] = o.ID
	}
	poIDsByCode := map[string]string{
		"draft":     poIDByLabel["Draft"],
		"approved":  poIDByLabel["Approved"],
		"cancelled": poIDByLabel["Cancelled"],
	}
	for code, id := range poIDsByCode {
		if id == "" {
			t.Fatalf("could not resolve PurchaseOrder status_id id for code %q from en listing: %+v", code, poEN)
		}
	}

	poAR := get("PurchaseOrder", "status_id", "ar")
	if got := findLabel(t, poAR, poIDsByCode, "draft"); got != "مسودة" {
		t.Errorf("PurchaseOrder draft (ar): expected \"مسودة\", got %q", got)
	}
	if got := findLabel(t, poAR, poIDsByCode, "approved"); got != "معتمد" {
		t.Errorf("PurchaseOrder approved (ar): expected \"معتمد\", got %q", got)
	}

	poFA := get("PurchaseOrder", "status_id", "fa")
	if got := findLabel(t, poFA, poIDsByCode, "draft"); got != "پیش‌نویس" {
		t.Errorf("PurchaseOrder draft (fa): expected \"پیش‌نویس\", got %q", got)
	}
	if got := findLabel(t, poFA, poIDsByCode, "cancelled"); got != "لغوشده" {
		t.Errorf("PurchaseOrder cancelled (fa): expected \"لغوشده\", got %q", got)
	}

	poTR := get("PurchaseOrder", "status_id", "tr")
	if got := findLabel(t, poTR, poIDsByCode, "draft"); got != "Taslak" {
		t.Errorf("PurchaseOrder draft (tr): expected \"Taslak\", got %q", got)
	}
	if got := findLabel(t, poTR, poIDsByCode, "approved"); got != "Onaylandı" {
		t.Errorf("PurchaseOrder approved (tr): expected \"Onaylandı\", got %q", got)
	}

	// A second module (sales), so this isn't purchasing-only wiring.
	// CustomerInvoice.status_id (customer_invoice_status), not
	// SalesOrder.status_id (sales_order_status) — "issued" is the
	// invoice's own code, a different StatusType from the order's.
	ciEN := get("CustomerInvoice", "status_id", "en")
	ciIDByLabel := map[string]string{}
	for _, o := range ciEN {
		ciIDByLabel[o.Label] = o.ID
	}
	ciIDsByCode := map[string]string{"issued": ciIDByLabel["Issued"]}
	if ciIDsByCode["issued"] == "" {
		t.Fatalf("could not resolve CustomerInvoice status_id id for code \"issued\" from en listing: %+v", ciEN)
	}
	ciAR := get("CustomerInvoice", "status_id", "ar")
	if got := findLabel(t, ciAR, ciIDsByCode, "issued"); got != "مُصدَرة" {
		t.Errorf("CustomerInvoice issued (ar): expected \"مُصدَرة\", got %q", got)
	}

	// A third module (crm), whose translations were pulled from the
	// pre-existing field.Case.status_id.* catalog rather than help-topic
	// prose — proving that source produced correct StatusSpecs content
	// too, not just the modules whose translations were newly authored
	// for this card.
	caseEN := get("Case", "status_id", "en")
	caseIDByLabel := map[string]string{}
	for _, o := range caseEN {
		caseIDByLabel[o.Label] = o.ID
	}
	caseIDsByCode := map[string]string{"resolved": caseIDByLabel["Resolved"]}
	if caseIDsByCode["resolved"] == "" {
		t.Fatalf("could not resolve Case status_id id for code \"resolved\" from en listing: %+v", caseEN)
	}
	caseFA := get("Case", "status_id", "fa")
	if got := findLabel(t, caseFA, caseIDsByCode, "resolved"); got != "حل‌شده" {
		t.Errorf("Case resolved (fa): expected \"حل‌شده\", got %q", got)
	}
}
