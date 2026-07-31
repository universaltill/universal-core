package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// TestRegionalLocale_RealBrowser drives the regional-formatting
// preference (#22) through a real browser, which is the only way to
// prove the parts a rendered-string test cannot: that the picker is an
// actually-operable <select> whose change handler navigates, that the
// resulting preference survives a fresh navigation with no query
// string (the cookie is really set by the browser, not just present in
// a header), and that a date input still holds the raw ISO value in the
// live DOM while the list cell beside it shows a Jalali date.
func TestRegionalLocale_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	shipDef := &entity.Definition{
		EntityType: "RegionalShipment",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "ship_date", Type: entity.FieldDate},
			{Name: "weight", Type: entity.FieldNumber},
		},
	}
	shipForm := &form.Definition{
		EntityType: "RegionalShipment",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{
				{Name: "name", Label: "Name"},
				{Name: "ship_date", Label: "Ship Date"},
				{Name: "weight", Label: "Weight"},
			}}},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	publishDef(t, tenantDB, shipDef, shipForm)

	// The record is seeded through the engine, not the create form:
	// this test is about how a stored date and number are DISPLAYED,
	// and the create form's own submit machinery is exercised by
	// form_save_test.go already.
	rec, err := crud.NewEngine(tenantDB).Create(context.Background(), shipDef, map[string]any{
		"name": "Container 1", "ship_date": "2026-04-03", "weight": 1234567.5,
	}, humanActor())
	if err != nil {
		t.Fatalf("seed RegionalShipment: %v", err)
	}
	recordID := rec.ID

	ctx := browserCtx(t, tenantID)

	// Default (en-GB): day-first.
	var listText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/RegionalShipment"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load list page: %v", err)
	}
	if !strings.Contains(listText, "03/04/2026") {
		t.Errorf("expected the en-GB date 03/04/2026 in the rendered table, got:\n%s", listText)
	}
	if !strings.Contains(listText, "1,234,567.5") {
		t.Errorf("expected a grouped number in the rendered table, got:\n%s", listText)
	}

	// Operate the picker the way a user does: select en-US and let the
	// element's own change handler navigate. This is the part a string
	// test can't prove — that the inline onchange actually works.
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`select.uc-nav-region`, "/records/RegionalShipment?region=en-US", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('select.uc-nav-region').dispatchEvent(new Event('change'))`, nil),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("operate region picker: %v", err)
	}
	if !strings.Contains(listText, "04/03/2026") {
		t.Errorf("picking en-US did not reformat the date, got:\n%s", listText)
	}

	// The preference persists across a navigation carrying no query
	// string at all — i.e. the browser really stored and resent the
	// cookie.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/RegionalShipment"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("reload list page: %v", err)
	}
	if !strings.Contains(listText, "04/03/2026") {
		t.Errorf("the en-US preference did not survive a clean reload, got:\n%s", listText)
	}

	// Farsi + Jalali: the list cell shows a converted date, while the
	// form input for the same record still holds the raw ISO value —
	// the display-only scope boundary, asserted in a live DOM.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/RegionalShipment?lang=fa&region=fa-IR"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load Farsi list page: %v", err)
	}
	if !strings.Contains(listText, "۱۴۰۵/۰۱/۱۴") {
		t.Errorf("expected the Jalali date ۱۴۰۵/۰۱/۱۴ in the rendered table, got:\n%s", listText)
	}

	var inputValue string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/RegionalShipment/"+recordID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`input[name="ship_date"]`, &inputValue, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load form for input check: %v", err)
	}
	if inputValue != "2026-04-03" {
		t.Errorf("date input = %q, want the raw ISO 2026-04-03 (a localized value would not round-trip on submit)", inputValue)
	}

	// The page's text direction still follows the LANGUAGE, not the
	// region — the two preferences must stay independent.
	var dir string
	if err := chromedp.Run(ctx,
		chromedp.AttributeValue(`html`, "dir", &dir, nil, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read dir attribute: %v", err)
	}
	if dir != "rtl" {
		t.Errorf("html dir = %q, want rtl for Farsi", dir)
	}
}

// A single-region language must not render a dead one-option picker.
func TestRegionalLocale_SingleRegionHidesPicker(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	var count int
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=tr"),
		chromedp.WaitVisible(`nav`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('select.uc-nav-region').length`, &count),
	); err != nil {
		t.Fatalf("load Turkish dashboard: %v", err)
	}
	if count != 0 {
		t.Errorf("Turkish has one region; expected no picker, found %d", count)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(`nav`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('select.uc-nav-region option').length`, &count),
	); err != nil {
		t.Fatalf("load English dashboard: %v", err)
	}
	if count < 2 {
		t.Errorf("English should offer several regions, found %d options", count)
	}
}
