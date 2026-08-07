package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestAPI_RenderRecordForm_MasterDetailSection_DegradesWhenChildUnreadable
// is the HTTP-level regression test for uc-infra#131: a clerk role
// granted read on PurchaseOrder but explicitly denied read on POLine
// used to 403 the WHOLE parent form (loadMasterDetailChildren
// propagated ListByField's authz.ErrDenied straight through
// buildFormRenderData/renderForm), even though the clerk can read the
// PurchaseOrder record itself. Same shape as
// TestAPI_RBAC_RenderedSurfaces_Denied403 and
// TestListPage_DefaultFilterOnReferenceColumn_UnreadableTarget: the
// degrade must be per-section, not a page-wide failure.
func TestAPI_RenderRecordForm_MasterDetailSection_DegradesWhenChildUnreadable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, db, poLineEntityDef(), poLineFormDef())

	engine := crud.NewEngine(db)
	po, err := engine.Create(ctx, purchaseOrderEntityDef(), map[string]any{"vendor_id": "v1"}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, poLineEntityDef(), map[string]any{"purchase_order_id": po.ID, "line_total": 150.5}, humanActor()); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	// clerk can read PurchaseOrder but is explicitly denied read on
	// POLine — the exact actor/condition uc-infra#131 reproduced
	// (PurchaseOrder can_read:true + POLine can_read:false).
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "POLine", "can_read": false, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/forms/PurchaseOrder/"+po.ID, tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("clerk can read PurchaseOrder but not POLine: expected the parent form to still render (200), not %d:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "internal error") {
		t.Fatalf("degrade must not leak as an internal error: %s", body)
	}
	// Positive control: the parent's OWN data actually rendered, not
	// just an empty/broken shell that happens to also lack "150.5" —
	// vendor_id is the one field this fixture's Header section shows.
	if !strings.Contains(body, "v1") {
		t.Fatalf("expected the parent's own data (vendor_id=v1) to render normally, got:\n%s", body)
	}
	if strings.Contains(body, "150.5") {
		t.Fatalf("expected the unreadable POLine child row (and its roll-up) to be omitted, got:\n%s", body)
	}

	// Negative control: an actor granted read on BOTH PurchaseOrder and
	// POLine still sees the child row — proves the omission above is
	// specifically the POLine denial degrading, not the section (or the
	// whole page) silently going empty for everyone regardless of RBAC.
	seedRBAC(t, db,
		map[string][]string{"supervisor": {"user-supervisor"}},
		[]map[string]any{
			{"role": "supervisor", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "supervisor", "entity_type": "POLine", "can_read": true, "can_write": false},
		},
	)
	controlReq := newRequest("GET", "/forms/PurchaseOrder/"+po.ID, tenantID, "user-supervisor", nil)
	controlRec := httptest.NewRecorder()
	mux.ServeHTTP(controlRec, controlReq)
	if controlRec.Code != http.StatusOK {
		t.Fatalf("supervisor (granted read on both): expected 200, got %d:\n%s", controlRec.Code, controlRec.Body.String())
	}
	if !strings.Contains(controlRec.Body.String(), "150.5") {
		t.Fatalf("supervisor (granted read on both PurchaseOrder and POLine) should still see the POLine child row/roll-up:\n%s", controlRec.Body.String())
	}
}

// twoSectionParentEntityDef/twoSectionParentFormDef and
// alphaChildEntityDef/betaChildEntityDef (+ their form defs) are a
// dedicated fixture for
// TestAPI_RenderRecordForm_MasterDetailSection_PerSectionDegradeIsIndependent
// below: purchaseOrderFormDef above has only ONE master-detail section,
// so no existing fixture can prove the degrade is truly PER-SECTION
// (independent review, uc-infra#131, finding #3) rather than
// accidentally affecting every section once any one of them hits
// ErrDenied. Two shipped kernel forms actually have two master-detail
// sections each (internal/kernel/purchasing/forms.go's
// RequestForQuotationForm, internal/kernel/projects/forms.go's
// ProjectForm), so this is a live shape, not a hypothetical one.
func twoSectionParentEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "TwoSectionParent",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
		},
		Relationships: []entity.Relationship{
			{Name: "alphas", Kind: entity.RelationComposition, Target: "AlphaChild", ParentField: "parent_id"},
			{Name: "betas", Kind: entity.RelationComposition, Target: "BetaChild", ParentField: "parent_id"},
		},
	}
}

func twoSectionParentFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "TwoSectionParent",
		Version:    1,
		Sections: []form.Section{
			{Title: "Header", Component: form.ComponentFields, Fields: []form.FormField{{Name: "name", Label: "Name"}}},
			{Title: "Alphas", Component: form.ComponentMasterDetail, Target: "AlphaChild"},
			{Title: "Betas", Component: form.ComponentMasterDetail, Target: "BetaChild"},
		},
	}
}

func alphaChildEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "AlphaChild",
		Version:    1,
		Fields: []entity.Field{
			{Name: "parent_id", Type: entity.FieldString, Required: true},
			{Name: "label", Type: entity.FieldString, Required: true},
		},
	}
}

func alphaChildFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "AlphaChild",
		Version:    1,
		Sections:   []form.Section{{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "label", Label: "Label"}}}},
	}
}

func betaChildEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "BetaChild",
		Version:    1,
		Fields: []entity.Field{
			{Name: "parent_id", Type: entity.FieldString, Required: true},
			{Name: "label", Type: entity.FieldString, Required: true},
		},
	}
}

func betaChildFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "BetaChild",
		Version:    1,
		Sections:   []form.Section{{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "label", Label: "Label"}}}},
	}
}

// TestAPI_RenderRecordForm_MasterDetailSection_PerSectionDegradeIsIndependent
// proves the degrade is genuinely PER-SECTION: a viewer denied read on
// AlphaChild but granted read on BetaChild must lose only the Alphas
// section's row, not the Betas section's — independent review finding
// #3 on uc-infra#131 (the original fix's own tests never exercised a
// form with more than one master-detail section, so this property was
// asserted in a doc comment but never actually pinned by a test).
func TestAPI_RenderRecordForm_MasterDetailSection_PerSectionDegradeIsIndependent(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, twoSectionParentEntityDef(), twoSectionParentFormDef())
	publishEntityAndForm(t, db, alphaChildEntityDef(), alphaChildFormDef())
	publishEntityAndForm(t, db, betaChildEntityDef(), betaChildFormDef())

	engine := crud.NewEngine(db)
	parent, err := engine.Create(ctx, twoSectionParentEntityDef(), map[string]any{"name": "Parent One"}, humanActor())
	if err != nil {
		t.Fatalf("seed TwoSectionParent: %v", err)
	}
	if _, err := engine.Create(ctx, alphaChildEntityDef(), map[string]any{"parent_id": parent.ID, "label": "AlphaRowLabel"}, humanActor()); err != nil {
		t.Fatalf("seed AlphaChild: %v", err)
	}
	if _, err := engine.Create(ctx, betaChildEntityDef(), map[string]any{"parent_id": parent.ID, "label": "BetaRowLabel"}, humanActor()); err != nil {
		t.Fatalf("seed BetaChild: %v", err)
	}

	// clerk can read the parent and BetaChild, but NOT AlphaChild.
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "TwoSectionParent", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "AlphaChild", "can_read": false, "can_write": false},
			{"role": "clerk", "entity_type": "BetaChild", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/forms/TwoSectionParent/"+parent.ID, tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (degrade, not a page-wide 403), got %d:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "AlphaRowLabel") {
		t.Fatalf("expected the denied AlphaChild section's row to be omitted, got:\n%s", body)
	}
	if !strings.Contains(body, "BetaRowLabel") {
		t.Fatalf("expected the READABLE BetaChild section's row to still render — one denied section must not blank out its siblings, got:\n%s", body)
	}
}

