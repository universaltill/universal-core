package crud

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestEngine_Create_FiresRegisteredHook(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	var gotAction audit.Action
	var gotEntityType string
	var gotName any
	fired := false
	engine.SetHook("Vendor", func(_ context.Context, tx *sql.Tx, d *entity.Definition, rec data.Record, action audit.Action, _ audit.Actor) error {
		fired = true
		gotAction = action
		gotEntityType = d.EntityType
		gotName = rec.Data["name"]
		if tx == nil {
			t.Fatal("expected a non-nil tx — hook must run inside the write's own transaction")
		}
		return nil
	})

	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme Textiles"}, actor)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !fired {
		t.Fatal("expected the registered hook to fire on Create")
	}
	if gotAction != audit.ActionCreate {
		t.Fatalf("expected action %q, got %q", audit.ActionCreate, gotAction)
	}
	if gotEntityType != "Vendor" {
		t.Fatalf("expected entity type Vendor, got %q", gotEntityType)
	}
	if gotName != "Acme Textiles" {
		t.Fatalf("expected hook to see the just-created record's data, got %v", gotName)
	}
	if rec.ID == "" {
		t.Fatal("expected Create to still return a valid record")
	}
}

func TestEngine_Update_FiresRegisteredHook(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme Textiles"}, actor)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var gotAction audit.Action
	var gotID string
	fired := false
	engine.SetHook("Vendor", func(_ context.Context, _ *sql.Tx, _ *entity.Definition, updated data.Record, action audit.Action, _ audit.Actor) error {
		fired = true
		gotAction = action
		gotID = updated.ID
		return nil
	})

	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{"name": "Acme Textiles Ltd"}, nil, actor); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !fired {
		t.Fatal("expected the registered hook to fire on Update")
	}
	if gotAction != audit.ActionUpdate {
		t.Fatalf("expected action %q, got %q", audit.ActionUpdate, gotAction)
	}
	if gotID != rec.ID {
		t.Fatalf("expected hook to see the updated record's id %q, got %q", rec.ID, gotID)
	}
}

// TestEngine_Create_HookErrorRollsBackWholeWrite is the whole point of
// running the hook inside the write's own transaction (Hook's own doc
// comment): a failing hook (e.g. a ledger posting that doesn't balance)
// must roll back the record write too, not leave an un-posted business
// document behind.
func TestEngine_Create_HookErrorRollsBackWholeWrite(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	hookErr := errors.New("simulated posting failure")
	engine.SetHook("Vendor", func(context.Context, *sql.Tx, *entity.Definition, data.Record, audit.Action, audit.Actor) error {
		return hookErr
	})

	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	_, err := engine.Create(ctx, def, map[string]any{"name": "Should Not Exist"}, actor)
	if !errors.Is(err, hookErr) {
		t.Fatalf("expected the hook's own error to be returned (wrapped), got %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'Vendor'`).Scan(&count); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the failed hook to roll back the record write too, found %d Vendor record(s)", count)
	}
	var auditCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE entity_type = 'Vendor'`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("expected the failed hook to roll back the audit entry too, found %d", auditCount)
	}
}

// TestEngine_Update_ReturnsFreshVersionAfterHookWritesAgain confirms
// Update's returned version reflects whatever a registered hook actually
// left the row on, not the version captured before the hook ran — a real
// bug an independent review of purchasing.MatchVendorInvoiceOnUpdate
// found: that hook does its own records.UpdateTx on the very record
// Update is already writing (a redirect to a different status), which
// bumps the row's version a second time inside the same transaction.
// Returning the pre-hook version would violate Update's own documented
// contract ("the version it should check against next time") and fail
// the caller's very next optimistic-locked Update with a false
// ErrVersionConflict — reproduced directly here without needing the
// purchasing package's own fixtures.
func TestEngine_Update_ReturnsFreshVersionAfterHookWritesAgain(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	records := data.NewRecordRepo(nil)
	engine.SetHook("Vendor", func(ctx context.Context, tx *sql.Tx, _ *entity.Definition, updated data.Record, action audit.Action, _ audit.Actor) error {
		if action != audit.ActionUpdate {
			return nil
		}
		// A second write to the same record, inside the same tx — same
		// shape as MatchVendorInvoiceOnUpdate's own redirect.
		_, err := records.UpdateTx(ctx, tx, "Vendor", updated.ID, map[string]any{"name": "Redirected By Hook"}, nil)
		return err
	})

	version := rec.Version
	gotVersion, err := engine.Update(ctx, def, rec.ID, map[string]any{"name": "First Edit"}, &version, actor)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	stored, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotVersion != stored.Version {
		t.Fatalf("Update returned version %d but the stored record is actually at version %d (hook's own write wasn't reflected)", gotVersion, stored.Version)
	}
	if stored.Data["name"] != "Redirected By Hook" {
		t.Fatalf("expected the hook's own write to be the one that stuck, got %v", stored.Data["name"])
	}

	// The real-world consequence: the caller's next optimistic-locked
	// Update, using the version Update just returned, must succeed —
	// not fail with a false ErrVersionConflict against a version the row
	// has already moved past.
	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{"name": "Second Edit"}, &gotVersion, actor); err != nil {
		t.Fatalf("expected the next Update to succeed using the version Update returned, got: %v", err)
	}
}

// TestEngine_Create_NoHookRegistered_IsANoOp confirms every existing
// entity type (no hook ever registered for it) behaves exactly as
// before this mechanism existed — the near-certain majority case.
func TestEngine_Create_NoHookRegistered_IsANoOp(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme Textiles"}, actor)
	if err != nil {
		t.Fatalf("Create without any registered hook should succeed unchanged, got: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("expected a valid record")
	}
}

// TestEngine_Delete_DoesNotFireHook pins down, with a real test rather
// than only a doc comment, a premise several hooks in this codebase rely
// on (most recently finance.SyncGLAccountOnWrite, uc-infra#204 —
// independent review flagged that this was asserted in a comment but
// never actually verified anywhere): Engine.Delete has no runHook call
// at all (see Delete's own body), so a hook registered for an entity
// type is never invoked on a soft-delete of that entity. If Delete ever
// grows a runHook call in the future, every hook relying on "only
// Create/Update, never Delete" needs to be re-audited — this test is
// what would catch that silently changing.
func TestEngine_Delete_DoesNotFireHook(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme Textiles"}, actor)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fired := false
	engine.SetHook("Vendor", func(context.Context, *sql.Tx, *entity.Definition, data.Record, audit.Action, audit.Actor) error {
		fired = true
		return nil
	})

	if err := engine.Delete(ctx, def, rec.ID, actor); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fired {
		t.Fatal("expected the registered hook NOT to fire on Delete — Engine.Delete has no runHook call")
	}
}
