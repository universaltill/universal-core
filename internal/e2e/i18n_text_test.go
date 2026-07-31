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