// rollupOrderEntityDef/rollupOrderFormDef and rollupLineEntityDef/
// rollupLineFormDef are a MINIMAL dedicated fixture reproducing the
// exact shape that made the original uc-infra#131 fix corrupt data
// (independent review finding #1, fixed in the same commit as this
// test): an entity field ("total") that is BOTH an ordinary writable
// ComponentFields input AND a master-detail section's RollUpTarget.
// purchaseOrderEntityDef/purchaseOrderFormDef above do NOT have this
// shape — their "total" is roll-up-display text only, never a declared
// entity Field or a rendered <input>, so they can't reproduce this bug
// class. The REAL internal/kernel/purchasing.PurchaseOrder/
// PurchaseOrderForm has exactly this shape ("total" is both a Fields
// entry with Type: entity.FieldNumber and PurchaseOrderForm's Header
// section lists {Name: "total", Label: "Total"} right alongside the
// Lines section's RollUpTarget: "total") — but pulling in the full
// purchasing module (Party/PartyRole/Currency/Status seeding) just to
// prove this specific interaction would make the test slow and its
// intent harder to read, so this fixture is the smallest shape with
// the same vulnerability.
func rollupOrderEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "RollupOrder",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldString, Required: true},
			{Name: "total", Type: entity.FieldNumber, Default: float64(0)},
		},
		Relationships: []entity.Relationship{
			{Name: "lines", Kind: entity.RelationComposition, Target: "RollupLine", ParentField: "order_id"},
		},
	}
}

func rollupOrderFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "RollupOrder",
		Version:    1,
		Sections: []form.Section{
			{Title: "Header", Component: form.ComponentFields, Fields: []form.FormField{
				{Name: "vendor_id", Label: "Vendor"},
				{Name: "total", Label: "Total"},
			}},
			{Title: "Lines", Component: form.ComponentMasterDetail, Target: "RollupLine", RollUp: "line_total", RollUpTarget: "total"},
		},
	}
}

func rollupLineEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "RollupLine",
		Version:    1,
		Fields: []entity.Field{
			{Name: "order_id", Type: entity.FieldString, Required: true},
			{Name: "line_total", Type: entity.FieldNumber, Required: true},
		},
	}
}

func rollupLineFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "RollupLine",
		Version:    1,
		Sections:   []form.Section{{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "line_total", Label: "Line Total"}}}},
	}
}

