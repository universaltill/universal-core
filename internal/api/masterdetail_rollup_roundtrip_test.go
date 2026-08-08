package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// purchaseOrderWithMoneyTotalEntityDef/purchaseOrderWithMoneyTotalFormDef
// are a variant of purchaseOrderEntityDef/purchaseOrderFormDef
// (handlers_test.go) that additionally declares the "total" field itself,
// typed entity.FieldMoney — matching the real shipped Definition
// (internal/kernel/purchasing.PurchaseOrder's own "total" field, and
// POLine's "line_total") rather than a plain FieldNumber. The shared
// fixtures omit "total" entirely — no other test in this file needs to
// read or write it — but TestAPI_UnavailableSectionRollUpFieldSurvives
// SaveRoundTrip below specifically exercises that RollUpTarget field
// surviving a real save round trip, so it needs somewhere to actually
// live on the record, and it needs to be FieldMoney: render.go's own
// comment on the RollUp loop states a coherent Definition never mixes a
// FieldMoney header total with a plain-FieldNumber child roll-up field,
// and the money branch (minor-units storage <-> major-unit "10.50"
// display, money.FromAny/money.ParseString) is exactly where a save
// round trip has historically corrupted a preserved value 100x
// (hiddenFieldValue's own doc comment) — a FieldNumber fixture would
// silently skip that branch entirely.
func purchaseOrderWithMoneyTotalEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldString, Required: true},
			{Name: "total", Type: entity.FieldMoney},
		},
		Relationships: []entity.Relationship{
			{Name: "lines", Kind: entity.RelationComposition, Target: "POLine", ParentField: "purchase_order_id"},
		},
	}
}

func purchaseOrderWithMoneyTotalFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []form.Section{
			{Title: "Header", Component: form.ComponentFields, Fields: []form.FormField{
				{Name: "vendor_id", Label: "Vendor"},
				{Name: "total", Label: "Total"},
			}},
			{Title: "Lines", Component: form.ComponentMasterDetail, Target: "POLine", RollUp: "line_total", RollUpTarget: "total"},
		},
	}
}

// poLineMoneyEntityDef/poLineMoneyFormDef mirror poLineEntityDef/
// poLineFormDef (handlers_test.go) but type line_total as
// entity.FieldMoney to match purchaseOrderWithMoneyTotalEntityDef's
// "total" above — see that fixture's own doc comment for why the two
// have to agree.
func poLineMoneyEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "POLine",
		Version:    1,
		Fields: []entity.Field{
			{Name: "purchase_order_id", Type: entity.FieldString, Required: true},
			{Name: "line_total", Type: entity.FieldMoney, Required: true},
		},
	}
}

func poLineMoneyFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "POLine",
		Version:    1,
		Sections: []form.Section{
			{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "line_total", Label: "Line Total"}}},
		},
	}
}

var (
	renderedTotalValue   = regexp.MustCompile(`name="total" value="([^"]*)"`)
	renderedVersionValue = regexp.MustCompile(`name="_version" value="([^"]*)"`)
)

