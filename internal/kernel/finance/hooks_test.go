package finance

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestSyncGLAccountOnWrite_CreateProjectsIntoGLAccounts is the core
// acceptance case for uc-infra#204: creating a finance.Account through
// the generic crud.Engine — the exact path a real tenant admin's save
// takes, not a direct SyncGLAccounts call — must be enough on its own
// for gl_accounts to pick it up, with no separate sync step.
func TestSyncGLAccountOnWrite_CreateProjectsIntoGLAccounts(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create Account: %v", err)
	}

	// Deliberately never called SyncGLAccounts above — the whole point
	// of the hook is that an ordinary Create is enough by itself.
	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, isActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("expected gl_accounts row for code 1000 immediately after Create, got: %v", err)
	}
	if !isActive {
		t.Fatal("expected the synced gl_account to be active")
	}
	var name, accountType string
	if err := tenantDB.QueryRowContext(ctx, `SELECT name, account_type FROM gl_accounts WHERE id = $1`, id).Scan(&name, &accountType); err != nil {
		t.Fatalf("read gl_account: %v", err)
	}
	if name != "Assets" || accountType != "asset" {
		t.Fatalf("expected name=Assets type=asset, got name=%q type=%q", name, accountType)
	}
}

// TestSyncGLAccountOnWrite_UpdatePropagates is the direct regression test
// for uc-infra#204's reported bug: before this hook existed, editing an
// already-created Account (renaming it, deactivating it) through the
// generic crud.Engine had no effect on gl_accounts at all — nothing
// outside cmd/seed-demo-data ever called SyncGLAccounts to pick the edit
// up. This test never calls SyncGLAccounts either; only Create/Update
// through the hooked engine, same as a real tenant admin's UI save.
func TestSyncGLAccountOnWrite_UpdatePropagates(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	rec, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Account: %v", err)
	}

	version := rec.Version
	if _, err := engine.Update(ctx, accountDefinition, rec.ID, map[string]any{
		"code": "1000", "name": "Total Assets", "type": "asset", "is_active": false,
	}, &version, actor); err != nil {
		t.Fatalf("update Account: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, isActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	if isActive {
		t.Fatal("expected the Update's is_active=false to have propagated to gl_accounts")
	}
	var name string
	if err := tenantDB.QueryRowContext(ctx, `SELECT name FROM gl_accounts WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read gl_account name: %v", err)
	}
	if name != "Total Assets" {
		t.Fatalf("expected the Update's rename to have propagated to gl_accounts, got name=%q", name)
	}
}

// TestSyncGLAccountOnWrite_ResolvesRealCurrencyCode mirrors
// TestSyncGLAccounts_ResolvesRealCurrencyCode (seed_test.go) for the hook
// path: an Account with a real currency_id resolves to that currency's
// own code, not the tenant base currency or DefaultGLCurrency.
func TestSyncGLAccountOnWrite_ResolvesRealCurrencyCode(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	currencyDefinition := publishedCurrencyDef(t, tenantDB)
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	eur, err := engine.Create(ctx, currencyDefinition, map[string]any{
		"code": "EUR", "name": "Euro", "is_base": false,
	}, actor)
	if err != nil {
		t.Fatalf("create EUR Currency: %v", err)
	}
	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true, "currency_id": eur.ID,
	}, actor); err != nil {
		t.Fatalf("create Account: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, _, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	var currency string
	if err := tenantDB.QueryRowContext(ctx, `SELECT currency FROM gl_accounts WHERE id = $1`, id).Scan(&currency); err != nil {
		t.Fatalf("read gl_account currency: %v", err)
	}
	if currency != "EUR" {
		t.Fatalf("expected the Account's own currency_id to resolve to %q, got %q", "EUR", currency)
	}
}

// TestSyncGLAccountOnWrite_NoCurrencyIDFallsBackToBaseCurrency mirrors
// TestSyncGLAccounts_FallsBackToTenantBaseCurrency (seed_test.go) for the
// hook path.
func TestSyncGLAccountOnWrite_NoCurrencyIDFallsBackToBaseCurrency(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	currencyDefinition := publishedCurrencyDef(t, tenantDB)
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	if _, err := engine.Create(ctx, currencyDefinition, map[string]any{
		"code": "QAR", "name": "Qatari Riyal", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create base Currency: %v", err)
	}
	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create Account: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, _, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	var currency string
	if err := tenantDB.QueryRowContext(ctx, `SELECT currency FROM gl_accounts WHERE id = $1`, id).Scan(&currency); err != nil {
		t.Fatalf("read gl_account currency: %v", err)
	}
	if currency != "QAR" {
		t.Fatalf("expected the tenant's base currency %q to be used as the fallback, got %q", "QAR", currency)
	}
}

// TestSyncGLAccountOnWrite_DuplicateCode_RejectedNotSilentlyOverwritten
// is the regression test for independent review's finding 3: before the
// Account Definition declared Unique on code (uc-infra#204 v2), a second
// Account created with a code already in use didn't just create a
// second, separately-identified Account — it silently clobbered the
// FIRST account's gl_accounts row wholesale the moment this hook made
// real writes reach that projection at all, with no error and no trace.
// Now the Create itself is rejected before the hook ever runs.
func TestSyncGLAccountOnWrite_DuplicateCode_RejectedNotSilentlyOverwritten(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create first Account 1000: %v", err)
	}

	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Something Else", "type": "expense", "is_active": false,
	}, actor); !errors.Is(err, crud.ErrUniqueConstraintViolation) {
		t.Fatalf("expected ErrUniqueConstraintViolation creating a second Account with code 1000, got: %v", err)
	}

	// The first account's own gl_accounts projection must be untouched —
	// the exact clobbering independent review reproduced before Unique
	// existed on this Definition.
	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, isActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	if !isActive {
		t.Fatal("expected the first Account's gl_accounts row to still be active — the rejected duplicate must never have reached the hook")
	}
	var name string
	if err := tenantDB.QueryRowContext(ctx, `SELECT name FROM gl_accounts WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read gl_account name: %v", err)
	}
	if name != "Assets" {
		t.Fatalf("expected the first Account's own name to survive the rejected duplicate, got %q", name)
	}
}