// TestAPI_RenderRecordForm_MasterDetailSection_DegradedRollUpDoesNotCorruptOnSave
// is the regression test for the independent review's finding #1 on
// uc-infra#131: before that finding was fixed, a viewer denied read on
// a master-detail section's child type still got the parent form to
// render (the 403 fix working as intended) — but the roll-up total was
// silently recomputed to 0 over the section it couldn't load, and
// because RollUpTarget is an ordinary WRITABLE field (not a read-only
// computed one), saving the form — even to change something completely
// unrelated — persisted that 0 over the real total. This is what
// internal/kernel/purchasing/ledger.go's GL posting reads directly, so
// this was silent financial-data corruption, not a cosmetic gap.
func TestAPI_RenderRecordForm_MasterDetailSection_DegradedRollUpDoesNotCorruptOnSave(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, rollupOrderEntityDef(), rollupOrderFormDef())
	publishEntityAndForm(t, db, rollupLineEntityDef(), rollupLineFormDef())

	engine := crud.NewEngine(db)
	// Seed total ALREADY at its correct, previously-saved value —
	// simulating an order last saved by someone who COULD see the
	// lines. Nothing in this codebase recomputes a roll-up target
	// outside formrender's own render-time computation (no
	// create/update hook does — confirmed for the real
	// PurchaseOrder/POLine pair too), so this is exactly what a real
	// prior save would have left behind.
	order, err := engine.Create(ctx, rollupOrderEntityDef(), map[string]any{"vendor_id": "v1", "total": 150.5}, humanActor())
	if err != nil {
		t.Fatalf("seed RollupOrder: %v", err)
	}
	if _, err := engine.Create(ctx, rollupLineEntityDef(), map[string]any{"order_id": order.ID, "line_total": 150.5}, humanActor()); err != nil {
		t.Fatalf("seed RollupLine: %v", err)
	}

	// clerk can read+WRITE the parent but cannot read the child type at
	// all — an ordinary grant (e.g. a buyer who edits header fields but
	// line-level detail is restricted to a different role), exactly the
	// shape the independent review's finding #1 flagged.
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "RollupOrder", "can_read": true, "can_write": true},
			{"role": "clerk", "entity_type": "RollupLine", "can_read": false, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	formReq := newRequest("GET", "/forms/RollupOrder/"+order.ID, tenantID, "user-clerk", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if formRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", formRec.Code, formRec.Body.String())
	}
	body := formRec.Body.String()

	// The rendered "total" input must still show the real stored value
	// (150.5), not a silently recomputed 0 — this is the actual
	// regression the independent review caught: without the fix, this
	// asserts 0 here, and the POST below would then persist that 0
	// over the real total.
	const marker = `name="total" value="`
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatalf("expected a \"total\" number input in the rendered form, got:\n%s", body)
	}
	rest := body[idx+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end == -1 {
		t.Fatalf("malformed total input in rendered form:\n%s", body)
	}
	renderedTotal := rest[:end]
	if renderedTotal != "150.5" {
		t.Fatalf("expected the degraded form to still show the real stored total 150.5 (not a recomputed 0), got total=%q in:\n%s", renderedTotal, body)
	}

	// Simulate real client behavior: a browser resubmits whatever value
	// was in the rendered input regardless of whether the user touched
	// it — htmx's default form-urlencoded POST includes every visible
	// field. Only vendor_id is actually being changed here, modeling
	// "the clerk edited something else and hit Save."
	saveBody := "vendor_id=v2&total=" + renderedTotal
	saveReq := httptest.NewRequest("POST", "/api/records/RollupOrder/"+order.ID, strings.NewReader(saveBody))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveReq.Header.Set("X-Tenant-ID", tenantID)
	saveReq.Header.Set("X-Actor-ID", "user-clerk")
	saveRec := httptest.NewRecorder()
	mux.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("expected the clerk's save (they have RollupOrder write) to succeed, got %d: %s", saveRec.Code, saveRec.Body.String())
	}

	// The real proof: read the record straight back from the DB and
	// confirm total is still 150.5, not silently zeroed by the save
	// the clerk just made to an unrelated field.
	saved, err := engine.Get(ctx, rollupOrderEntityDef(), order.ID)
	if err != nil {
		t.Fatalf("re-fetch RollupOrder: %v", err)
	}
	if total, _ := saved.Data["total"].(float64); total != 150.5 {
		t.Fatalf("expected total to remain 150.5 after the clerk's save (a degraded Lines section must not corrupt it), got %v (full record: %v)", saved.Data["total"], saved.Data)
	}
}

