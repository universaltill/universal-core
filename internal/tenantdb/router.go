// Package tenantdb resolves a tenant id to that tenant's own database
// connection (ADR-0003): every tenant gets its own Postgres database on
// the same server, tracked in the control-plane tenants table. This is
// the one new piece of infrastructure the database-per-tenant migration
// adds — every other repository/call site just changes from taking a
// tenantID parameter to taking an already-resolved *sql.DB from here.
package tenantdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/universaltill/universal-core/internal/db"
)

// ErrTenantNotFound is returned by Get when tenantID has no row in the
// control-plane tenants table.
var ErrTenantNotFound = errors.New("tenantdb: tenant not found")

// safeIdentifier matches every db_name this package ever generates
// (Create's `uc_tenant_<32 hex chars>` — see the control migration's
// tenants.db_name column) and is deliberately more permissive than that
// one shape, so a future db_name convention doesn't need this package
// touched. Validated before ever being interpolated into `CREATE
// DATABASE`/a connection string regardless of source, since Postgres
// identifiers can't be parameterized the way values can — this check is
// load-bearing, not cosmetic.
var safeIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{2,62}$`)

func validateIdentifier(name string) error {
	if !safeIdentifier.MatchString(name) {
		return fmt.Errorf("tenantdb: refusing unsafe database identifier %q", name)
	}
	return nil
}

// quoteIdentifier double-quotes a Postgres identifier, doubling any
// embedded double-quote — defense in depth alongside validateIdentifier
// above, not a substitute for it (quoting alone doesn't stop a value
// from being e.g. a reserved word or containing a quote-escaping
// sequence worth rejecting outright).
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Router resolves tenant ids to per-tenant *sql.DB connections, caching
// opened pools. One Router per process, constructed once alongside the
// control-plane connection (see NewRouter).
type Router struct {
	control *sql.DB
	baseURL *url.URL

	mu    sync.RWMutex
	pools map[string]*sql.DB
	sf    singleflight.Group
}

// NewRouter builds a Router from an already-open, already-migrated
// control-plane connection and the DSN used to open it — the DSN is
// needed as a template for per-tenant connections (only the database
// name differs; host/port/user/password/sslmode carry over unchanged).
// Does not run any migration itself — the caller applies
// db.ApplyControl to controlDB before or after constructing a Router,
// same as any other repository/engine in this codebase.
func NewRouter(controlDB *sql.DB, controlDSN string) (*Router, error) {
	u, err := url.Parse(controlDSN)
	if err != nil {
		return nil, fmt.Errorf("tenantdb: parse control dsn: %w", err)
	}
	return &Router{
		control: controlDB,
		baseURL: u,
		pools:   make(map[string]*sql.DB),
	}, nil
}

// Get returns tenantID's own *sql.DB, opening and caching it on first
// use. Safe for concurrent use by multiple callers/goroutines, including
// concurrent first-use of the same never-before-seen tenantID (dedup'd
// via singleflight, not a coarse lock across every tenant).
func (r *Router) Get(ctx context.Context, tenantID string) (*sql.DB, error) {
	if pool, ok := r.cached(tenantID); ok {
		return pool, nil
	}

	v, err, _ := r.sf.Do(tenantID, func() (any, error) {
		if pool, ok := r.cached(tenantID); ok {
			return pool, nil
		}

		dbName, err := r.dbNameFor(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		pool, err := r.open(ctx, dbName)
		if err != nil {
			return nil, err
		}

		r.mu.Lock()
		r.pools[tenantID] = pool
		r.mu.Unlock()
		return pool, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*sql.DB), nil
}

func (r *Router) cached(tenantID string) (*sql.DB, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pool, ok := r.pools[tenantID]
	return pool, ok
}

func (r *Router) dbNameFor(ctx context.Context, tenantID string) (string, error) {
	var dbName string
	err := r.control.QueryRowContext(ctx,
		`SELECT db_name FROM tenants WHERE id = $1`, tenantID,
	).Scan(&dbName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTenantNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tenantdb: look up tenant %s: %w", tenantID, err)
	}
	return dbName, nil
}

func (r *Router) open(ctx context.Context, dbName string) (*sql.DB, error) {
	if err := validateIdentifier(dbName); err != nil {
		return nil, err
	}

	u := *r.baseURL
	u.Path = "/" + dbName
	pool, err := sql.Open("pgx", u.String())
	if err != nil {
		return nil, fmt.Errorf("tenantdb: open database %s: %w", dbName, err)
	}
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("tenantdb: ping database %s: %w", dbName, err)
	}
	return pool, nil
}

// Create provisions a brand-new tenant: inserts its control-plane row
// (name, region, a freshly generated db_name), creates the actual
// Postgres database, and applies the tenant migration set to it. The
// control-plane connection (passed to NewRouter) must have CREATE
// DATABASE privilege — a real operational requirement ADR-0003 calls
// out explicitly, not assumed here.
//
// CREATE DATABASE cannot run inside a transaction, so this isn't atomic
// with the tenants-row insert: if provisioning fails after the row is
// written, Create removes it (best effort) rather than leaving an
// orphaned tenant row with no matching database. A crash between the
// insert and that cleanup could still leave one — an inherent limit of
// spanning two databases, not something a transaction can fix here.
func (r *Router) Create(ctx context.Context, name, region string) (tenantID string, err error) {
	var dbName string
	err = r.control.QueryRowContext(ctx, `
		INSERT INTO tenants (name, region, db_name)
		VALUES ($1, $2, 'uc_tenant_' || replace(gen_random_uuid()::text, '-', ''))
		RETURNING id, db_name`, name, region,
	).Scan(&tenantID, &dbName)
	if err != nil {
		return "", fmt.Errorf("tenantdb: insert tenant row: %w", err)
	}

	if err := r.provision(ctx, tenantID, dbName); err != nil {
		_, _ = r.control.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
		return "", err
	}
	return tenantID, nil
}

func (r *Router) provision(ctx context.Context, tenantID, dbName string) error {
	if err := validateIdentifier(dbName); err != nil {
		return err
	}

	if _, err := r.control.ExecContext(ctx, `CREATE DATABASE `+quoteIdentifier(dbName)); err != nil {
		return fmt.Errorf("tenantdb: create database %s: %w", dbName, err)
	}

	pool, err := r.open(ctx, dbName)
	if err != nil {
		return err
	}

	if err := db.ApplyTenant(ctx, pool); err != nil {
		pool.Close()
		return fmt.Errorf("tenantdb: apply tenant migrations to %s: %w", dbName, err)
	}

	r.mu.Lock()
	r.pools[tenantID] = pool
	r.mu.Unlock()
	return nil
}

// Close closes every cached tenant connection pool. Intended for
// graceful process shutdown and test cleanup, not called mid-request.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for id, pool := range r.pools {
		if err := pool.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("tenantdb: close pool for tenant %s: %w", id, err)
		}
		delete(r.pools, id)
	}
	return firstErr
}
