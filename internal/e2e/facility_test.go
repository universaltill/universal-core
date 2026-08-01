package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
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
