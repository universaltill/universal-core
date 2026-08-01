package e2e

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/testexec"
)

// stockTransferTestServer is testServer (csv_import_test.go) plus
// purchasing.PublishStatuses and the "StockTransfer" hook (purchasing.
// ValidateStockTransfer) registered on the Handler — the shared
// testServer() deliberately registers NO hooks at all (its own doc
// comment doesn't say so explicitly, but api.New(...) there is never
// followed by a RegisterHook call), so it can't be reused as-is for a
// test that needs to prove a hook actually fires through a real browser
// submission. Built as its own local server, not a change to the shared
// helper, to keep zero blast radius on every other e2e test that reuses
// testServer() (registering purchasing's ledger-posting hooks globally
// there would require every other test seeding GL accounts it doesn't
// need).
func stockTransferTestServer(t *testing.T) (srv *httptest.Server, tenantID string, tenantDB *sql.DB) {
	t.Helper()
	router := newTestRouter(t)
	ctx := context.Background()
	actor := humanActor()

	id, err := router.Create(ctx, "E2E Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	handler := api.New(router, catalog, nil, nil, nil, nil, nil)
	handler.RegisterHook("StockTransfer", purchasing.ValidateStockTransfer)
	mux := http.NewServeMux()
	handler.Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, id, tenantDB
}

// setReferenceHiddenValue sets a reference field's hidden id input
// directly via its JS .value property (not chromedp.SetValue, which
// targets the visible .uc-ref-search text box — a different input with
// no name attribute at all, formrender/render.go's own tmplSrc; only the
// hidden input actually round-trips on submit). This deliberately
// bypasses the searchable-picker UI (reference_picker_test.go already
// covers that interaction on its own) — this test's own subject is
// StockTransfer's validation hook, not the picker widget.
//
// Also fills the visible .uc-ref-search box with a placeholder label:
// that box carries HTML5 `required` (render.go's tmplSrc) whenever the
// field is Required, and leaving it empty silently blocks the browser's
// own native constraint validation from ever submitting the form at all
// — no request, no htmx event, just a click that does nothing (the bug
// this comment exists to stop a future edit from reintroducing). Its
// content is otherwise irrelevant to this test: it has no name
// attribute and never reaches the server.
func setReferenceHiddenValue(t *testing.T, ctx context.Context, field, id string) {
	t.Helper()
	sel := `.uc-ref[data-field="` + cssAttr(field) + `"]`
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`(() => {
			const wrap = document.querySelector(`+jsQuote(sel)+`);
			wrap.querySelector('input[type="hidden"]').value = `+jsQuote(id)+`;
			wrap.querySelector('.uc-ref-search').value = `+jsQuote(id)+`;
		})()`, nil,
	)); err != nil {
		t.Fatalf("set reference hidden value for %s: %v", field, err)
	}
}

func jsQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// TestStockTransfer_CreateRealBrowser (#13) drives StockTransfer's
// validation hook (ValidateStockTransfer, ledger.go) through real
// headless browser submissions — the thing a rendered-HTML-string test
// can't prove: that a same-facility submission is genuinely rejected
// with a visible error, not a raw 500 or a blank swap; that a valid
// submission genuinely persists end to end (form -> real HTTP POST ->
// real hook -> real Postgres row); and that the saved record's own EDIT
// form refuses the same rule rather than letting an ordinary edit store
// what the create path refused.
func TestStockTransfer_CreateRealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := stockTransferTestServer(t)
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

	item, err := engine.Create(ctx, def("Item"), map[string]any{
		"sku": "SKU-E2E-ST-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	facilityA, err := engine.Create(ctx, def("Facility"), map[string]any{
		"code": "WH-E2E-A", "name": "E2E Warehouse A", "facility_type": "warehouse",
	}, actor)
	if err != nil {
		t.Fatalf("create Facility A: %v", err)
	}
	facilityB, err := engine.Create(ctx, def("Facility"), map[string]any{
		"code": "WH-E2E-B", "name": "E2E Warehouse B", "facility_type": "warehouse",
	}, actor)
	if err != nil {
		t.Fatalf("create Facility B: %v", err)
	}
	draftStatusID := publishedStatusID(t, tenantDB, "stock_transfer_status", "draft")

	bctx := browserCtx(t, tenantID)

	// Same-facility submission: rejected, with a visible error — the
	// point of driving this in a real browser rather than calling the
	// hook directly (already covered by internal/kernel/purchasing's own
	// unit tests).
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/StockTransfer/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open new StockTransfer form: %v", err)
	}
	setReferenceHiddenValue(t, bctx, "item_id", item.ID)
	setReferenceHiddenValue(t, bctx, "from_facility_id", facilityA.ID)
	setReferenceHiddenValue(t, bctx, "to_facility_id", facilityA.ID)
	setReferenceHiddenValue(t, bctx, "status_id", draftStatusID)
	if err := chromedp.Run(bctx,
		chromedp.SetValue(`input[name="qty"]`, "10", chromedp.ByQuery),
		chromedp.SetValue(`input[name="transfer_date"]`, "2026-08-01", chromedp.ByQuery),
		// A plain click, not submitForm(): the settle helper rejects on a
		// non-2xx response, and a 400 is exactly what this scenario wants
		// — the toast's own visibility is the wait condition (same
		// pattern as TestPurchaseOrderStages_ReversedDates_ToastInRealBrowser).
		chromedp.Click(`form.uc-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-toast.uc-toast-visible`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("same-facility save + toast: %v", err)
	}
	var toastText string
	if err := chromedp.Run(bctx, chromedp.Text(`#uc-toast`, &toastText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read toast: %v", err)
	}
	if !strings.Contains(toastText, "must differ") {
		t.Fatalf("toast %q should carry the same-facility rejection reason", toastText)
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'StockTransfer'`).Scan(&count); err != nil {
		t.Fatalf("count StockTransfer records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the rejected browser submission to leave no StockTransfer row behind, found %d", count)
	}

	// Now a valid submission on a fresh form — distinct facilities —
	// must genuinely succeed end to end.
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/StockTransfer/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("re-open new StockTransfer form: %v", err)
	}
	setReferenceHiddenValue(t, bctx, "item_id", item.ID)
	setReferenceHiddenValue(t, bctx, "from_facility_id", facilityA.ID)
	setReferenceHiddenValue(t, bctx, "to_facility_id", facilityB.ID)
	setReferenceHiddenValue(t, bctx, "status_id", draftStatusID)
	if err := chromedp.Run(bctx,
		chromedp.SetValue(`input[name="qty"]`, "10", chromedp.ByQuery),
		chromedp.SetValue(`input[name="transfer_date"]`, "2026-08-01", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("valid create + save: %v", err)
	}
	recordID := savedRecordID(t, bctx, "StockTransfer")

	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'StockTransfer' AND id = $1`, recordID).Scan(&count); err != nil {
		t.Fatalf("count StockTransfer record %s: %v", recordID, err)
	}
	if count != 1 {
		t.Fatalf("expected the valid StockTransfer to actually persist to Postgres, found %d rows for id %s", count, recordID)
	}

	// The saved record's own EDIT form must refuse the same rule — the
	// hole an independent review measured on this card's first draft,
	// where the hook gated on audit.ActionCreate and a legitimately
	// created transfer could be edited into a same-facility one through
	// exactly this form. Still a plain click, not submitForm(): a 400 is
	// what this scenario wants.
	setReferenceHiddenValue(t, bctx, "to_facility_id", facilityA.ID)
	if err := chromedp.Run(bctx,
		chromedp.Click(`form.uc-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-toast.uc-toast-visible`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("same-facility edit + toast: %v", err)
	}
	if err := chromedp.Run(bctx, chromedp.Text(`#uc-toast`, &toastText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read edit toast: %v", err)
	}
	if !strings.Contains(toastText, "must differ") {
		t.Fatalf("edit toast %q should carry the same-facility rejection reason", toastText)
	}
	var storedToFacility string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT data->>'to_facility_id' FROM records WHERE entity_type = 'StockTransfer' AND id = $1`, recordID,
	).Scan(&storedToFacility); err != nil {
		t.Fatalf("read back StockTransfer %s: %v", recordID, err)
	}
	if storedToFacility != facilityB.ID {
		t.Fatalf("expected the rejected edit to roll back entirely, to_facility_id is now %q", storedToFacility)
	}

	// And it shows up on the real rendered list page.
	var listText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/StockTransfer"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render StockTransfer list: %v", err)
	}
	if !strings.Contains(listText, "E2E Warehouse A") || !strings.Contains(listText, "E2E Warehouse B") {
		t.Errorf("StockTransfer list should show both facilities by name, got:\n%s", listText)
	}
}
