// Package db embeds and applies this kernel's SQL migrations
// (internal/db/migrations/*.sql — CLAUDE.md: raw SQL lives only here and
// in internal/data). Embedding means the compiled binary carries its own
// migrations; a deployment never needs the source tree or a separate
// migration-runner image alongside it.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// controlFS/tenantFS (ADR-0003): the control-plane database (tenants
// registry only) and per-tenant databases (everything else, tenant_id
// columns removed) each get their own independent migration set and
// schema_migrations history, applied via ApplyControl/ApplyTenant below.
// migrationsFS/Apply above are untouched, not superseded — they're what
// actually ran against the original shared database and stay usable for
// it until that database is decommissioned (a separate, later decision,
// not made here).
//
//go:embed migrations/control/*.sql
var controlFS embed.FS

//go:embed migrations/tenant/*.sql
var tenantFS embed.FS

// migrationLockKey is an arbitrary constant used with pg_advisory_lock to
// serialize concurrent Apply calls — e.g. several replicas booting
// simultaneously against a fresh database. Neither `CREATE TABLE IF NOT
// EXISTS schema_migrations` nor the per-migration `SELECT EXISTS ...
// INSERT` check-then-write is itself a safe compare-and-set under
// concurrent execution (confirmed empirically: 4 of 5 concurrent Apply
// calls against a fresh database failed outright on a duplicate-key
// error from the race in CREATE TABLE IF NOT EXISTS, crash-looping every
// replica but one). The lock makes only one caller run migrations at a
// time; the rest block on pg_advisory_lock until it's done, then find
// everything already applied and return immediately.
const migrationLockKey = 727271

// Separate lock keys for the control-plane and per-tenant migration sets
// (ADR-0003): pg_advisory_lock's key space is shared across the whole
// Postgres server/cluster, not scoped to the database a session happens
// to be connected to — reusing migrationLockKey here would make
// ApplyTenant for one tenant's brand-new database serialize against
// ApplyTenant for a completely unrelated tenant's, or against
// migrationLockKey's original concurrent-replica-boot scenario, for no
// reason. Tenant provisioning across different tenants still serializes
// against itself under migrationLockKeyTenant — acceptable while
// provisioning is rare and not a hot path; revisit only if that changes.
const (
	migrationLockKeyControl = 727272
	migrationLockKeyTenant  = 727273
)

// Apply runs every embedded migration not yet recorded in
// schema_migrations, in filename order (001_, 002_, ... — the append-only
// numbering CLAUDE.md's Process section requires), each in its own
// transaction. Safe to call on every process start: an already-applied
// migration is a no-op, so this is how cmd/universal-core brings a fresh
// database up to date without a separate migrate step. Also safe to call
// concurrently from multiple processes (see migrationLockKey).
//
// This is the original shared-database migration set (ADR-0003): it
// stays exactly as it was, untouched, since it's what actually ran
// against the pre-migration database — not superseded by
// ApplyControl/ApplyTenant below until that database is decommissioned
// (a separate, later decision, not made here).
func Apply(ctx context.Context, sqlDB *sql.DB) error {
	return applyFS(ctx, sqlDB, migrationsFS, "migrations", migrationLockKey)
}

// ApplyControl brings the control-plane database (the tenants registry
// only) up to date. See ApplyTenant for the per-tenant counterpart.
func ApplyControl(ctx context.Context, sqlDB *sql.DB) error {
	return applyFS(ctx, sqlDB, controlFS, "migrations/control", migrationLockKeyControl)
}

// ApplyTenant brings one tenant's own database up to date. Unlike Apply/
// ApplyControl (both re-run on every cmd/universal-core process start),
// the only automatic caller is internal/tenantdb.Router's Create, at a
// brand-new tenant's provisioning time — an already-provisioned tenant's
// database is opened by Router.Get without re-applying migrations, so a
// new tenant migration file does NOT reach existing tenants on its own.
// An operator brings an existing tenant database up to date by running
// cmd/migrate -target tenant against it directly (see that command's
// doc comment); nothing in this codebase does that automatically today.
func ApplyTenant(ctx context.Context, sqlDB *sql.DB) error {
	return applyFS(ctx, sqlDB, tenantFS, "migrations/tenant", migrationLockKeyTenant)
}

