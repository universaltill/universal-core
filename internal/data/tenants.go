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

// Create inserts a new tenant and returns its generated id.
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