// TestSyncGLAccountOnWrite_RenamingCodeUpdatesTheSameRowInPlace asserts
// the uc-infra#205 fix: gl_accounts is now keyed by source_record_id
// (GLAccountRepo.UpsertBySourceRecord), a durable link back to the
// Account record it was projected from — not by code alone
// (UpsertByCode, still used by every other caller that has no Account
// record to link to). Renaming an existing Account's code — legal; code
// is Unique but not immutable, see Account()'s own doc comment — must
// now update the SAME gl_accounts row in place: same id, new code, old
// code no longer resolves to anything. Before this fix (see git
// history for the prior version of this test, then named
// ...OrphansThePreviousGLAccountsRow) a rename inserted a second row
// under the new code and left the old code's row behind, active,
// unreachable.
func TestSyncGLAccountOnWrite_RenamingCodeUpdatesTheSameRowInPlace(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	rec, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Account: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	originalID, _, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("expected a gl_accounts row for code 1000 right after create, got: %v", err)
	}

	version := rec.Version
	if _, err := engine.Update(ctx, accountDefinition, rec.ID, map[string]any{
		"code": "1100", "name": "Assets", "type": "asset", "is_active": true,
	}, &version, actor); err != nil {
		t.Fatalf("update Account's code: %v", err)
	}

	newID, isActive, err := glAccounts.IDByCode(ctx, "1100")
	if err != nil {
		t.Fatalf("expected a gl_accounts row for the NEW code 1100, got: %v", err)
	}
	if newID != originalID {
		t.Fatalf("expected the rename to update the SAME gl_accounts row in place (id %q), got a different row (id %q)", originalID, newID)
	}
	if !isActive {
		t.Fatal("expected the renamed row to still read as active")
	}

	// The fix's whole point: the OLD code must no longer resolve to
	// anything — no orphaned row left reachable under it.
	if _, _, err := glAccounts.IDByCode(ctx, "1000"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected the OLD code 1000 to no longer resolve after the rename, got: %v", err)
	}

	// Exactly one gl_accounts row total for this Account — not two.
	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM gl_accounts WHERE source_record_id = $1`, rec.ID).Scan(&count); err != nil {
		t.Fatalf("count gl_accounts rows for source_record_id: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 gl_accounts row linked to this Account record, got %d", count)
	}
}

// TestSyncGLAccountOnWrite_IsActiveOmittedOnCreate_StoresActive asserts
// the fixed behavior uc-infra#212 shipped: crud.Engine.Create now
// applies entity.Field.Default for any field genuinely absent from the
// payload, for every field type, before validation/persistence
// (internal/kernel/entity.ApplyDefaults). Previously named
// ...StoresInactive and pinned the opposite (buggy) behavior — renamed
// and flipped once this closed, per this test's own prior doc comment.
//
// uc-infra#206 already closed the half of this gap that reached a real
// admin through the UI: internal/kernel/formrender's new-record
// rendering honors a FieldBool's Default (the checkbox renders
// pre-checked), so a browser create flow was never actually affected by
// the deeper gap this test exercises. This test's own path bypasses
// formrender entirely — engine.Create is called directly with
// is_active left out of the payload, exactly what a non-browser caller
// (CSV import, a direct API/engine.Create call) does — and is the case
// #212 closed.
func TestSyncGLAccountOnWrite_IsActiveOmittedOnCreate_StoresActive(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	// is_active deliberately omitted — Account()'s Default:true is a
	// Definition-level declaration; engine.Create must fill it in now.
	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset",
	}, actor); err != nil {
		t.Fatalf("create Account without is_active: %v", err)
	}

	_, isActive, err := data.NewGLAccountRepo(tenantDB).IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	if !isActive {
		t.Fatal("expected engine.Create to apply Account's Default:true for the omitted is_active field (uc-infra#212) — got is_active=false")
	}
}

// TestSyncGLAccountOnWrite_CurrencyIDNotFound_RollsBackAccountUpdate
// covers the GetTx error branch independent review flagged as untested:
// an Account referencing a currency_id that doesn't resolve (e.g. a
// stale/bad reference) must fail the write loudly, not panic or silently
// fall back to a wrong currency.
func TestSyncGLAccountOnWrite_CurrencyIDNotFound_RollsBackAccountUpdate(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
		"currency_id": "00000000-0000-0000-0000-000000000000",
	}, actor); err == nil {
		t.Fatal("expected Create to fail when currency_id doesn't resolve to a real Currency record, got nil")
	}

	if _, _, err := data.NewGLAccountRepo(tenantDB).IDByCode(ctx, "1000"); err == nil {
		t.Fatal("expected no gl_accounts row after the rolled-back create")
	}
	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'Account'`).Scan(&count); err != nil {
		t.Fatalf("count Account records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the failed hook to roll back the Account write too, found %d Account record(s)", count)
	}
}

