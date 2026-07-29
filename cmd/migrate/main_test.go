// Smoke tests for the real compiled cmd/migrate binary — CLAUDE.md's
// "Smoke tests: a real compiled binary/server actually starts,
// responds..." layer. applyFuncFor above already exercises the
// -target -> db.Apply* mapping as a unit; what's untested until now is
// the actual process: flag parsing, DATABASE_URL handling, exit codes,
// and log output a real operator or CI step depends on.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "migrate")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func run(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return testexec.Run(t, binPath, env, args...)
}

func TestMigrate_MissingDatabaseURL_FailsFast(t *testing.T) {
	_, stderr, code := run(t, []string{})
	if code == 0 {
		t.Fatal("expected non-zero exit with DATABASE_URL unset")
	}
	if !strings.Contains(stderr, "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got stderr: %q", stderr)
	}
}

func TestMigrate_UnknownTarget_FailsFast(t *testing.T) {
	dsn := testexec.FreshDatabase(t, "uc_test_migrate_bad_target")
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-target=bogus")
	if code == 0 {
		t.Fatal("expected non-zero exit with an unknown -target")
	}
	if !strings.Contains(stderr, `unknown -target "bogus"`) {
		t.Fatalf("expected unknown-target error, got stderr: %q", stderr)
	}
}

func TestMigrate_Legacy_AppliesSchemaAndIsIdempotent(t *testing.T) {
	dsn := testexec.FreshDatabase(t, "uc_test_migrate_legacy")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn})
	if code != 0 {
		t.Fatalf("first run: exit %d, stderr: %s", code, stderr)
	}
	assertTableExists(t, dsn, "tenants")
	assertTableExists(t, dsn, "records")

	// Idempotent: a second run against the same already-migrated
	// database (simulating a process restart) must not fail.
	_, stderr, code = run(t, []string{"DATABASE_URL=" + dsn})
	if code != 0 {
		t.Fatalf("second run should be a no-op, got exit %d, stderr: %s", code, stderr)
	}
}

func TestMigrate_Control_AppliesOnlyControlPlaneSchema(t *testing.T) {
	dsn := testexec.FreshDatabase(t, "uc_test_migrate_control")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-target=control")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	assertTableExists(t, dsn, "tenants")
	assertTableMissing(t, dsn, "records")
}

func TestMigrate_Tenant_AppliesOnlyTenantSchema(t *testing.T) {
	dsn := testexec.FreshDatabase(t, "uc_test_migrate_tenant")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-target=tenant")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	assertTableExists(t, dsn, "records")
	assertTableMissing(t, dsn, "tenants")
}

func assertTableExists(t *testing.T, dsn, table string) {
	t.Helper()
	if !tableExists(t, dsn, table) {
		t.Fatalf("expected table %q to exist", table)
	}
}

func assertTableMissing(t *testing.T, dsn, table string) {
	t.Helper()
	if tableExists(t, dsn, table) {
		t.Fatalf("did not expect table %q to exist", table)
	}
}

func tableExists(t *testing.T, dsn, table string) bool {
	t.Helper()
	db := testexec.Open(t, dsn)
	var exists bool
	err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}