func applyFS(ctx context.Context, sqlDB *sql.DB, fsys embed.FS, dir string, lockKey int64) error {
	// Advisory locks are scoped to the SESSION (the specific connection)
	// that acquired them, so the lock must be held on one pinned Conn for
	// this call's whole duration — the pool's sql.DB is still used for the
	// actual migration statements below, since the lock's only job is to
	// stop a second concurrent caller from proceeding, not to keep every
	// individual statement on the same connection.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration lock: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// A background context: ctx may already be canceled/expired by
		// the time this call returns (e.g. the caller's own deadline), but
		// the lock must still be released — an unlock that never runs
		// wedges every future call against this database forever, not
		// just this one.
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := applyOne(ctx, sqlDB, fsys, dir, name); err != nil {
			return err
		}
	}
	return nil
}

func applyOne(ctx context.Context, sqlDB *sql.DB, fsys embed.FS, dir, name string) error {
	var applied bool
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, name,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check migration %s: %w", name, err)
	}
	if applied {
		return nil
	}

	stmt, err := fsys.ReadFile(dir + "/" + name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for migration %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	if _, err := tx.ExecContext(ctx, string(stmt)); err != nil {
		if isAlreadyExistsError(err) {
			return fmt.Errorf(
				"apply migration %s (from %s): a database object it creates already exists — "+
					"this database most likely already has a different migration set applied to it: "+
					"check DATABASE_URL, and which migration set ran against it before — "+
					"cmd/migrate's -target=legacy vs. the control-plane set that cmd/provision-tenant, "+
					"cmd/install-module and cmd/universal-core apply internally (via db.ApplyControl) "+
					"are not interchangeable; each target/internal caller applies a genuinely different "+
					"schema, not a duplicate of another (universaltill/uc-infra#84): %w",
				name, dir, err,
			)
		}
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (filename) VALUES ($1)`, name,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	return tx.Commit()
}

// alreadyExistsSQLStates are the Postgres "duplicate_*" error codes
// (class 42, syntax error or access rule violation) a migration
// statement can fail with when the object it's trying to create is
// already present — the symptom of applying two incompatible migration
// sets to the same database (universaltill/uc-infra#84), not a
// transient/retryable failure. See
// https://www.postgresql.org/docs/current/errcodes-appendix.html.
//
// duplicate_database (42P04) is deliberately not in this set: every
// migration statement here runs inside a transaction (applyOne's tx,
// above), and Postgres cannot run CREATE DATABASE inside a transaction
// block at all — that fails with 25001 (active_sql_transaction), never
// 42P04. Nothing under internal/db/migrations{,/control,/tenant} issues
// CREATE DATABASE, so 42P04 can't occur from this call site; including
// it would just be a dead, untestable-for-real allowlist entry
// (independent review of uc-infra#84's fix, 2026-08-05).
var alreadyExistsSQLStates = map[string]bool{
	"42P07": true, // duplicate_table
	"42701": true, // duplicate_column
	"42710": true, // duplicate_object (e.g. a constraint or index)
	"42P06": true, // duplicate_schema
	"42723": true, // duplicate_function — none of today's migrations define
	// one, but a future CREATE FUNCTION/TRIGGER migration hitting this
	// class of collision should get the same clear diagnosis, not a
	// silent fallback to the raw error (independent review).
}

// isAlreadyExistsError reports whether err is a Postgres error whose
// SQLSTATE indicates the migration failed because it tried to create
// something that already exists.
func isAlreadyExistsError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return alreadyExistsSQLStates[pgErr.Code]
}