// TestSyncGLAccountOnWrite_CurrencyNotPublished_RollsBackAccountCreate is
// the error-path/edge-case test: if a caller somehow wires this hook in
// before the foundation module (and therefore Currency) has been
// published, the hook must fail loudly and — per Hook's own transactional
// contract — take the Account write down with it, rather than leaving a
// half-written Account with no matching gl_accounts row.
func TestSyncGLAccountOnWrite_CurrencyNotPublished_RollsBackAccountCreate(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	// finance.Publish only, deliberately skipping foundation.Publish —
	// Account exists but Currency never does.
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	accountDefinition := publishedAccountDef(t, tenantDB)

	engine := crud.NewEngine(tenantDB)
	engine.SetHook("Account", SyncGLAccountOnWrite)

	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor); err == nil {
		t.Fatal("expected Create to fail when the hook can't resolve a base currency (Currency never published), got nil")
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'Account'`).Scan(&count); err != nil {
		t.Fatalf("count Account records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the failed hook to roll back the Account write too, found %d Account record(s)", count)
	}
	if _, _, err := data.NewGLAccountRepo(tenantDB).IDByCode(ctx, "1000"); err == nil {
		t.Fatal("expected no gl_accounts row to have been created either")
	}
}

// publishedAccountDef is publishedCurrencyDef's counterpart for Account.
func publishedAccountDef(t *testing.T, tenantDB *sql.DB) *entity.Definition {
	t.Helper()
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	raw, err := entityDefs.GetPublished(context.Background(), "Account")
	if err != nil {
		t.Fatalf("GetPublished(Account): %v", err)
	}
	def, err := entity.Unmarshal(raw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Account: %v", err)
	}
	return def
}
