package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// The purchasing report reads straight off the records table
// (internal/data/reporting.go) — unlike every other page in this
// package, it never looks anything up in the Definition registry, so
// these tests seed raw records directly rather than going through
// publishEntityAndForm/the CRUD API first.

func TestAPI_PurchasingReport_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/reports/purchasing", nil) // no X-Tenant-ID/X-Actor-ID
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_PurchasingReport_TenantScopedAndEscapesRecordData is the one
// highest-stakes check for a page whose entire content is aggregated
// business data: tenant B's purchase order must never influence tenant
// A's report, and a vendor name containing raw HTML/script content
// (plausible — Party.name is free-text, reachable via CSV import) must
// render escaped, not executable.
func TestAPI_PurchasingReport_TenantScopedAndEscapesRecordData(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	_, dbB := newTestTenant(t, router)
	recordsA := data.NewRecordRepo(dbA)
	recordsB := data.NewRecordRepo(dbB)
	ctx := context.Background()

	statusA, err := recordsA.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status for tenant A: %v", err)
	}
	statusB, err := recordsB.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status for tenant B: %v", err)
	}

	vendorA, err := recordsA.Create(ctx, "Party", map[string]any{
		"name": `Acme" onmouseover="alert(1)<script>alert(2)</script>`, "party_type": "organization",
	})
	if err != nil {
		t.Fatalf("create Party for tenant A: %v", err)
	}
	if _, err := recordsA.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-A1", "vendor_id": vendorA.ID, "status_id": statusA.ID, "total": 1234.5,
	}); err != nil {
		t.Fatalf("create PurchaseOrder for tenant A: %v", err)
	}

	vendorB, err := recordsB.Create(ctx, "Party", map[string]any{"name": "Tenant B Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party for tenant B: %v", err)
	}
	if _, err := recordsB.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-B1", "vendor_id": vendorB.ID, "status_id": statusB.ID, "total": 999999.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder for tenant B: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/reports/purchasing", tenantA, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, "999999") || strings.Contains(body, "Tenant B Vendor") {
		t.Errorf("tenant B's data leaked into tenant A's report:\n%s", body)
	}
	if !strings.Contains(body, "1234.5") {
		t.Errorf("expected tenant A's own PurchaseOrder total (1234.5) in the report:\n%s", body)
	}
	if strings.Contains(body, "<script>alert(2)</script>") {
		t.Errorf("vendor name rendered as raw, unescaped HTML — XSS: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(2)&lt;/script&gt;") {
		t.Errorf("expected the vendor name's script tag HTML-escaped in the output:\n%s", body)
	}
}

// TestAPI_PurchasingReport_StockoutRiskAndEmptyStates covers the stock
// side (a join through InventoryItem -> Item, unlike the vendor table's
// join through PurchaseOrder -> Party) and confirms both empty states
// (no vendors, no stockouts) render their own message instead of an
// empty table.
func TestAPI_PurchasingReport_StockoutRiskAndEmptyStates(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	records := data.NewRecordRepo(db)
	ctx := context.Background()

	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-EMPTY", "name": "Out of Stock Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	if _, err := records.Create(ctx, "InventoryItem", map[string]any{
		"item_id": item.ID, "qty_on_hand": 0, "qty_available_to_promise": 0,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/reports/purchasing", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "SKU-EMPTY") || !strings.Contains(body, "Out of Stock Widget") {
		t.Errorf("expected the stockout-risk item in the report:\n%s", body)
	}
	if !strings.Contains(body, "No purchase orders yet.") {
		t.Errorf("expected the vendor-empty-state message (no POs seeded for this tenant):\n%s", body)
	}
}

// TestAPI_PurchasingReport_EntityTypeListMatchesTheActualQueries pins
// purchasingReportEntityTypes to a hardcoded, independently-derived set
// so a change to the var — an addition OR an omission — fails loudly
// here instead of silently. This is NOT redundant with the table-driven
// test below: that test only proves each LISTED type is enforced: it
// iterates the var itself, so an unlisted type (independent review,
// 2026-07-30: "Status" was missing from the first draft, joined by
// PurchaseOrderStatusBreakdown but never checked) produces no subtest at
// all and would pass silently forever. This test is what actually
// catches that class of drift.
func TestAPI_PurchasingReport_EntityTypeListMatchesTheActualQueries(t *testing.T) {
	// Independently re-derived from internal/data/reporting.go's SQL, not
	// copied from purchasingReportEntityTypes: PurchaseOrderStatusBreakdown
	// reads PurchaseOrder and joins Status; TopVendorsBySpend reads
	// PurchaseOrder and joins Party; StockSummary reads InventoryItem;
	// StockoutRiskItems reads InventoryItem and joins Item.
	want := map[string]bool{"PurchaseOrder": true, "Status": true, "Party": true, "InventoryItem": true, "Item": true}
	got := make(map[string]bool, len(purchasingReportEntityTypes))
	for _, et := range purchasingReportEntityTypes {
		got[et] = true
	}
	if len(got) != len(purchasingReportEntityTypes) {
		t.Fatalf("purchasingReportEntityTypes has a duplicate entry: %v", purchasingReportEntityTypes)
	}
	for et := range want {
		if !got[et] {
			t.Errorf("purchasingReportEntityTypes is missing %q, which the SQL reads from", et)
		}
	}
	for et := range got {
		if !want[et] {
			t.Errorf("purchasingReportEntityTypes names %q, which no query in internal/data/reporting.go reads from — either the SQL changed and this test's `want` set needs updating, or the entry is stale", et)
		}
	}
}

// TestAPI_PurchasingReport_DeniedWhenAnyUnderlyingTypeIsRestricted is the
// ADR-0006 addendum's regression test: a role denied read on any ONE of
// the entity types the report aggregates must be refused the whole page,
// not served a partial or full report. Table-driven over
// purchasingReportEntityTypes, so it only proves each LISTED type is
// enforced — see TestAPI_PurchasingReport_EntityTypeListMatchesTheActual
// Queries above for the check that catches an unlisted type.
func TestAPI_PurchasingReport_DeniedWhenAnyUnderlyingTypeIsRestricted(t *testing.T) {
	for _, restricted := range purchasingReportEntityTypes {
		t.Run(restricted, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)

			seedRBAC(t, db,
				map[string][]string{"outsider": {"user-outsider"}},
				[]map[string]any{{"role": "outsider", "entity_type": restricted, "can_read": false, "can_write": false}},
			)

			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)
			rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-outsider")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("role denied read on %s: expected 403, got %d: %s", restricted, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "Access denied") {
				t.Fatalf("not the rendered denial page:\n%s", rec.Body.String())
			}
		})
	}
}

