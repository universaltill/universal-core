package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_finance_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

func TestPublish_PublishesEveryFinanceDefinition(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(tenantDB)

	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	all := All()
	if len(all) == 0 {
		t.Fatal("All() returned no Definitions — test would pass vacuously")
	}
	for _, def := range all {
		v, err := repo.GetPublished(ctx, def.EntityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", def.EntityType, err)
		}
		if v.Version != def.Version {
			t.Fatalf("%s: expected published version %d, got %d", def.EntityType, def.Version, v.Version)
		}
	}
}

func TestPublish_IsIdempotent(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("second Publish should be a no-op, got: %v", err)
	}
}

func TestPublishForms_PublishesEveryFinanceForm(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewFormDefinitionRepo(tenantDB)

	if err := PublishForms(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}

	forms := AllForms()
	if len(forms) == 0 {
		t.Fatal("AllForms() returned no Definitions — test would pass vacuously")
	}
	for _, f := range forms {
		v, err := repo.GetPublished(ctx, f.EntityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", f.EntityType, err)
		}
		if v.Version != f.Version {
			t.Fatalf("%s: expected published form version %d, got %d", f.EntityType, f.Version, v.Version)
		}
	}
}

func TestPublishForms_IsIdempotent(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if err := PublishForms(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("first PublishForms: %v", err)
	}
	if err := PublishForms(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("second PublishForms should be a no-op, got: %v", err)
	}
}

