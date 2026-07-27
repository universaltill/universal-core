package tenantdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
)

// controlTestDB returns an open connection to a fresh, uniquely-named
// database with ApplyControl already run against it, plus the DSN
// needed to construct a Router. Skips if TEST_DATABASE_URL isn't set,
// matching every other DB-backed test in this repo.
func controlTestDB(t *testing.T) (*sql.DB, string) {
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
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin connection: %v", err)
	}

	name := fmt.Sprintf("uc_test_router_control_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + quoteIdentifier(name)); err != nil {
		t.Fatalf("create control database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdentifier(name))
	})

	dsn := dsnWithDatabase(t, base, name)
	control, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open control database %s: %v", name, err)
	}
	t.Cleanup(func() { control.Close() })
	if err := control.Ping(); err != nil {
		t.Fatalf("ping control database %s: %v", name, err)
	}
	if _, err := control.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}

	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	return control, dsn
}

func dsnWithDatabase(t *testing.T, base, dbName string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base dsn: %v", err)
	}
	u.Path = "/" + dbName
	return u.String()
}

// dropTenantDatabase looks up tenantID's db_name via the control
// connection and drops that database — test cleanup, mirroring what a
// real tenant-offboarding tool would eventually do, just not built as
// production code yet.
func dropTenantDatabase(t *testing.T, control *sql.DB, tenantID string) {
	t.Helper()
	var dbName string
	err := control.QueryRowContext(context.Background(),
		`SELECT db_name FROM tenants WHERE id = $1`, tenantID,
	).Scan(&dbName)
	if errors.Is(err, sql.ErrNoRows) {
		return // Create's own cleanup already removed the row.
	}
	if err != nil {
		t.Logf("dropTenantDatabase: look up db_name for %s: %v", tenantID, err)
		return
	}
	_, _ = control.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
	_, _ = control.Exec(`DROP DATABASE IF EXISTS ` + quoteIdentifier(dbName))
}