// TestAPI_PurchasingReport_DeniedOnUnrelatedTypeStillSeesReport is the
// negative control the table-driven denial test above can't provide by
// itself: it proves the gate is scoped to purchasingReportEntityTypes,
// not "any Permission row anywhere makes this report 403." A role denied
// read on an entity type the report never touches (Employee — no query
// in internal/data/reporting.go reads it) must still see the real
// report. Without this, an over-broad gate (deny-on-anything-denies-the-
// report) would pass every other test in this file.
func TestAPI_PurchasingReport_DeniedOnUnrelatedTypeStillSeesReport(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	records := data.NewRecordRepo(db)

	st, err := records.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Demo Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-UNRELATED", "vendor_id": vendor.ID, "status_id": st.ID, "total": 314.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	seedRBAC(t, db,
		map[string][]string{"outsider": {"user-outsider"}},
		[]map[string]any{{"role": "outsider", "entity_type": "Employee", "can_read": false, "can_write": false}},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-outsider")
	if rec.Code != http.StatusOK {
		t.Fatalf("role denied on an unrelated type (Employee): expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "314") {
		t.Fatalf("expected the seeded PurchaseOrder total in the report:\n%s", rec.Body.String())
	}
}

// TestAPI_PurchasingReport_AdminAndUnrestrictedRolesStillSeeIt covers the
// two ways this gate must NOT trigger: tenant_admin always passes
// (ADR-0006's lockout-prevention convention), and a role granted read on
// every underlying type sees the real report, not just a 403 avoided.
func TestAPI_PurchasingReport_AdminAndUnrestrictedRolesStillSeeIt(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()

	statusRow, err := data.NewRecordRepo(db).Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	vendor, err := data.NewRecordRepo(db).Create(ctx, "Party", map[string]any{"name": "Acme", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := data.NewRecordRepo(db).Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "status_id": statusRow.ID, "total": 42.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	perms := make([]map[string]any, len(purchasingReportEntityTypes))
	for i, et := range purchasingReportEntityTypes {
		perms[i] = map[string]any{"role": "reader", "entity_type": et, "can_read": true, "can_write": false}
	}
	seedRBAC(t, db,
		map[string][]string{"reader": {"user-reader"}, authz.AdminRoleCode: {"user-admin"}},
		perms,
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	for _, actor := range []string{"user-reader", "user-admin"} {
		rec := getAs(t, mux, "/reports/purchasing", tenantID, actor)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", actor, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "42") {
			t.Fatalf("%s: expected the seeded PurchaseOrder total in the report:\n%s", actor, rec.Body.String())
		}
	}
}

// TestAPI_PurchasingReport_StatusDeniedButOthersGrantedStillBlocksReport
// is the exact regression independent review found live: a role granted
// read on PurchaseOrder/Party/InventoryItem/Item but explicitly denied
// on Status must still be refused the whole report, not served the
// status breakdown with everything else intact.
// PurchaseOrderStatusBreakdown (internal/data/reporting.go) joins
// PurchaseOrder to Status to read the human-readable status code — a
// role that cannot read Status cannot legitimately see that join's
// output, even though Status is not the type the URL/report is "about."
// Distinct from the table-driven denial test above (which denies one
// type at a time, proving the OTHER types don't accidentally deny too)
// — this proves the specific leak: granting every type the report is
// nominally about while denying the join target it also silently reads.
func TestAPI_PurchasingReport_StatusDeniedButOthersGrantedStillBlocksReport(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	records := data.NewRecordRepo(db)

	statusRow, err := records.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Demo Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-STATUS-LEAK", "vendor_id": vendor.ID, "status_id": statusRow.ID, "total": 7777.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	seedRBAC(t, db,
		map[string][]string{"probe": {"user-probe"}},
		[]map[string]any{
			{"role": "probe", "entity_type": "Status", "can_read": false, "can_write": false},
			{"role": "probe", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "probe", "entity_type": "Party", "can_read": true, "can_write": false},
			{"role": "probe", "entity_type": "InventoryItem", "can_read": true, "can_write": false},
			{"role": "probe", "entity_type": "Item", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-probe")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("role denied Status read (everything else granted): expected 403, got %d — the report leaked status-joined data despite the denial:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Access denied") {
		t.Fatalf("not the rendered denial page:\n%s", rec.Body.String())
	}
}

// purchaseOrderModuleEntityDef/FormDef exist only so accessibleModules
// has something to group under the "purchasing" module key — the report
// link itself needs no Definition (ReportingRepo reads raw records), but
// reaching /modules/purchasing at all requires at least one published
// entity+form declaring Module "purchasing" (see accessibleModules' own
// doc comment). Deliberately minimal; not the real purchasing.PurchaseOrder
// Definition, which this package's tests never import directly.
func purchaseOrderModuleEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Module:     "purchasing",
		Fields:     []entity.Field{{Name: "po_number", Type: entity.FieldString, Required: true}},
	}
}

func purchaseOrderModuleFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Details",
			Component: form.ComponentFields,
			Fields:    []form.FormField{{Name: "po_number", Label: "PO Number"}},
		}},
	}
}