// TestAPI_UnavailableSectionRollUpFieldSurvivesSaveRoundTrip (uc-infra#162,
// split from uc-infra#132's independent review): #132 proved, at the
// formrender unit level, that a RollUp target field like
// PurchaseOrder.total keeps its last-saved value instead of being
// recomputed to 0 once its source section becomes unavailable
// (TestRender_UnavailableSectionRollUpFieldKeepsSavedValue in
// internal/kernel/formrender). What that left unproven is the scenario
// that actually matters for data safety: a tenant that legitimately had
// POLine licensed, accumulated a real total, then had it become
// unavailable (module downgrade, licensing change, or any other path to
// data.ErrNotFound) — does an ordinary re-save of the PurchaseOrder form
// afterward still keep that total, or does the kernel's
// full-record-replacement update (buildHiddenFields's own doc comment:
// a save is a full replacement, not a merge) silently wipe it?
//
// This drives the real handler end to end rather than inferring the
// answer from the render mechanism alone: GET the form and parse the
// ACTUAL rendered value out of the HTML (not just assume it), POST that
// value back application/x-www-form-urlencoded — the real content type
// formrender's own <form> submits (parseRecordFields' doc comment; a
// bare JSON POST exercises a different code path a browser save never
// takes) — with the same "_version" hidden field a real save carries,
// then re-GET the record through the JSON API and confirm the stored
// total survived.
//
// "POLine becomes unavailable" is simulated via
// EntityDefinitionRepo.Rollback — the registry's own modeled
// published->rolled_back status transition (see data/definitions.go's
// status-machine comment), not a raw-SQL test-only unpublish helper.
// There is no module install/uninstall mechanism in this codebase yet
// (grepped: no production caller of Rollback exists) — Rollback is the
// closest existing modeled transition to "this tenant's POLine module
// went away," not itself the mechanism a real downgrade would use.
// uc-infra#132's own regression test
// (TestAPI_RenderRecordForm_UnlicensedMasterDetailChildOmitsSectionNot404
// in handlers_test.go) covers the adjacent "never published at all"
// shape; this one is specifically the "was available, then wasn't"
// downgrade shape the issue is about.
func TestAPI_UnavailableSectionRollUpFieldSurvivesSaveRoundTrip(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	entRepo := data.NewEntityDefinitionRepo(tenantDB)
	publishEntityAndForm(t, tenantDB, purchaseOrderWithMoneyTotalEntityDef(), purchaseOrderWithMoneyTotalFormDef())
	publishEntityAndForm(t, tenantDB, poLineMoneyEntityDef(), poLineMoneyFormDef())

	// 35050 minor units = $350.50 — real Money storage, not a display
	// string; render.go's FieldMoney case converts this to "350.50" for
	// the form input, and back again on save via money.ParseString.
	const totalMinorUnits = 35050
	engine := crud.NewEngine(tenantDB)
	po, err := engine.Create(ctx, purchaseOrderWithMoneyTotalEntityDef(), map[string]any{"vendor_id": "v1", "total": totalMinorUnits}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, poLineMoneyEntityDef(), map[string]any{
		"purchase_order_id": po.ID, "line_total": totalMinorUnits,
	}, humanActor()); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	// Simulate POLine's module becoming unavailable AFTER the total was
	// legitimately accumulated — the "downgrade" scenario uc-infra#162 is
	// about, as opposed to #132's own test which never published POLine
	// in the first place.
	if err := entRepo.Rollback(ctx, "POLine", 1, humanActor()); err != nil {
		t.Fatalf("roll back POLine definition: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	getReq := newRequest("GET", "/forms/PurchaseOrder/"+po.ID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	body := getRec.Body.String()
	if strings.Contains(body, "Lines") {
		t.Fatalf("expected the now-unavailable Lines section to be omitted from the rendered form, got:\n%s", body)
	}
	totalMatch := renderedTotalValue.FindStringSubmatch(body)
	if totalMatch == nil {
		t.Fatalf("expected a rendered total input in the form, got:\n%s", body)
	}
	if totalMatch[1] != "350.50" {
		t.Fatalf("expected the rendered total to preserve 350.50 rather than being recomputed to 0.00, got %q", totalMatch[1])
	}
	versionMatch := renderedVersionValue.FindStringSubmatch(body)
	if versionMatch == nil {
		t.Fatalf("expected a rendered _version hidden field in the form, got:\n%s", body)
	}

	// A real save resubmits every visible field's current value, plus the
	// hidden "_version" field, as application/x-www-form-urlencoded — the
	// exact content type and shape formrender's own <form> produces (see
	// this test's own doc comment) — not a hand-built JSON body. This
	// kernel's update is a full record replacement, not a merge (see
	// buildHiddenFields's own doc comment), so posting back exactly what
	// the GET above rendered is what an unmodified browser save sends.
	postBody := url.Values{
		"vendor_id": {"v1"},
		"total":     {totalMatch[1]},
		"_version":  {versionMatch[1]},
	}.Encode()
	updateReq := newRequest("POST", "/api/records/PurchaseOrder/"+po.ID, tenantID, "farshid", []byte(postBody))
	updateReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	getRecReq := newRequest("GET", "/api/records/PurchaseOrder/"+po.ID, tenantID, "farshid", nil)
	getRecRec := httptest.NewRecorder()
	mux.ServeHTTP(getRecRec, getRecReq)
	if getRecRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRecRec.Code, getRecRec.Body.String())
	}
	var stored struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(getRecRec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("unmarshal record response: %v", err)
	}
	if got, ok := stored.Data.Data["total"].(float64); !ok || got != totalMinorUnits {
		t.Errorf("expected the save round trip to preserve total=%d minor units, not wipe it, got %v", totalMinorUnits, stored.Data.Data["total"])
	}
}
