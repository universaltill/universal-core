package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TenantRepo is the repository for the tenants table. Its only
// production caller so far is cmd/provision-tenant (tests across this
// codebase insert tenants directly via SQL as part of their own DB
// setup, which is a different concern than a real code path — CLAUDE.md's
// repository-pattern rule is about where raw SQL lives in the running
// application, not test fixtures).
type TenantRepo struct {
	db querier
}

func NewTenantRepo(db querier) *TenantRepo {
	return &TenantRepo{db: db}
}

// Create inserts a new tenant and returns its generated id. Legacy-schema
// shape (no db_name) — only valid against the original shared-database
// tenants table (internal/db/migrations/001_init.sql), not the
// control-plane schema (internal/db/migrations/control/), which requires
// db_name. Use CreateWithDatabase against the control-plane schema.
func (r *TenantRepo) Create(ctx context.Context, name, region string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name, region) VALUES ($1, $2) RETURNING id`,
		name, region,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create tenant: %w", err)
	}
	return id, nil
}

// CreateWithDatabase inserts a new tenant into the control-plane schema
// (ADR-0003), generating a fresh, safe db_name in the same statement —
// the control-plane counterpart of Create above. Used by
// internal/tenantdb.Router.Create, which then actually creates and
// migrates the returned db_name as a real Postgres database; this method
// only writes the registry row.
func (r *TenantRepo) CreateWithDatabase(ctx context.Context, name, region string) (id, dbName string, err error) {
	err = r.db.QueryRowContext(ctx, `
		INSERT INTO tenants (name, region, db_name)
		VALUES ($1, $2, 'uc_tenant_' || replace(gen_random_uuid()::text, '-', ''))
		RETURNING id, db_name`,
		name, region,
	).Scan(&id, &dbName)
	if err != nil {
		return "", "", fmt.Errorf("create tenant: %w", err)
	}
	return id, dbName, nil
}

// DBName resolves tenantID's own database name from the control-plane
// registry — internal/tenantdb.Router.Get's own lookup, kept here per
// CLAUDE.md's repository-pattern rule (raw SQL lives only in
// internal/data).
func (r *TenantRepo) DBName(ctx context.Context, tenantID string) (string, error) {
	var dbName string
	err := r.db.QueryRowContext(ctx,
		`SELECT db_name FROM tenants WHERE id = $1`, tenantID,
	).Scan(&dbName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up tenant %s db_name: %w", tenantID, err)
	}
	return dbName, nil
}

// Delete removes a tenant's control-plane row — internal/tenantdb.Router.
// Create's best-effort cleanup when provisioning the actual database
// fails after this row was written (see that method's own doc comment
// on why this can't be atomic with CREATE DATABASE).
func (r *TenantRepo) Delete(ctx context.Context, tenantID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	if err != nil {
		return fmt.Errorf("delete tenant %s: %w", tenantID, err)
	}
	return nil
}

// ListIDs returns every tenant id in the control-plane registry — what
// internal/worker's fan-out needs to poll every tenant's own workflow
// job queue (ADR-0003: ClaimNext/ReclaimStale are each scoped to one
// tenant's database now, so something above them has to iterate every
// tenant). No pagination yet — fine at today's tenant count, revisit
// alongside ADR-0001 §3's own named "doesn't scale past a few hundred
// tenants" risk if this ever needs to.
func (r *TenantRepo) ListIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list tenant ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan tenant id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetZitadelOrgID links tenantID to a Zitadel organization — the
// one-time admin action that makes real login (internal/webauth)
// resolvable for that tenant at all; a tenant with no linked org can
// still be used via httpx.DevAuth, but every real Zitadel sign-in for
// its org's members ends at webauth's "no tenant linked" page until
// this runs. No self-serve onboarding flow calls this yet — it's a
// deliberate manual step (matches cmd/provision-tenant's own current
// scope), not something a login attempt can trigger itself.
func (r *TenantRepo) SetZitadelOrgID(ctx context.Context, tenantID, orgID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE tenants SET zitadel_org_id = $1 WHERE id = $2`,
		orgID, tenantID,
	)
	if err != nil {
		return fmt.Errorf("set tenant zitadel org id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByZitadelOrgID resolves a Zitadel organization id (an id_token claim,
// see internal/webauth) to the Universal Core tenant it's linked to —
// internal/webauth's login callback's only per-sign-in DB lookup; every
// later request reads the already-resolved tenant_id straight out of the
// sealed session cookie, not this query again.
func (r *TenantRepo) GetByZitadelOrgID(ctx context.Context, orgID string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM tenants WHERE zitadel_org_id = $1`,
		orgID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get tenant by zitadel org id: %w", err)
	}
	return id, nil
}
