// Real-browser E2E for system-of-record read-only enforcement
// (uc-infra#102): a record imported from a read_only external source
// cannot be hand-edited, and the refusal a user actually sees is the
// TRANSLATED message in the global error toast (writeCrudErrorLocalized's
// 409 envelope — internal/api/handlers.go), never authz's logs-only
// English Error() string. internal/api/authz_sor_test.go proves the
// envelope; this proves the toast really renders it on a real form save,
// and that a hand-created record of the same type stays editable through
// the very same form.
package e2e

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// seedSoRItemFixture is internal/api's seedSoRFixture re-derived for this
// package's tenant (test helpers don't cross packages) and for Item —
// the entity type whose form testServer actually publishes. Everything is
// written through the RAW engine, the system path the import engine
// itself uses — never the guarded engine whose refusal this test probes:
// an ExternalSQLSource named sourceName, one imported Item, its
// ExternalIdentity row, and the SystemOfRecord read_only row declaring
// that source the owner of Item. Also seeds one hand-created Item with
// NO identity row — the positive control that must stay editable.
func seedSoRItemFixture(t *testing.T, tenantDB *sql.DB, sourceName string) (importedID, handmadeID string) {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(tenantDB)
	actor := humanActor()

	src, err := engine.Create(ctx, foundation.ExternalSQLSource(), map[string]any{
		"name": sourceName, "driver": "postgres", "host": "legacy.internal", "database": "navdb",
	}, actor)
	if err != nil {
		t.Fatalf("seed ExternalSQLSource: %v", err)
	}
	imported, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "NAV-1000", "name": "Imported Item", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed imported Item: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.ExternalIdentity(), map[string]any{
		"source_id":       src.ID,
		"source_relation": "public.item",
		"entity_type":     "Item",
		"record_id":       imported.ID,
		"external_key":    "1000",
	}, actor); err != nil {
		t.Fatalf("seed ExternalIdentity: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.SystemOfRecord(), map[string]any{
		"entity_type": "Item", "source_id": src.ID, "mode": "read_only",
	}, actor); err != nil {
		t.Fatalf("seed SystemOfRecord: %v", err)
	}
	handmade, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "OWN-1", "name": "Home-Grown Item", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed hand-made Item: %v", err)
	}
	return imported.ID, handmade.ID
}

// itemName reads one Item's stored name straight from the tenant
// database — what the browser reported has to match what is actually
// persisted (or, for the blocked save, actually NOT persisted).
func itemName(t *testing.T, tenantDB *sql.DB, id string) string {
	t.Helper()
	var name string
	if err := tenantDB.QueryRow(
		`SELECT data->>'name' FROM records WHERE id = $1 AND deleted_at IS NULL`, id,
	).Scan(&name); err != nil {
		t.Fatalf("read Item %s: %v", id, err)
	}
	return name
}

// TestSoRReadOnly_RealBrowser drives both halves of uc-infra#102's
// user-visible contract through one tenant's real edit forms:
//
//  1. Editing the IMPORTED Item and clicking Save surfaces the translated
//     block message — naming the owning source — in the real error toast
//     (the 409 envelope rendered by layout.go's htmx:responseError
//     listener), the form survives the failure, and direct SQL confirms
//     nothing changed.
//  2. The hand-created Item of the very same entity type, saved through
//     the very same form, goes through fine and persists — the read_only
//     declaration protects imported records, not the whole type.
func TestSoRReadOnly_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	importedID, handmadeID := seedSoRItemFixture(t, tenantDB, "NAV Mirror")
	ctx := browserCtx(t, tenantID)

	// Blocked half: a plain Click, not submitForm() — clickAndSettle's
	// settle helper rejects on a non-2xx response, and the 409 is exactly
	// what this test wants; the toast's own visibility is the wait
	// condition (TestPurchaseOrderStages_ReversedDates' convention).
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Item/"+importedID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Hand Edited Item", chromedp.ByQuery),
		chromedp.Click(`form.uc-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-toast.uc-toast-visible`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("save imported record + wait for the block toast: %v", err)
	}

	var toastText string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-toast`, &toastText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read toast text: %v", err)
	}
	// The en catalog's sor.read_only_blocked with {source} substituted —
	// the message a user can act on, naming who owns the record.
	if !strings.Contains(toastText, "This record is owned by NAV Mirror") {
		t.Fatalf("expected the translated block message naming the source, got toast: %q", toastText)
	}
	// authz.SoRReadOnlyError's Error() text is logs-only by contract
	// (sor.go's doc comment) — its distinctive phrasing must never reach
	// the toast.
	if strings.Contains(toastText, "read-only external source") {
		t.Fatalf("the engine's logs-only Error() text leaked into the toast: %q", toastText)
	}

	// The form is still there with the user's typed value — a blocked
	// save must not wipe the form away (error_toast_test.go's rule).
	var typedName string
	if err := chromedp.Run(ctx, chromedp.Value(`input[name="name"]`, &typedName, chromedp.ByQuery)); err != nil {
		t.Fatalf("read form value after blocked save: %v", err)
	}
	if typedName != "Hand Edited Item" {
		t.Fatalf("expected the form to survive the blocked save with the typed value, got %q", typedName)
	}
	// And underneath, nothing changed.
	if got := itemName(t, tenantDB, importedID); got != "Imported Item" {
		t.Fatalf("expected the blocked save to leave the record untouched, got name %q", got)
	}

	// Positive control: the un-identified Item of the same type saves
	// fine through the same form — submitForm() (clickAndSettle) demands
	// the 2xx settle the successful path produces.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Item/"+handmadeID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Home-Grown Item Ltd", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("save hand-made record: %v", err)
	}
	var savedName string
	if err := chromedp.Run(ctx, chromedp.Value(`input[name="name"]`, &savedName, chromedp.ByQuery)); err != nil {
		t.Fatalf("read form value after successful save: %v", err)
	}
	if savedName != "Home-Grown Item Ltd" {
		t.Fatalf("expected the edit form to reflect the saved value, got %q", savedName)
	}
	if got := itemName(t, tenantDB, handmadeID); got != "Home-Grown Item Ltd" {
		t.Fatalf("expected the hand-made record's edit to persist, got name %q", got)
	}
}
