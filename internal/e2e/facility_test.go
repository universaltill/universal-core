package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestFacility_ListFormAndInventoryPicker (#12) drives the surface the
// facility dimension actually exposes to a user, in a real browser.
//
// The assertion that matters is the last one: an InventoryItem form
// must show its facility by NAME. facility_id is a required reference
// on an entity a user edits directly, and the raw-UUID rendering that
// shipped with Employee (#16) is precisely the regression a
// definition-shape test cannot see — the Definition is correct in both
// the working and the broken case.
func TestFacility_ListFormAndInventoryPicker(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	engine := crud.NewEngine(tenantDB)
	def := func(entityType string) *entity.Definition {
		t.Helper()
		v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}

	facilityDef := def("Facility")
	main, err := engine.Create(ctx, facilityDef, map[string]any{
		"code": "MAIN", "name": "Main Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Facility: %v", err)
	}
	if _, err := engine.Create(ctx, facilityDef, map[string]any{
		"code": "STORE-01", "name": "Doha Retail Store", "facility_type": "store", "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create second Facility: %v", err)
	}

	item, err := engine.Create(ctx, def("Item"), map[string]any{
		"sku": "SKU-E2E-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	inv, err := engine.Create(ctx, def("InventoryItem"), map[string]any{
		"item_id": item.ID, "facility_id": main.ID,
		"qty_on_hand": 250.0, "qty_available_to_promise": 250.0,
	}, actor)
	if err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	browser := browserCtx(t, tenantID)

	// The facility list renders both facilities with their type
	// resolved to a label, not the raw enum code.
	var listText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/Facility"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render facility list: %v", err)
	}
	for _, want := range []string{"Main Warehouse", "Doha Retail Store"} {
		if !strings.Contains(listText, want) {
			t.Errorf("facility list is missing %q:\n%s", want, listText)
		}
	}
	if !strings.Contains(listText, "Warehouse") || !strings.Contains(listText, "Store") {
		t.Errorf("facility_type must render as a label, not a raw enum code:\n%s", listText)
	}

	// The facility form round-trips its type and active flag.
	var facilityType string
	var isActive bool
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Facility/"+main.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`select[name="facility_type"]`, &facilityType, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('form.uc-form input[name="is_active"][type="checkbox"]').checked`, &isActive),
	); err != nil {
		t.Fatalf("render facility form: %v", err)
	}
	if facilityType != "warehouse" {
		t.Errorf("facility_type select = %q, want the stored value warehouse", facilityType)
	}
	if !isActive {
		t.Error("is_active must render checked for an active facility")
	}

	// The point of the card, in the browser: an InventoryItem shows
	// which facility its stock sits in, by name.
	var facilityLabel string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/InventoryItem/"+inv.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
			const wrap = document.querySelector('form.uc-form .uc-ref[data-field="facility_id"]');
			if (!wrap) return '<no facility picker on the inventory form>';
			const box = wrap.querySelector('.uc-ref-search');
			return box ? box.value : '<no search box>';
		})()`, &facilityLabel),
	); err != nil {
		t.Fatalf("render inventory item form: %v", err)
	}
	if facilityLabel != "Main Warehouse" {
		t.Errorf("facility picker shows %q, want the facility name — a UUID here is the #16 regression", facilityLabel)
	}
}

// TestGoodsReceiptForm_FacilityPickerRendersByName (uc-infra#54) is
// TestFacility_ListFormAndInventoryPicker's own reasoning applied to
// GoodsReceipt's new required facility_id (v2): a Definition-shape test
// (purchasing_test.go's TestGoodsReceiptForm_HeaderIncludesFacility)
// proves the field exists on the form, never that the reference picker
// actually renders the facility by name rather than a raw UUID in the
// real browser a buyer uses to log a receipt. Both entities got the
// facility dimension in the same doc comment (ADR-0015 §5); only
// InventoryItem had browser coverage for it until now.
func TestGoodsReceiptForm_FacilityPickerRendersByName(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	// testServer publishes entities/forms but not the status graph —
	// PurchaseOrder.status_id needs real Status records to point at
	// (same setup purchasing_report_test.go's own comment describes).
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	facility, err := engine.Create(ctx, purchasing.Facility(), map[string]any{
		"code": "MAIN", "name": "Main Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Facility: %v", err)
	}
	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "GR E2E Vendor", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create vendor Party: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("create vendor PartyRole: %v", err)
	}
	draftID := publishedStatusID(t, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-E2E-1", "vendor_id": vendor.ID, "order_date": "2026-01-01",
		"status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	// No GoodsReceiptLine created — this test only needs the header form
	// to render, not the ledger/InventoryItem-crediting hook, which has
	// its own dedicated coverage in internal/kernel/purchasing/ledger_test.go.
	gr, err := engine.Create(ctx, purchasing.GoodsReceipt(), map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-01-10", "facility_id": facility.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}

	browser := browserCtx(t, tenantID)
	var facilityLabel string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/GoodsReceipt/"+gr.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
			const wrap = document.querySelector('form.uc-form .uc-ref[data-field="facility_id"]');
			if (!wrap) return '<no facility picker on the goods receipt form>';
			const box = wrap.querySelector('.uc-ref-search');
			return box ? box.value : '<no search box>';
		})()`, &facilityLabel),
	); err != nil {
		t.Fatalf("render goods receipt form: %v", err)
	}
	if facilityLabel != "Main Warehouse" {
		t.Errorf("facility picker shows %q, want the facility name — a raw UUID here is the same #16-class regression TestFacility_ListFormAndInventoryPicker guards for InventoryItem", facilityLabel)
	}
}

