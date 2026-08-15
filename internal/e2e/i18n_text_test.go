package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// publishDef drives one entity + form Definition through the real
// CreateDraft -> Approve -> Publish registry lifecycle, the same path the
// api package's own test helper uses — needed here because no
// foundation/purchasing entity has an i18n_text field yet (this feature
// ships the type).
func publishDef(t *testing.T, db *sql.DB, entDef *entity.Definition, formDef *form.Definition) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	entRaw, err := json.Marshal(entDef)
	if err != nil {
		t.Fatalf("marshal entity def: %v", err)
	}
	entRepo := data.NewEntityDefinitionRepo(db)
	if _, err := entRepo.CreateDraft(ctx, entDef.EntityType, entDef.Version, entRaw, actor); err != nil {
		t.Fatalf("CreateDraft entity: %v", err)
	}
	if err := entRepo.Approve(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Approve entity: %v", err)
	}
	if err := entRepo.Publish(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Publish entity: %v", err)
	}

	formRaw, err := json.Marshal(formDef)
	if err != nil {
		t.Fatalf("marshal form def: %v", err)
	}
	formRepo := data.NewFormDefinitionRepo(db)
	if _, err := formRepo.CreateDraft(ctx, formDef.EntityType, formDef.Version, formRaw, actor); err != nil {
		t.Fatalf("CreateDraft form: %v", err)
	}
	if err := formRepo.Approve(ctx, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Approve form: %v", err)
	}
	if err := formRepo.Publish(ctx, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Publish form: %v", err)
	}
}

// TestI18nText_RealBrowser drives the i18n_text field (#23, ADR-0009)
// through a real browser: it fills the per-locale inputs of a multilingual
// `name`, saves, confirms each locale persisted across a fresh page load,
// then confirms a reference picker on another entity shows that record's
// label in the VIEWER's locale (Turkish here) — the whole point of the
// feature, proven in a real DOM, not just a rendered string.
func TestI18nText_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	unitDef := &entity.Definition{
		EntityType: "MultiUnit",
		Version:    1,
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldI18nText, Required: true}},
	}
	unitForm := &form.Definition{
		EntityType: "MultiUnit",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}}}},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	widgetDef := &entity.Definition{
		EntityType: "Widget2",
		Version:    1,
		Fields: []entity.Field{
			{Name: "sku", Type: entity.FieldString, Required: true},
			{Name: "unit_id", Type: entity.FieldReference, Target: "MultiUnit"},
		},
	}
	widgetForm := &form.Definition{
		EntityType: "Widget2",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "sku", Label: "SKU"}, {Name: "unit_id", Label: "Unit"}}}},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	publishDef(t, tenantDB, unitDef, unitForm)
	publishDef(t, tenantDB, widgetDef, widgetForm)

	ctx := browserCtx(t, tenantID)

	// Fill the per-locale name inputs and save.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/MultiUnit/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="name.en"]`, "Each", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name.tr"]`, "Adet", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save MultiUnit: %v", err)
	}
	unitID := savedRecordID(t, ctx, "MultiUnit")

	// Reload fresh: both locales persisted to Postgres, not just reflected
	// client-side.
	var enVal, trVal string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/MultiUnit/"+unitID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`input[name="name.en"]`, &enVal, chromedp.ByQuery),
		chromedp.Value(`input[name="name.tr"]`, &trVal, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read persisted i18n values: %v", err)
	}
	if enVal != "Each" || trVal != "Adet" {
		t.Fatalf("expected {en:Each, tr:Adet} persisted, got {en:%q, tr:%q}", enVal, trVal)
	}

	// Create a Widget2 and pick the unit through the combobox. The Turkish
	// viewer's search must surface the Turkish label "Adet".
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Widget2/new?lang=tr"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="sku"]`, "W-1", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("fill Widget2: %v", err)
	}
	// The picker option must read "Adet" (tr), not "Each" (en) or a raw id.
	// Any keystroke triggers the (locale-cookie-scoped) fetch; the i18n
	// label field degrades to an unfiltered list, so a single char surfaces
	// every unit — here just the one, labelled in Turkish.
	pickReference(t, ctx, "unit_id", "A", "Adet")
	if got := referenceHiddenValue(t, ctx, "unit_id"); got != unitID {
		t.Fatalf("expected the picked unit id %q, got %q", unitID, got)
	}
	if err := chromedp.Run(ctx, submitForm()); err != nil {
		t.Fatalf("save Widget2: %v", err)
	}
	widgetID := savedRecordID(t, ctx, "Widget2")

	// Reload the Widget2 edit form in Turkish: the picker's current-value
	// label must show "Adet".
	var currentLabel string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Widget2/"+widgetID+"?lang=tr"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`.uc-ref[data-field="unit_id"] .uc-ref-search`, &currentLabel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read reloaded unit label: %v", err)
	}
	if currentLabel != "Adet" {
		t.Fatalf("expected the reloaded picker to show the Turkish label 'Adet', got %q", currentLabel)
	}
}

