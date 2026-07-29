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
