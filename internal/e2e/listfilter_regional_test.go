package e2e

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

func filterShipmentDef(entityType string) (*entity.Definition, *form.Definition) {
	return &entity.Definition{
			EntityType: entityType,
			Version:    1,
			Fields: []entity.Field{
				{Name: "name", Type: entity.FieldString, Required: true},
				{Name: "ship_date", Type: entity.FieldDate},
				{Name: "weight", Type: entity.FieldNumber},
			},
		}, &form.Definition{
			EntityType: entityType,
			Version:    1,
			Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "name", Label: "Name"},
					{Name: "ship_date", Label: "Ship Date"},
					{Name: "weight", Label: "Weight"},
				}}},
			Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
		}
}

// TestListFilter_RegionalNumber_RealBrowser drives board #74's fix
// through a real browser rather than an httptest request: it puts the
// browser's own JS engine (not Go's url.QueryEscape) in charge of
// encoding a Turkish-grouped number into the real search input's live
// value, reads it back out of the actual DOM, and navigates the real
// browser tab to the resulting filtered URL — checking the matching
// row comes back and that the filter box itself still shows exactly
// the regional text, not a normalized ASCII form. (A literal click on
// the page's own submit button was tried first and reproducibly raced
// chromedp's frame-lifecycle tracking in this sandbox — every
// navigation-triggering click intermittently left the next DOM query
// hung against a torn-down execution context, independent of any wait/
// retry strategy tried. Driving the navigation directly here still
// exercises the real input's DOM, the real cell rendering, and the
// real server-side parsing — the parts that are actually this card's
// risk — without that unrelated browser-automation flakiness.)
func TestListFilter_RegionalNumber_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	shipDef, shipForm := filterShipmentDef("FilterShipment")
	publishDef(t, tenantDB, shipDef, shipForm)
	if _, err := crud.NewEngine(tenantDB).Create(context.Background(), shipDef, map[string]any{
		"name": "Container 1", "ship_date": "2026-04-03", "weight": 1234567.5,
	}, humanActor()); err != nil {
		t.Fatalf("seed FilterShipment: %v", err)
	}

	ctx := browserCtx(t, tenantID)

	// filter=weight is explicit (not left to the page's own "first
	// filterable column" default, which is "name") — the hidden filter
	// field this test's own SetValue+navigate flow relies on comes from
	// THIS page load.
	var listText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/FilterShipment?lang=tr&region=tr-TR&filter=weight"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load Turkish list page: %v", err)
	}
	if !strings.Contains(listText, "1.234.567,5") {
		t.Fatalf("expected the Turkish-grouped number in the rendered table, got:\n%s", listText)
	}

	// Put the Turkish-grouped text into the real input, then ask the
	// BROWSER (not the test) to compute the encoded URL its own form
	// submission would produce (encodeURIComponent, the same algorithm
	// a real GET form submit uses for a query value), and navigate to
	// it directly — genuine browser-side encoding of the value, without
	// the click/navigation race.
	// The unfiltered page (no ?q= yet) renders no hidden `filter` input
	// at all (listview.go only sets FilterField once a filter is
	// actually active) — this page load's own filter=weight came from
	// the URL this test built, so that's what a real submit of this
	// exact page would also carry; read only `q` live from the DOM.
	var href string
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`input[name="q"]`, "1.234.567,5", chromedp.ByQuery),
		chromedp.Evaluate(`(function() {
			var f = document.querySelector('form.uc-list-filter');
			var q = document.querySelector('input[name="q"]').value;
			return f.getAttribute('action') + '?filter=weight&q=' + encodeURIComponent(q);
		})()`, &href),
	); err != nil {
		t.Fatalf("compute filter URL from the live form: %v", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+href),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the browser-encoded filter URL: %v", err)
	}
	if !strings.Contains(listText, "Container 1") {
		t.Errorf("the Turkish-displayed number, encoded by the browser itself, found no match, got:\n%s", listText)
	}

	// The filter box itself must echo back exactly what was typed, not
	// a normalized ASCII form — checked in the live DOM, not the raw
	// HTML string, so this also proves the browser actually rendered
	// the attribute the server sent rather than something a template
	// escaping bug silently altered.
	var boxValue string
	if err := chromedp.Run(ctx,
		chromedp.Value(`input[name="q"]`, &boxValue, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read filter box value: %v", err)
	}
	if boxValue != "1.234.567,5" {
		t.Errorf("filter box value = %q, want the raw typed text 1.234.567,5", boxValue)
	}
}

