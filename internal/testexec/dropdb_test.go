package testexec

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The regression guard for a leak that was silent by construction:
// nothing failed when a tenant database was left behind, so it went
// unnoticed until ~4,100 orphaned uc_tenant_* databases had accumulated
// on a dev machine. A test that merely called the helper and passed
// would prove nothing — these assert the database is genuinely GONE from
// pg_database afterwards.

// adminConn opens a connection to the TEST_DATABASE_URL server itself.
func adminConn(t *testing.T) *sql.DB {
	t.Helper()
	admin, err := sql.Open("pgx", TestDatabaseURL(t))
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	return admin
}

func databaseExists(t *testing.T, admin *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count pg_database %s: %v", name, err)
	}
	return n > 0
}

// createScratchDatabase makes a database WITHOUT registering any
// cleanup of its own — the helper under test is the only thing that can
// remove it, which is what makes the assertion meaningful.
func createScratchDatabase(t *testing.T, admin *sql.DB, prefix string) (name, dsn string) {
	t.Helper()
	name = fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return name, u.String()
}

// TestDropConnectedDatabase_ActuallyDropsIt runs the helper inside a
// subtest so its t.Cleanup fires while the parent can still observe the
// result — the only way to assert on something a cleanup did.
func TestDropConnectedDatabase_ActuallyDropsIt(t *testing.T) {
	admin := adminConn(t)
	var name string

	t.Run("inner", func(t *testing.T) {
		var dsn string
		name, dsn = createScratchDatabase(t, admin, "uc_test_dropconn")
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open scratch database: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if err := db.Ping(); err != nil {
			t.Fatalf("ping scratch database: %v", err)
		}
		DropConnectedDatabase(t, db)

		if !databaseExists(t, admin, name) {
			t.Fatalf("%s should still exist while the inner test is running", name)
		}
	})

	if databaseExists(t, admin, name) {
		t.Fatalf("%s survived DropConnectedDatabase — the leak is not closed", name)
	}
}

// The drop must succeed even with a live connection still open — which
// is the actual situation at cleanup time, since the Router's own pool
// is closed by a cleanup registered EARLIER and therefore running LATER
// (t.Cleanup is LIFO). Without the pg_terminate_backend, DROP DATABASE
// fails with "database is being accessed by other users" and the leak
// silently persists.
func TestDropConnectedDatabase_DropsDespiteOpenConnection(t *testing.T) {
	admin := adminConn(t)
	var name string

	t.Run("inner", func(t *testing.T) {
		var dsn string
		name, dsn = createScratchDatabase(t, admin, "uc_test_dropbusy")
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open scratch database: %v", err)
		}
		if err := db.Ping(); err != nil {
			t.Fatalf("ping scratch database: %v", err)
		}
		DropConnectedDatabase(t, db)
		// Deliberately NOT closed: an open pool is the real condition
		// this has to survive.
		var one int
		if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("hold a live connection open: %v", err)
		}
	})

	if databaseExists(t, admin, name) {
		t.Fatalf("%s survived the drop while a connection was open — pg_terminate_backend is not doing its job", name)
	}
}

func TestDropDatabaseByName_IsIdempotent(t *testing.T) {
	admin := adminConn(t)
	var name string

	t.Run("inner", func(t *testing.T) {
		name, _ = createScratchDatabase(t, admin, "uc_test_dropname")
		// Registered twice: the second drop finds nothing and must not
		// fail the test (DROP DATABASE IF EXISTS).
		DropDatabaseByName(t, name)
		DropDatabaseByName(t, name)
	})

	if databaseExists(t, admin, name) {
		t.Fatalf("%s survived DropDatabaseByName", name)
	}
}
