package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestStatusForm_NameIsMultilingual_RealBrowser is the production-form
// proof for uc-infra#214 (ADR-0030): internal/e2e/i18n_text_test.go's
// TestI18nText_RealBrowser already proves the FieldI18nText mechanism
// itself works end-to-end (a synthetic entity/form pair) — this test
// proves the REAL, shipped foundation.StatusForm() picks that mechanism
// up correctly with no incompatible field override, since that form is
// declared independently of this change (a plain {Name: "name", Label:
// "Name"} FormField, unchanged by uc-infra#214's own diff) and nothing
// generically proves a specific production form renders a specific
// field type correctly other than actually loading it in a browser.
//
// Also the one genuinely new user-visible surface this phase has: an
// admin creating a Status record BY HAND (StatusForm's own doc comment:
// "you will not usually create these... only when defining a brand-new
// lifecycle from scratch") now sees one name input per locale instead
// of one plain text box — narrow, but real, hence a real browser test
// rather than treating this as covered by the generic mechanism test
// alone.
func TestStatusForm_NameIsMultilingual_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}

	// Seed the StatusType this Status will belong to directly — the
	// picker widget itself (search, debounce, DOM option rendering) is
	// already covered by TestI18nText_RealBrowser and
	// reference_picker_test.go; this test's subject is the "name" field,
	// not the "status_type_id" field, so a direct seed keeps it focused.
	engine := crud.NewEngine(tenantDB)
	statusTypeRec, err := engine.Create(ctx, foundation.StatusType(), map[string]any{
		"entity_type": "WidgetLifecycle", "code": "widget_lifecycle_status", "name": "Widget Lifecycle Status",
	}, actor)
	if err != nil {
		t.Fatalf("seed StatusType: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/Status/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open Status/new: %v", err)
	}
	pickReference(t, bctx, "status_type_id", "Widget", "Widget Lifecycle Status")
	if err := chromedp.Run(bctx,
		chromedp.SetValue(`input[name="code"]`, "draft", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name.en"]`, "Draft", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name.tr"]`, "Taslak", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save Status: %v", err)
	}
	statusID := savedRecordID(t, bctx, "Status")

	// Reload fresh: both locales persisted to Postgres as an i18n_text
	// object, not just reflected client-side, and the picker's current
	// value still resolves.
	var enVal, trVal, refLabel string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/Status/"+statusID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`input[name="name.en"]`, &enVal, chromedp.ByQuery),
		chromedp.Value(`input[name="name.tr"]`, &trVal, chromedp.ByQuery),
		chromedp.Value(`.uc-ref[data-field="status_type_id"] .uc-ref-search`, &refLabel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read reloaded Status: %v", err)
	}
	if enVal != "Draft" || trVal != "Taslak" {
		t.Fatalf("expected {en:Draft, tr:Taslak} persisted, got {en:%q, tr:%q}", enVal, trVal)
	}
	if refLabel != "Widget Lifecycle Status" {
		t.Fatalf("expected the reloaded status_type_id picker to show %q, got %q", "Widget Lifecycle Status", refLabel)
	}

	rec, err := engine.Get(ctx, foundation.Status(), statusID)
	if err != nil {
		t.Fatalf("Get saved Status: %v", err)
	}
	name, ok := rec.Data["name"].(map[string]any)
	if !ok || name["en"] != "Draft" || name["tr"] != "Taslak" {
		t.Fatalf("expected the stored record's name to be {en:Draft, tr:Taslak}, got %#v", rec.Data["name"])
	}
	if rec.Data["status_type_id"] != statusTypeRec.ID {
		t.Fatalf("expected status_type_id %q, got %v", statusTypeRec.ID, rec.Data["status_type_id"])
	}
}