// TestI18nText_RealBrowser_SearchNarrowsAndSortsByViewerLocale is the
// real-browser proof for the i18n-aware reference-picker search/sort fix
// (uc-infra#249 item 1): before this fix, searchReferenceOptions forced
// canSortFilter = false for any entity.FieldI18nText label field, so a
// picker targeting one always degraded to an unsorted, unfiltered capped
// listing — every record, in creation order, regardless of what was typed.
// TestI18nText_RealBrowser above only ever seeds ONE MultiUnit record, so
// it cannot distinguish "real per-locale filter/sort" from "degraded
// unfiltered listing that happens to contain the one record" — both look
// identical with a single row. This test seeds three, so a real DOM
// assertion can tell the two apart:
//
//  1. Typing a Turkish substring that matches only SOME records' Turkish
//     translation must narrow the rendered option list to just those
//     matches, not render all three — proves the EXISTS/jsonb_each_text
//     filter is real, not the old degrade-to-everything behaviour.
//  2. The matching options must render in Turkish alphabetical order, NOT
//     creation order — the two are deliberately made to disagree (the
//     alphabetically-first match is created LAST) so a passing assertion
//     can only mean the new SortI18nLocales/COALESCE sort actually ran,
//     not a coincidence of insertion order.
func TestI18nText_RealBrowser_SearchNarrowsAndSortsByViewerLocale(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	unitDef := &entity.Definition{
		EntityType: "MultiUnit",
		Version:    1,
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldI18nText, Required: true}},
	}
	unitForm := &form.Definition{
		EntityType: "MultiUnit",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}}}},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	widgetDef := &entity.Definition{
		EntityType: "Widget2",
		Version:    1,
		Fields: []entity.Field{
			{Name: "sku", Type: entity.FieldString, Required: true},
			{Name: "unit_id", Type: entity.FieldReference, Target: "MultiUnit"},
		},
	}
	widgetForm := &form.Definition{
		EntityType: "Widget2",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "sku", Label: "SKU"}, {Name: "unit_id", Label: "Unit"}}}},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	publishDef(t, tenantDB, unitDef, unitForm)
	publishDef(t, tenantDB, widgetDef, widgetForm)

	ctx := browserCtx(t, tenantID)

	// Deliberate creation order: "Adres" (address) first, "Kutu" (box, a
	// non-match) second, "Adet" (unit) LAST. "Adet" sorts alphabetically
	// BEFORE "Adres" in Turkish ('e' < 'r') despite being created after it
	// — so a filtered+sorted result of [Adet, Adres] can only come from a
	// real locale-aware sort, not from creation order surviving unchanged.
	for _, u := range []struct{ en, tr string }{
		{"Address unit", "Adres"},
		{"Box", "Kutu"},
		{"Each", "Adet"},
	} {
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/forms/MultiUnit/new"),
			chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
			chromedp.SetValue(`input[name="name.en"]`, u.en, chromedp.ByQuery),
			chromedp.SetValue(`input[name="name.tr"]`, u.tr, chromedp.ByQuery),
			submitForm(),
		); err != nil {
			t.Fatalf("seed MultiUnit %q/%q: %v", u.en, u.tr, err)
		}
		savedRecordID(t, ctx, "MultiUnit")
	}

	// On a Turkish-locale Widget2 form, type "Ad" into the unit picker —
	// matches "Adet" and "Adres" but not "Kutu".
	scope := `.uc-ref[data-field="unit_id"]`
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Widget2/new?lang=tr"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SendKeys(scope+` .uc-ref-search`, "Ad", chromedp.ByQuery),
		chromedp.WaitVisible(scope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead 'Ad': %v", err)
	}

	var labels []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`Array.prototype.map.call(document.querySelectorAll('`+scope+` .uc-ref-option'),function(e){return e.textContent;})`,
		&labels,
	)); err != nil {
		t.Fatalf("read narrowed+sorted options: %v", err)
	}
	want := []string{"Adet", "Adres"}
	if len(labels) != len(want) || labels[0] != want[0] || labels[1] != want[1] {
		t.Fatalf("expected the Turkish type-ahead to narrow to %v in Turkish-alphabetical order (real per-locale filter+sort, not the old degrade-to-everything-in-creation-order behaviour), got %v", want, labels)
	}
}