// TestRouter_CreateProvisionsAWorkingTenantDatabase confirms Create does
// everything ADR-0003 §3 describes: a real database, migrated, reachable
// via Get afterward, and genuinely usable (a query against a
// tenant-owned table succeeds).
func TestRouter_CreateProvisionsAWorkingTenantDatabase(t *testing.T) {
	control, dsn := controlTestDB(t)
	router, err := NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	ctx := context.Background()

	tenantID, err := router.Create(ctx, "Acme Corp", "eu-west")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tenantID == "" {
		t.Fatal("expected a non-empty tenant id")
	}
	t.Cleanup(func() { dropTenantDatabase(t, control, tenantID) })

	var name, region, dbName string
	err = control.QueryRowContext(ctx,
		`SELECT name, region, db_name FROM tenants WHERE id = $1`, tenantID,
	).Scan(&name, &region, &dbName)
	if err != nil {
		t.Fatalf("query tenants row: %v", err)
	}
	if name != "Acme Corp" || region != "eu-west" {
		t.Fatalf("got name=%q region=%q, want Acme Corp/eu-west", name, region)
	}
	if !safeIdentifier.MatchString(dbName) {
		t.Fatalf("db_name %q does not match the expected safe-identifier shape", dbName)
	}

	pool, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("Get after Create: %v", err)
	}

	var count int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&count); err != nil {
		t.Fatalf("query records table in provisioned tenant database: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a freshly provisioned tenant database to have zero records, got %d", count)
	}

	// The whole point: this tenant's database must not carry a tenants
	// table of its own (that's the control plane's, not a tenant's).
	var tenantsExists bool
	err = pool.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'tenants')`,
	).Scan(&tenantsExists)
	if err != nil {
		t.Fatalf("check tenants table: %v", err)
	}
	if tenantsExists {
		t.Fatal("did not expect the control-plane tenants table inside a tenant database")
	}
}

// TestRouter_GetCachesThePool confirms a second Get for the same tenant
// returns the identical *sql.DB, not a fresh connection each time.
func TestRouter_GetCachesThePool(t *testing.T) {
	control, dsn := controlTestDB(t)
	router, err := NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	ctx := context.Background()

	tenantID, err := router.Create(ctx, "Cached Co", "eu-west")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { dropTenantDatabase(t, control, tenantID) })

	first, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	second, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if first != second {
		t.Fatal("expected Get to return the same cached *sql.DB on a second call")
	}
}

// TestRouter_GetUnknownTenantReturnsErrTenantNotFound confirms Get
// distinguishes "never provisioned" from any other failure mode.
func TestRouter_GetUnknownTenantReturnsErrTenantNotFound(t *testing.T) {
	control, dsn := controlTestDB(t)
	router, err := NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	_, err = router.Get(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected ErrTenantNotFound, got %v", err)
	}
}

// TestRouter_ConcurrentCreateForDifferentTenantsSucceeds is the
// regression test for the reason Create/provision don't wrap
// CREATE DATABASE in a transaction shared with anything else, and the
// reason Get uses singleflight keyed per tenant rather than a single
// global lock: provisioning two unrelated tenants at once must not
// serialize against or corrupt each other.
func TestRouter_ConcurrentCreateForDifferentTenantsSucceeds(t *testing.T) {
	control, dsn := controlTestDB(t)
	router, err := NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	ctx := context.Background()

	const n = 4
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = router.Create(ctx, fmt.Sprintf("Tenant %d", i), "eu-west")
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Create %d failed: %v", i, err)
		}
		if seen[ids[i]] {
			t.Fatalf("tenant id %s created more than once", ids[i])
		}
		seen[ids[i]] = true
		t.Cleanup(func(id string) func() { return func() { dropTenantDatabase(t, control, id) } }(ids[i]))
	}

	for _, id := range ids {
		if _, err := router.Get(ctx, id); err != nil {
			t.Fatalf("Get for concurrently created tenant %s: %v", id, err)
		}
	}
}

// TestRouter_CreateCleansUpOnProvisioningFailure confirms Create doesn't
// leave an orphaned tenants row if provisioning fails after the insert.
// Forced deterministically via a dedicated role with CREATEDB revoked
// (superuser test roles bypass privilege checks entirely, so this can't
// be forced by revoking anything from the default test role) — this
// doubles as a check that ADR-0003's own named operational requirement
// (the control connection needs CREATEDB) is real: without it, Create
// fails exactly this way, not silently.
func TestRouter_CreateCleansUpOnProvisioningFailure(t *testing.T) {
	control, dsn := controlTestDB(t)
	ctx := context.Background()

	const roleName = "uc_test_router_no_createdb"
	if _, err := control.ExecContext(ctx, `DROP ROLE IF EXISTS `+roleName); err != nil {
		t.Fatalf("drop pre-existing role: %v", err)
	}
	if _, err := control.ExecContext(ctx, `CREATE ROLE `+roleName+` LOGIN PASSWORD 'test' NOCREATEDB`); err != nil {
		t.Fatalf("create restricted role: %v", err)
	}
	t.Cleanup(func() { _, _ = control.Exec(`DROP ROLE IF EXISTS ` + roleName) })
	if _, err := control.ExecContext(ctx, `GRANT ALL ON TABLE tenants TO `+roleName); err != nil {
		t.Fatalf("grant table access to restricted role: %v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(roleName, "test")
	restrictedControl, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open restricted control connection: %v", err)
	}
	defer restrictedControl.Close()
	if err := restrictedControl.Ping(); err != nil {
		t.Fatalf("ping restricted control connection: %v", err)
	}

	router, err := NewRouter(restrictedControl, u.String())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	_, err = router.Create(ctx, "Doomed Co", "eu-west")
	if err == nil {
		t.Fatal("expected Create to fail: the control role has no CREATEDB privilege")
	}

	var count int
	if err := control.QueryRowContext(ctx, `SELECT count(*) FROM tenants WHERE name = $1`, "Doomed Co").Scan(&count); err != nil {
		t.Fatalf("count tenants rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected Create's failure to clean up the tenants row, found %d left over", count)
	}
}
