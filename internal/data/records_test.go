package data

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
)

// freshTenantDB mirrors internal/kernel/crud's own fixture of the same
// name — RecordRepo's non-transactional Update/Delete wrappers
// (internal/kernel/crud.Engine always goes through UpdateTx/DeleteTx
// inside an explicit transaction, per CLAUDE.md's audit-atomicity rule,
// so these two thin wrappers had no coverage anywhere in the repo before
// this file) need the same real schema every other RecordRepo method is
// exercised against transitively through crud.Engine's own tests.
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

	name := fmt.Sprintf("uc_test_data_%d", time.Now().UnixNano())
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

// TestRecordRepo_UpdateUsesOwnConnectionPool confirms the non-Tx Update
// wrapper genuinely writes (via r.db, not a caller-supplied transaction)
// and delegates correctly to UpdateTx — same effective behavior, called
// without an explicit transaction.
func TestRecordRepo_UpdateUsesOwnConnectionPool(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Widget", map[string]any{"name": "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newVersion, err := repo.Update(ctx, "Widget", rec.ID, map[string]any{"name": "updated"}, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if newVersion != rec.Version+1 {
		t.Fatalf("expected version %d, got %d", rec.Version+1, newVersion)
	}

	got, err := repo.Get(ctx, "Widget", rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data["name"] != "updated" {
		t.Fatalf("expected updated data to be persisted, got %+v", got.Data)
	}
}

// TestRecordRepo_UpdateVersionConflict confirms the non-Tx wrapper
// surfaces ErrVersionConflict exactly like UpdateTx does when
// expectedVersion is stale — the optimistic-locking behavior isn't lost
// by going through the thinner entry point.
func TestRecordRepo_UpdateVersionConflict(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Widget", map[string]any{"name": "v1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := rec.Version - 1 // a version that will never match
	if _, err := repo.Update(ctx, "Widget", rec.ID, map[string]any{"name": "v2"}, &stale); err != ErrVersionConflict {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
}

// TestRecordRepo_UpdateNotFound confirms updating a nonexistent id
// through the non-Tx wrapper returns ErrNotFound, not ErrVersionConflict
// — UpdateTx's own doc comment explains why a single UPDATE can't tell
// these apart on its own and needs the follow-up existence check.
func TestRecordRepo_UpdateNotFound(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	if _, err := repo.Update(ctx, "Widget", "00000000-0000-0000-0000-000000000000", map[string]any{"name": "x"}, nil); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestRecordRepo_DeleteUsesOwnConnectionPool confirms the non-Tx Delete
// wrapper soft-deletes a real record (Get no longer finds it afterward)
// via its own connection pool.
func TestRecordRepo_DeleteUsesOwnConnectionPool(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Widget", map[string]any{"name": "to-delete"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(ctx, "Widget", rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, "Widget", rec.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestRecordRepo_DeleteNotFound(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	if err := repo.Delete(ctx, "Widget", "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRecordRepo_ListTx_ParticipatesInCallerTransaction(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	if _, err := repo.Create(ctx, "Widget", map[string]any{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tx, err := tdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	got, err := repo.ListTx(ctx, tx, "Widget")
	if err != nil {
		t.Fatalf("ListTx: %v", err)
	}
	if len(got) != 1 || got[0].Data["name"] != "a" {
		t.Fatalf("expected 1 Widget record visible within the tx, got %+v", got)
	}
}

// TestRecordRepo_ListTx_SeesUncommittedWritesInSameTx confirms ListTx
// reads within the *same* transaction, not a separate connection —
// what internal/kernel/ledger's period check actually needs (seeing a
// Period this same transaction may have just touched, not only
// already-committed state a fresh connection would see).
func TestRecordRepo_ListTx_SeesUncommittedWritesInSameTx(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	tx, err := tdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := repo.CreateTx(ctx, tx, "Widget", map[string]any{"name": "uncommitted"}); err != nil {
		t.Fatalf("CreateTx: %v", err)
	}

	got, err := repo.ListTx(ctx, tx, "Widget")
	if err != nil {
		t.Fatalf("ListTx: %v", err)
	}
	if len(got) != 1 || got[0].Data["name"] != "uncommitted" {
		t.Fatalf("expected ListTx to see the uncommitted write within the same tx, got %+v", got)
	}

	// A separate connection must NOT see it (not committed yet) —
	// confirms this really is transaction-scoped visibility, not a
	// coincidence of List also happening to return the same row.
	outside, err := repo.List(ctx, "Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(outside) != 0 {
		t.Fatalf("expected the uncommitted write to be invisible outside the tx, got %+v", outside)
	}
}
