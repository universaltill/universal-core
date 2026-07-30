// Package testexec provides shared helpers for smoke-testing this
// repo's cmd/ binaries as real compiled subprocesses — CLAUDE.md's
// "Smoke tests: a real compiled binary/server actually starts,
// responds..." requirement, which a package-internal Go test calling
// main()'s helper functions directly would not actually prove (main()
// itself, the flag parsing, and the os.Exit/log.Fatal exit-code
// behavior are exactly what's untested otherwise).
package testexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Build compiles the cmd package at pkgDir (a directory containing
// main.go, e.g. "." when run from within that package's own directory)
// to a temp binary named binName and returns its path plus a cleanup
// func that removes the temp directory. Intended for use from a
// package's TestMain (built once per test binary run, not once per test
// case — go build is slow enough that doing it per-test would make a
// package with a dozen smoke-test cases painfully slow).
func Build(pkgDir, binName string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "uc-smoke-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	path = filepath.Join(dir, binName)
	cmd := exec.Command("go", "build", "-o", path, pkgDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build %s: %w\n%s", pkgDir, err, out)
	}
	return path, func() { os.RemoveAll(dir) }, nil
}

// TestDatabaseURL returns TEST_DATABASE_URL, skipping the test if unset
// — the same convention every other integration test in this repo
// already follows (see internal/db/migrate_test.go's testDB).
func TestDatabaseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

// FreshDatabase creates a brand-new, uniquely-named database on the same
// Postgres server as TEST_DATABASE_URL (pgcrypto enabled, nothing else
// applied — the caller runs whichever migration set it needs, exactly
// as a real cmd/migrate invocation would against a freshly-created
// database) and returns its own connection DSN. Registers cleanup
// (terminate + drop) via t.Cleanup. Mirrors internal/db/migrate_test.go's
// freshTestDB and internal/e2e/csv_import_test.go's freshControlDB —
// this is the third copy of that exact pattern, now factored out since
// cmd/'s smoke tests need it for both control and tenant databases
// across five separate packages.
func FreshDatabase(t *testing.T, namePrefix string) (dsn string) {
	t.Helper()
	base := TestDatabaseURL(t)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("%s_%d", namePrefix, time.Now().UnixNano())
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
	dsn = u.String()

	fresh, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open fresh database %s: %v", name, err)
	}
	defer fresh.Close()
	if err := fresh.Ping(); err != nil {
		t.Fatalf("ping fresh database %s: %v", name, err)
	}
	if _, err := fresh.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	return dsn
}

// Open opens conn to dsn and registers t.Cleanup to close it, sparing
// every call site the same three lines.
func Open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// DropTenantDatabase registers a t.Cleanup that drops tenantID's own
// physical database (looked up via control's tenants.db_name). Every
// tenant internal/tenantdb.Router.Create provisions is a second,
// separately-named Postgres database (ADR-0003) that FreshDatabase's own
// cleanup has no knowledge of — dropping the control-plane database a
// tenant's row lived in does not drop the tenant's own database, so any
// test that provisions a tenant (directly via Router.Create, or
// indirectly by running cmd/provision-tenant/cmd/seed-demo-data as a
// subprocess) must call this too, or the tenant database leaks
// permanently. Mirrors internal/tenantdb/router_test.go's own
// dropTenantDatabase, factored out here so cmd/'s smoke tests share it
// instead of leaking (found via code review, 2026-07-29).
func DropTenantDatabase(t *testing.T, control *sql.DB, tenantID string) {
	t.Helper()
	t.Cleanup(func() {
		var dbName string
		err := control.QueryRowContext(context.Background(),
			`SELECT db_name FROM tenants WHERE id = $1`, tenantID,
		).Scan(&dbName)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			t.Logf("DropTenantDatabase: look up db_name for %s: %v", tenantID, err)
			return
		}
		_, _ = control.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
		_, _ = control.Exec(`DROP DATABASE IF EXISTS "` + strings.ReplaceAll(dbName, `"`, `""`) + `"`)
	})
}

