package finance

import (
	"context"
	"database/sql"
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