// TestFacility_NewFormChecksActiveByDefault (uc-infra#206) is the real-
// browser proof that a FieldBool's declared Default now actually reaches
// a brand-new record's form. A rendered-HTML-string test can't stand in
// for this: it would prove the `checked` attribute is present in the
// markup formrender emitted, never that the browser itself treats the
// checkbox as checked, or — the part that actually matters to an admin —
// that submitting the form without touching the checkbox at all persists
// is_active=true rather than the false a checkbox's ordinary unchecked-
// by-default HTML semantics would otherwise silently store (the sibling
// gap finance's TestSyncGLAccountOnWrite_IsActiveOmittedOnCreate_
// StoresActive documents one layer down, for Account specifically —
// this is the same fix's UI-facing half, for Facility).
func TestFacility_NewFormChecksActiveByDefault(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := browserCtx(t, tenantID)

	var isActiveOnLoad bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Facility/new"),
		chromedp.WaitVisible(`form.uc-form input[name="is_active"][type="checkbox"]`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('form.uc-form input[name="is_active"][type="checkbox"]')?.checked === true`, &isActiveOnLoad),
	); err != nil {
		t.Fatalf("render new Facility form: %v", err)
	}
	if !isActiveOnLoad {
		t.Fatal("expected is_active to render checked on a brand-new Facility form (Default: true), got unchecked")
	}

	// Fill only what's required, deliberately never touching the Active
	// checkbox — the real gesture (or non-gesture) that reached
	// ledger.ErrInactiveAccount for Account before uc-infra#206.
	// facility_type is left alone too: its own Default ("warehouse", a
	// FieldEnum) was already honored before this fix.
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`input[name="code"]`, "MAIN", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Main Warehouse", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save new Facility: %v", err)
	}
	id := savedRecordID(t, ctx, "Facility")

	// crud.Engine.Get only reads def.EntityType (crud.go), so the plain
	// Facility() Definition constructor is equivalent to (and cheaper
	// than) fetching and unmarshaling the published Definition here.
	got, err := crud.NewEngine(tenantDB).Get(context.Background(), purchasing.Facility(), id)
	if err != nil {
		t.Fatalf("read back Facility after save: %v", err)
	}
	if isActive, _ := got.Data["is_active"].(bool); !isActive {
		t.Fatal("expected the saved Facility to persist is_active=true without the checkbox ever being touched, got false")
	}

	// Reload from scratch, same reasoning as TestDelegation_RealBrowser:
	// proves the stored value round-trips back to a checked box, not
	// just that the unreloaded page still shows what was typed.
	var isActiveAfterReload bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Facility/"+id),
		chromedp.WaitVisible(`form.uc-form input[name="is_active"][type="checkbox"]`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('form.uc-form input[name="is_active"][type="checkbox"]')?.checked === true`, &isActiveAfterReload),
	); err != nil {
		t.Fatalf("reload saved Facility form: %v", err)
	}
	if !isActiveAfterReload {
		t.Fatal("expected is_active to reload checked after save, got unchecked")
	}
}