// TestRegionPicker_RealBrowser_ClearsFilterAcrossRegionSwitch is
// uc-infra#128 driven through a real browser: operate the actual
// <select class="uc-nav-region"> the way a viewer does (not an httptest
// request built by hand) while a FieldNumber filter is active, and
// confirm the resulting page neither silently drops a matching row (the
// raw text reinterpreted under the new region's rules) nor leaves the
// filter box showing stale text from the region that's no longer active
// — the live-DOM proof that listview_filter_regional_test.go's
// TestRegionPicker_DropsFilterValueAcrossRegionSwitch can only show at
// the rendered-HTML-string level.
//
// Two rows are seeded (not one): a single row can't distinguish "the
// filter was truly cleared" from "the filter is still active and still
// happens to match" (uc-infra#128's independent review, F5) — after the
// switch, BOTH must be visible.
//
// The region is read out of the LIVE option element rather than
// hardcoded (independent review, F4): chromedp.SetValue on a <select>
// silently assigns "" if no option actually has the value given, so
// asserting the option exists first is what proves the picker really
// offers a stale-q-free en-IN link, not just that this test's hardcoded
// guess happened to match. And the switch is confirmed with
// chromedp.Location — not a WaitVisible/Text pair on an element that's
// already visible in the pre-switch DOM — because that combination isn't
// a navigation barrier and can read the OLD page's content, making a
// "did it change" assertion pass vacuously (this repo's own established
// pattern for exactly this, see rfq_compare_quotes_link_test.go,
// module_menu_hub_test.go, tenant_picker_test.go).
func TestRegionPicker_RealBrowser_ClearsFilterAcrossRegionSwitch(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	shipDef, shipForm := filterShipmentDef("RegionSwitchShipment")
	publishDef(t, tenantDB, shipDef, shipForm)
	eng := crud.NewEngine(tenantDB)
	if _, err := eng.Create(context.Background(), shipDef, map[string]any{
		"name": "Container 1", "ship_date": "2026-04-03", "weight": 1234567.5,
	}, humanActor()); err != nil {
		t.Fatalf("seed RegionSwitchShipment: %v", err)
	}
	if _, err := eng.Create(context.Background(), shipDef, map[string]any{
		"name": "Container 2", "ship_date": "2026-01-15", "weight": 42.0,
	}, humanActor()); err != nil {
		t.Fatalf("seed second RegionSwitchShipment: %v", err)
	}

	ctx := browserCtx(t, tenantID)

	// British grouping ("en" is the language whose picker actually
	// offers more than one region — most others don't — and en-GB vs.
	// en-IN disagree on grouping, the same pair the httptest-level
	// sibling test uses).
	var listText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/RegionSwitchShipment?filter=weight&q="+url.QueryEscape("1,234,567.5")),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load British-grouped filter: %v", err)
	}
	if !strings.Contains(listText, "Container 1") || strings.Contains(listText, "Container 2") {
		t.Fatalf("expected the British-grouped filter to match only Container 1, got:\n%s", listText)
	}

	// Read the picker's own live en-IN option value rather than assuming
	// it — proves the offered link is really stale-q-free, and that this
	// test's SetValue below actually lands on a real option.
	var enINHref string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(function() {
			var opts = document.querySelectorAll('select.uc-nav-region option');
			for (var i = 0; i < opts.length; i++) {
				var u = new URL(opts[i].value, window.location.href);
				if (u.searchParams.get('region') === 'en-IN') return opts[i].value;
			}
			return '';
		})()`, &enINHref),
	); err != nil {
		t.Fatalf("read region picker options: %v", err)
	}
	if enINHref == "" {
		t.Fatal("no en-IN option found in the region picker")
	}
	if strings.Contains(enINHref, "q=") {
		t.Fatalf("region picker's own en-IN option carries \"q=\" — following it would reinterpret the filter under en-IN's rules: %s", enINHref)
	}

	// Operate the picker the way a user does: select that real option and
	// let the element's own change handler navigate. chromedp.Location
	// alongside the WaitVisible/Text pair (this repo's own established
	// idiom for "did the click really navigate", see
	// rfq_compare_quotes_link_test.go) is what proves the browser landed
	// on the new URL rather than reading the pre-switch page's DOM.
	var landedURL string
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`select.uc-nav-region`, enINHref, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('select.uc-nav-region').dispatchEvent(new Event('change'))`, nil),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Location(&landedURL),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("operate region picker: %v", err)
	}
	if !strings.Contains(landedURL, "region=en-IN") || strings.Contains(landedURL, "q=") {
		t.Fatalf("expected the switch to land on a region=en-IN URL with no q=, got: %s", landedURL)
	}
	if !strings.Contains(listText, "Container 1") || !strings.Contains(listText, "Container 2") {
		t.Errorf("switching region should show every row (filter=weight with no q, matching nothing left inactive) — got:\n%s", listText)
	}

	var boxValue string
	if err := chromedp.Run(ctx,
		chromedp.Value(`input[name="q"]`, &boxValue, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read filter box value: %v", err)
	}
	if boxValue != "" {
		t.Errorf("filter box = %q after a region switch, want empty — showing the old region's text implies it still applies", boxValue)
	}
}