// TestPublish_ResumesFromPartiallyDraftedState mirrors purchasing's/
// sales' own regression coverage for moduleseed.PublishAll's
// resume-after-crash contract, applied to this package's own Definition
// set.
func TestPublish_ResumesFromPartiallyDraftedState(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(tenantDB)
	actor := humanActor()

	accountDef := Account()
	raw, err := json.Marshal(accountDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, accountDef.EntityType, accountDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	// Deliberately do NOT approve/publish — simulating a crash right here.

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	v, err := repo.GetPublished(ctx, "Account")
	if err != nil {
		t.Fatalf("expected Account to be published after resuming from a draft-only state, got: %v", err)
	}
	if v.Version != accountDef.Version {
		t.Fatalf("expected published version %d, got %d", accountDef.Version, v.Version)
	}
}

// TestPublish_LeavesRolledBackVersionAlone mirrors purchasing's/sales'
// own regression test of the same name for parity — the behavior itself
// (moduleseed.PublishAll never resurrects a deliberately rolled-back
// version) is already covered generically at the moduleseed layer since
// finance.Publish is a straight passthrough to PublishAll; this is
// package-level regression coverage for finance specifically, not proof
// of new behavior.
func TestPublish_LeavesRolledBackVersionAlone(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(tenantDB)
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := repo.Rollback(ctx, "Account", 1, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if _, err := repo.GetPublished(ctx, "Account"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected Account to stay rolled back (no published version), got: %v", err)
	}
}

// TestAccount_HierarchyResolvesEndToEnd is the most important integration
// test in this file: proves the self-reference chart-of-accounts shape
// actually works against real Postgres, not just that Account()'s Go
// struct declares a reference field. Creates a real parent, a real
// child pointing at it, and confirms the child's parent_account_id
// round-trips correctly through crud.Engine.
func TestAccount_HierarchyResolvesEndToEnd(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	v, err := entityDefs.GetPublished(ctx, "Account")
	if err != nil {
		t.Fatalf("GetPublished(Account): %v", err)
	}
	def, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Account definition: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	parent, err := engine.Create(ctx, def, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create parent Account: %v", err)
	}

	child, err := engine.Create(ctx, def, map[string]any{
		"code": "1100", "name": "Cash and Bank", "type": "asset",
		"parent_account_id": parent.ID, "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create child Account: %v", err)
	}

	got, err := engine.Get(ctx, def, child.ID)
	if err != nil {
		t.Fatalf("get child Account: %v", err)
	}
	if got.Data["parent_account_id"] != parent.ID {
		t.Fatalf("expected child's parent_account_id to be %q, got %v", parent.ID, got.Data["parent_account_id"])
	}
}

func TestSyncGLAccounts_CreatesMatchingGLAccountsRows(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	accountDefRaw, err := entityDefs.GetPublished(ctx, "Account")
	if err != nil {
		t.Fatalf("GetPublished(Account): %v", err)
	}
	accountDef, err := entity.Unmarshal(accountDefRaw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Account: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	if _, err := engine.Create(ctx, accountDef, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create Account: %v", err)
	}

	if err := SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("SyncGLAccounts: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, _, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("expected gl_accounts row for code 1000 after sync, got: %v", err)
	}
	var name, accountType, currency string
	var isActive bool
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT name, account_type, currency, is_active FROM gl_accounts WHERE id = $1`, id,
	).Scan(&name, &accountType, &currency, &isActive); err != nil {
		t.Fatalf("read gl_account: %v", err)
	}
	if name != "Assets" || accountType != "asset" || currency != defaultGLCurrency || !isActive {
		t.Fatalf("unexpected gl_account: name=%q type=%q currency=%q active=%v", name, accountType, currency, isActive)
	}
}

// TestSyncGLAccounts_ResolvesRealCurrencyCode confirms the sync doesn't
// just always fall back to defaultGLCurrency — when an Account really
// does set currency_id, the synced gl_accounts row gets that currency's
// actual code, not the fallback.
func TestSyncGLAccounts_ResolvesRealCurrencyCode(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	currencyDefRaw, err := entityDefs.GetPublished(ctx, "Currency")
	if err != nil {
		t.Fatalf("GetPublished(Currency): %v", err)
	}
	currencyDef, err := entity.Unmarshal(currencyDefRaw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Currency: %v", err)
	}
	accountDefRaw, err := entityDefs.GetPublished(ctx, "Account")
	if err != nil {
		t.Fatalf("GetPublished(Account): %v", err)
	}
	accountDef, err := entity.Unmarshal(accountDefRaw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Account: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	gbp, err := engine.Create(ctx, currencyDef, map[string]any{"code": "GBP", "name": "British Pound"}, actor)
	if err != nil {
		t.Fatalf("create Currency: %v", err)
	}
	if _, err := engine.Create(ctx, accountDef, map[string]any{
		"code": "1100", "name": "Cash", "type": "asset", "currency_id": gbp.ID, "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create Account: %v", err)
	}

	if err := SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("SyncGLAccounts: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, _, err := glAccounts.IDByCode(ctx, "1100")
	if err != nil {
		t.Fatalf("expected gl_accounts row for code 1100, got: %v", err)
	}
	var currency string
	if err := tenantDB.QueryRowContext(ctx, `SELECT currency FROM gl_accounts WHERE id = $1`, id).Scan(&currency); err != nil {
		t.Fatalf("read gl_account currency: %v", err)
	}
	if currency != "GBP" {
		t.Fatalf("expected the real Currency.code %q to be synced, got %q", "GBP", currency)
	}
}

func TestSyncGLAccounts_IsIdempotent(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	accountDefRaw, err := entityDefs.GetPublished(ctx, "Account")
	if err != nil {
		t.Fatalf("GetPublished(Account): %v", err)
	}
	accountDef, err := entity.Unmarshal(accountDefRaw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Account: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	if _, err := engine.Create(ctx, accountDef, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor); err != nil {
		t.Fatalf("create Account: %v", err)
	}

	if err := SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("first SyncGLAccounts: %v", err)
	}
	if err := SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("second SyncGLAccounts should be a no-op, got: %v", err)
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM gl_accounts WHERE code = '1000'`).Scan(&count); err != nil {
		t.Fatalf("count gl_accounts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 gl_accounts row after re-running SyncGLAccounts, got %d", count)
	}
}

// TestSyncGLAccounts_PropagatesAccountUpdate is the central promise
// ADR-0004 makes about this function — editing a finance.Account (here:
// renaming it and deactivating it) and re-syncing must update the
// existing gl_accounts row in place, not just create-once-and-forget.
// Caught as a real gap by independent review: TestSyncGLAccounts_
// IsIdempotent only proved re-running sync over *unchanged* data was a
// no-op, never that a real edit actually propagates.
func TestSyncGLAccounts_PropagatesAccountUpdate(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	accountDefRaw, err := entityDefs.GetPublished(ctx, "Account")
	if err != nil {
		t.Fatalf("GetPublished(Account): %v", err)
	}
	accountDef, err := entity.Unmarshal(accountDefRaw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Account: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	rec, err := engine.Create(ctx, accountDef, map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Account: %v", err)
	}
	if err := SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("first SyncGLAccounts: %v", err)
	}

	version := rec.Version
	if _, err := engine.Update(ctx, accountDef, rec.ID, map[string]any{
		"code": "1000", "name": "Total Assets", "type": "asset", "is_active": false,
	}, &version, actor); err != nil {
		t.Fatalf("update Account: %v", err)
	}
	if err := SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("second SyncGLAccounts: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, isActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	var name string
	if err := tenantDB.QueryRowContext(ctx, `SELECT name FROM gl_accounts WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read gl_account name: %v", err)
	}
	if name != "Total Assets" {
		t.Fatalf("expected the renamed Account to propagate to gl_accounts, got name=%q", name)
	}
	if isActive {
		t.Fatal("expected the deactivated Account to propagate to gl_accounts.is_active=false")
	}
}