// DropConnectedDatabase registers a t.Cleanup that drops whatever
// physical database db is connected to, discovering its name from the
// connection itself (SELECT current_database()) rather than from a
// control-plane lookup.
//
// This is DropTenantDatabase's sibling for the common case where a test
// holds a tenant's own *sql.DB but NOT the control database — which is
// every test-side tenant helper in internal/api, internal/worker and
// internal/e2e. Threading a control handle through those would have
// meant changing the signature of a helper with over a hundred call
// sites, to obtain a mapping the tenant's own connection can already
// answer for itself.
//
// Why this exists at all: internal/tenantdb.Router.Create provisions a
// SECOND, separately-named Postgres database per tenant (ADR-0003).
// Dropping the control database a tenant's row lived in does not drop
// that database, so a test that provisions a tenant and cleans up only
// its control database leaks the tenant's database permanently — and
// silently, since nothing fails. Left unnoticed, this reached ~4,100
// orphaned uc_tenant_* databases on one dev machine, growing every time
// the suite ran (found 2026-07-30; the same class of bug an earlier
// review already caught once in cmd/'s smoke tests, which is what
// DropTenantDatabase was factored out for).
//
// Connections are terminated before the drop because the cleanup that
// closes the Router's own pools is registered EARLIER than this one and
// therefore runs LATER (t.Cleanup is LIFO) — so the tenant's pool is
// still open at this point, and DROP DATABASE would otherwise fail with
// "database is being accessed by other users."
func DropConnectedDatabase(t *testing.T, db *sql.DB) {
	t.Helper()
	var dbName string
	if err := db.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&dbName); err != nil {
		// Read eagerly, not inside the cleanup: by cleanup time the
		// connection may already be closed, and a failure to even
		// identify the database is worth surfacing while the test is
		// still running rather than silently leaking.
		t.Fatalf("DropConnectedDatabase: resolve current_database(): %v", err)
	}
	DropDatabaseByName(t, dbName)
}

// DropDatabaseByName registers a t.Cleanup that terminates every
// connection to dbName and drops it, over its own admin connection to
// TEST_DATABASE_URL (the database being dropped cannot be the one the
// dropping connection is attached to).
func DropDatabaseByName(t *testing.T, dbName string) {
	t.Helper()
	base := TestDatabaseURL(t)
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", base)
		if err != nil {
			t.Logf("DropDatabaseByName(%s): open admin connection: %v", dbName, err)
			return
		}
		defer admin.Close()
		quoted := `"` + strings.ReplaceAll(dbName, `"`, `""`) + `"`

		// Terminate-then-drop, retried: a test whose background goroutines
		// outlive it (internal/worker deliberately has one that does not
		// join its pollers) can hold a *sql.DB whose pool reopens a
		// connection in the window between the terminate and the drop,
		// making DROP DATABASE fail with "database is being accessed by
		// other users". Independent review raised this and could not
		// reproduce it in 130 runs — but the failure mode is the leak this
		// helper exists to prevent, reappearing silently, so it is closed
		// by construction rather than left to timing.
		for attempt := 0; attempt < 3; attempt++ {
			_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
			if _, err = admin.Exec(`DROP DATABASE IF EXISTS ` + quoted); err == nil {
				return
			}
		}
		// Errorf, not Logf: a database that survived every attempt IS a
		// leak, and this helper's whole purpose is that leaks stop being
		// invisible. Not Fatalf — that would skip the remaining cleanups
		// and strand more state than it reports.
		t.Errorf("DropDatabaseByName(%s): database survived %d drop attempts and has LEAKED: %v", dbName, 3, err)
	})
}

// Run executes the binary at binPath with the given environment and
// arguments, capturing stdout/stderr separately and normalizing a
// nonzero exit into exitCode rather than an error — the shape every
// cmd/ smoke test needs to assert against a binary's real exit
// behavior. Any failure to start the process at all (not the same as
// the process itself exiting nonzero) still fails the test via t.Fatalf.
func Run(t *testing.T, binPath string, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return outBuf.String(), errBuf.String(), 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return outBuf.String(), errBuf.String(), exitErr.ExitCode()
	}
	t.Fatalf("run %s: %v", binPath, err)
	return "", "", -1
}