// TestLoadMasterDetailChildren_DegradesOnErrDeniedNotOtherErrors is the
// direct/unit-level counterpart to the HTTP-level tests above: it calls
// loadMasterDetailChildren itself (same package, unexported method) so
// the degrade branch (including its 4th return value, the degraded-
// sections set formrender's roll-up fix above depends on) and the "a
// genuine error still propagates" branch are each pinned independently
// of formrender's HTML output.
func TestLoadMasterDetailChildren_DegradesOnErrDeniedNotOtherErrors(t *testing.T) {
	router := newTestRouter(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, db, poLineEntityDef(), poLineFormDef())

	engine := crud.NewEngine(db)
	po, err := engine.Create(ctx, purchaseOrderEntityDef(), map[string]any{"vendor_id": "v1"}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, poLineEntityDef(), map[string]any{"purchase_order_id": po.ID, "line_total": 42.0}, humanActor()); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "POLine", "can_read": false, "can_write": false},
		},
	)

	h := testHandler(t, router)
	deniedScope, err := h.scope(ctx, httpx.RequestContext{TenantID: tenantID, Actor: audit.Actor{Type: audit.ActorHuman, ID: "user-clerk"}})
	if err != nil {
		t.Fatalf("build tenantScope: %v", err)
	}

	mdc, err := h.loadMasterDetailChildren(ctx, deniedScope, purchaseOrderEntityDef(), purchaseOrderFormDef(), po.ID, "en")
	if err != nil {
		t.Fatalf("expected the POLine section (ErrDenied) to degrade, not hard-fail the whole call: %v", err)
	}
	if _, ok := mdc.rows["POLine"]; ok {
		t.Fatalf("expected the POLine section omitted from children when the viewer can't read it, got: %v", mdc.rows)
	}
	if _, ok := mdc.defs["POLine"]; ok {
		t.Fatalf("expected the POLine section omitted from childDefs when the viewer can't read it, got: %v", mdc.defs)
	}
	if _, ok := mdc.referenceLabels["POLine"]; ok {
		t.Fatalf("expected the POLine section omitted from referenceLabels when the viewer can't read it, got: %v", mdc.referenceLabels)
	}
	if !mdc.degraded["POLine"] {
		t.Fatalf("expected degraded[\"POLine\"] to be true — formrender's roll-up fix depends on this to distinguish \"couldn't load\" from \"genuinely zero rows\", got: %v", mdc.degraded)
	}

	// A genuine (non-authz) failure from ListByField itself must still
	// hard-fail the whole call, not silently degrade — the fix only
	// special-cases authz.ErrDenied via errors.Is, nothing else. Forced
	// deterministically by dropping the `records` table in a SEPARATE,
	// otherwise-unrestricted tenant (no RBAC configured, so this run
	// isn't exercising the denial path at all): `records` is the table
	// ListByField itself queries (internal/data/records.go), while
	// checkRead's own role lookup and the child Definition lookup
	// (ts.entityDef) both query DIFFERENT tables (roles/permissions,
	// entity_definitions) that are untouched — so this failure is
	// specifically ListByField's raw query, not some other call in the
	// function racing ahead of it. Confirmed below by asserting the
	// exact wrap-message prefix loadMasterDetailChildren's ListByField
	// error branch uses.
	failTenantID, failDB := newTestTenant(t, router)
	if err := foundation.Publish(ctx, failDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish (fail tenant): %v", err)
	}
	publishEntityAndForm(t, failDB, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, failDB, poLineEntityDef(), poLineFormDef())
	failEngine := crud.NewEngine(failDB)
	failPO, err := failEngine.Create(ctx, purchaseOrderEntityDef(), map[string]any{"vendor_id": "v1"}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder (fail tenant): %v", err)
	}
	failScope, err := h.scope(ctx, httpx.RequestContext{TenantID: failTenantID, Actor: humanActor()})
	if err != nil {
		t.Fatalf("build tenantScope (fail tenant): %v", err)
	}
	// Sanity check first, before corrupting the schema: this actor/
	// tenant combination succeeds normally — otherwise a failure below
	// could just as easily mean "this actor is denied too" as "a
	// genuine error propagates," which would prove nothing.
	if _, err := h.loadMasterDetailChildren(ctx, failScope, purchaseOrderEntityDef(), purchaseOrderFormDef(), failPO.ID, "en"); err != nil {
		t.Fatalf("sanity check: fail tenant should succeed before the table is dropped: %v", err)
	}
	// CASCADE: record_unique_keys (0006_record_unique_keys.sql) holds a
	// FK to records(id), which blocks a plain DROP TABLE. This tenant
	// database is disposable and used only for this one assertion, so
	// dropping its dependents along with it is fine.
	if _, err := failDB.ExecContext(ctx, "DROP TABLE records CASCADE"); err != nil {
		t.Fatalf("drop records table: %v", err)
	}
	_, err = h.loadMasterDetailChildren(ctx, failScope, purchaseOrderEntityDef(), purchaseOrderFormDef(), failPO.ID, "en")
	if err == nil {
		t.Fatalf("expected a genuine (non-ErrDenied) ListByField failure to still propagate as an error, got nil")
	}
	if errors.Is(err, authz.ErrDenied) {
		t.Fatalf("expected a real DB failure, not an authz denial: %v", err)
	}
	if !strings.Contains(err.Error(), "list POLine children") {
		t.Fatalf("expected the error to come from loadMasterDetailChildren's own ListByField wrap (\"list POLine children: ...\"), not some other call, got: %v", err)
	}
}