// TestAPI_PurchasingReport_ModuleMenuHidesReportLinkWhenDenied is the
// regression test for the gap independent review found: /modules/
// purchasing offered the report link unconditionally, so a user denied
// read on any purchasingReportEntityTypes entry saw a link that led
// straight to a guaranteed 403 — precisely the "tile that leads to a
// 403" outcome dashboard.go's own CanRead filtering exists to prevent
// for entity nodes, just not previously applied to report links.
func TestAPI_PurchasingReport_ModuleMenuHidesReportLinkWhenDenied(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, purchaseOrderModuleEntityDef(), purchaseOrderModuleFormDef())

	var grantedPerms, deniedPerms []map[string]any
	for _, et := range purchasingReportEntityTypes {
		grantedPerms = append(grantedPerms, map[string]any{"role": "reader", "entity_type": et, "can_read": true, "can_write": false})
		// Every type granted EXCEPT "Item" — deliberately NOT PurchaseOrder,
		// which is the only type published under the "purchasing" module
		// here: denying it would also hide the module's own hub node
		// (accessibleModules' CanRead filtering, dashboard.go), collapsing
		// the module to a 404 and proving nothing about the report link
		// specifically. Denying an entity type the report needs but the
		// module page doesn't isolate the exact thing this test targets.
		deniedPerms = append(deniedPerms, map[string]any{"role": "denied", "entity_type": et, "can_read": et != "Item", "can_write": false})
	}
	seedRBAC(t, db,
		map[string][]string{"reader": {"user-reader"}, "denied": {"user-denied"}},
		append(grantedPerms, deniedPerms...),
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	granted := getAs(t, mux, "/modules/purchasing", tenantID, "user-reader")
	if granted.Code != http.StatusOK {
		t.Fatalf("user-reader module menu: expected 200, got %d: %s", granted.Code, granted.Body.String())
	}
	if !strings.Contains(granted.Body.String(), "/reports/purchasing") {
		t.Fatalf("user-reader (full read access) lost the report link:\n%s", granted.Body.String())
	}

	denied := getAs(t, mux, "/modules/purchasing", tenantID, "user-denied")
	if denied.Code != http.StatusOK {
		t.Fatalf("user-denied module menu: expected 200 (PurchaseOrder itself is still granted, so the module's own hub node stays reachable), got %d: %s", denied.Code, denied.Body.String())
	}
	if !strings.Contains(denied.Body.String(), "/records/PurchaseOrder") {
		t.Fatalf("user-denied lost the PurchaseOrder hub node too — this test no longer isolates the report link specifically:\n%s", denied.Body.String())
	}
	if strings.Contains(denied.Body.String(), "/reports/purchasing") {
		t.Fatalf("user-denied (Item read denied, a report dependency) still saw the report link, which leads to a guaranteed 403:\n%s", denied.Body.String())
	}
}
