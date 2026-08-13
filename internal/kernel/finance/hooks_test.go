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

// TestSyncGLAccountOnWrite_RenamingCodeOrphansThePreviousGLAccountsRow
// pins down, deliberately, a real limitation independent review found
// (finding 2) that this change does not close: gl_accounts is keyed by
// Account.code (UpsertByCode), and nothing links a gl_accounts row back
// to the Account record it was projected from. Renaming an existing
// Account's code — legal; code is Unique but not immutable, see
// Account()'s own doc comment — makes this hook upsert a NEW gl_accounts
// row under the new code, while the OLD code's row is left behind,
// active, orphaned. This test exists so that behavior can't silently
// change (for better or worse) without this test having to change too —
// not as an endorsement of the behavior. Closing it properly needs
// gl_accounts to gain a source-record link (a migration) or Account.code
// to become immutable after create; tracked as a separate backlog card.
func TestSyncGLAccountOnWrite_RenamingCodeOrphansThePreviousGLAccountsRow(t *testing.T) {
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
		"code": "1100", "name": "Assets", "type": "asset", "is_active": true,
	}, &version, actor); err != nil {
		t.Fatalf("update Account's code: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	if _, _, err := glAccounts.IDByCode(ctx, "1100"); err != nil {
		t.Fatalf("expected a gl_accounts row for the NEW code 1100, got: %v", err)
	}
	// Documents today's real gap: the OLD code's row is still there,
	// unchanged, orphaned — not cleaned up, not deactivated.
	oldID, oldActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("expected the OLD code 1000's gl_accounts row to still exist (orphaned, not this fix's job to clean up), got: %v", err)
	}
	if !oldActive {
		t.Fatal("expected the orphaned old-code row to still read as active — this hook never touches it once the code changes")
	}
	if oldID == "" {
		t.Fatal("expected a real id for the orphaned row")
	}
}

// TestSyncGLAccountOnWrite_IsActiveOmittedOnCreate_StoresInactive pins
// down another real gap independent review found (finding 5): a
// programmatic engine.Create call that omits a field entirely never
// consults that field's declared entity.Field.Default — nothing in
// internal/kernel/crud or internal/kernel/entity applies Default at
// write time, for any field type, not just FieldBool.
//
// uc-infra#206 closed the half of this gap that actually reached a real
// admin: internal/kernel/formrender's new-record rendering now honors a
// FieldBool's Default the same way it already did for FieldEnum, so the
// ordinary UI create flow this test's own history worried about (an
// admin who never touches the "Active" checkbox) now submits
// is_active=true explicitly — the checkbox itself renders pre-checked,
// so the browser's own hidden-false/checkbox-true submission trick
// carries the real value through, and engine.Create receives it SET,
// not omitted, once the request comes from that fixed form.
//
// This test's own path bypasses formrender entirely — engine.Create is
// called directly with is_active left out of the payload, which is
// exactly what a non-browser caller (CSV import, a future API create,
// this test itself) still does. That deeper gap — Default is a
// Definition-level declaration formrender now reads but crud/entity
// still never does — is unchanged by #206 and is genuinely a separate,
// broader change (every field type, every non-form write path), so it
// stays open rather than folded into #206's narrower rendering fix.
func TestSyncGLAccountOnWrite_IsActiveOmittedOnCreate_StoresInactive(t *testing.T) {
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
	// Definition-level declaration, not something Create fills in.
	if _, err := engine.Create(ctx, accountDefinition, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset",
	}, actor); err != nil {
		t.Fatalf("create Account without is_active: %v", err)
	}

	_, isActive, err := data.NewGLAccountRepo(tenantDB).IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	if isActive {
		t.Fatal("expected today's real (pre-existing, cross-module) gap to store is_active=false when omitted via a direct engine.Create call — if this now passes with isActive=true, crud/entity started applying Field.Default at write time and this test (and uc-infra#212) should be revisited; formrender's own half of this gap is already closed by uc-infra#206")
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