// TestListFilter_RegionalDate_AmbiguousOrder_RealBrowser pairs a
// negative and a positive check on the SAME literal query text — a
// negative check alone (independent review) can't distinguish "the
// locale's day/month order was actually applied" from "the whole
// mechanism does nothing and nothing ever matches". "04/03/2026" is a
// well-formed date shape but meaningless under en-GB's ACTIVE
// day-first field order — it means 4 March, not the stored 3 April,
// so it must NOT match — while the identical text IS the seeded date
// under en-US's month-first order, and must match.
func TestListFilter_RegionalDate_AmbiguousOrder_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	shipDef, shipForm := filterShipmentDef("AmbigDateShipment")
	publishDef(t, tenantDB, shipDef, shipForm)
	if _, err := crud.NewEngine(tenantDB).Create(context.Background(), shipDef, map[string]any{
		"name": "Container 1", "ship_date": "2026-04-03", "weight": 1234567.5,
	}, humanActor()); err != nil {
		t.Fatalf("seed AmbigDateShipment: %v", err)
	}

	ctx := browserCtx(t, tenantID)

	// No row matches under en-GB, so the page renders the empty-state
	// paragraph instead of a <table> at all — wait for THAT, not for a
	// table that will never appear.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/AmbigDateShipment?filter=ship_date&q=04%2F03%2F2026"),
		chromedp.WaitVisible(`p.uc-empty`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load ambiguous-date filter: %v", err)
	}
	if strings.Contains(bodyText, "Container 1") {
		t.Errorf("en-GB day-first filter must not match a stored 2026-04-03 record under an ambiguous date string, got:\n%s", bodyText)
	}

	// Same literal query text, en-US region: month-first, so it means
	// the stored 3 April and MUST match. If this fails too, the
	// negative check above proved nothing.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/records/AmbigDateShipment?region=en-US&filter=ship_date&q=04%2F03%2F2026"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load en-US filter: %v", err)
	}
	if !strings.Contains(bodyText, "Container 1") {
		t.Errorf("en-US month-first filter should match the stored 2026-04-03 record, got:\n%s", bodyText)
	}
}
