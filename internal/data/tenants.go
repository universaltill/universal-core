package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	err := r.withSlugRetry(name, func(slug string) error {
		return r.db.QueryRowContext(ctx,
			`INSERT INTO tenants (name, region, slug) VALUES ($1, $2, $3) RETURNING id`,
			name, region, slug,
		).Scan(&id)
	})
	if err != nil {
		return "", fmt.Errorf("create tenant: %w", err)
	}
	return id, nil
}

// withSlugRetry runs insert with Slugify(name), then with -2..-9
// suffixes on a slug-uniqueness violation — the DB's unique index is
// the only collision authority (racing creates both pre-checking SELECT
// would still collide). Ten same-named tenants exhausting the suffixes
// is a "pick a different name" error, not a case to engineer around.
// NOT transaction-safe: inside a *sql.Tx the first violation aborts the
// transaction and every retry would fail with "current transaction is
// aborted", masking the real error — callers must pass a plain *sql.DB
// (both current ones do).
func (r *TenantRepo) withSlugRetry(name string, insert func(slug string) error) error {
	base := Slugify(name)
	slug := base
	for i := 2; ; i++ {
		err := insert(slug)
		if err == nil {
			return nil
		}
		if i > 9 || !isSlugUniqueViolation(err) {
			return err
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
}

// isSlugUniqueViolation matches the tenants_slug_key unique index by
// name — narrower than matching any 23505, so a db_name collision (a
// different bug) still surfaces as itself.
func isSlugUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "tenants_slug_key")
}

// CreateWithDatabase inserts a new tenant into the control-plane schema
// (ADR-0003), generating a fresh, safe db_name in the same statement —
// the control-plane counterpart of Create above. Used by
// internal/tenantdb.Router.Create, which then actually creates and
// migrates the returned db_name as a real Postgres database; this method
// only writes the registry row.
func (r *TenantRepo) CreateWithDatabase(ctx context.Context, name, region string) (id, dbName string, err error) {
	err = r.withSlugRetry(name, func(slug string) error {
		return r.db.QueryRowContext(ctx, `
			INSERT INTO tenants (name, region, db_name, slug)
			VALUES ($1, $2, 'uc_tenant_' || replace(gen_random_uuid()::text, '-', ''), $3)
			RETURNING id, db_name`,
			name, region, slug,
		).Scan(&id, &dbName)
	})
	if err != nil {
		return "", "", fmt.Errorf("create tenant: %w", err)
	}
	return id, dbName, nil
}

// GetBySlug resolves a URL slug (/t/{slug}/…, ADR-0011) to a tenant id.
func (r *TenantRepo) GetBySlug(ctx context.Context, slug string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM tenants WHERE slug = $1`, slug,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get tenant by slug: %w", err)
	}
	return id, nil
}

// SlugByID is GetBySlug's inverse — what the canonicalizing redirect
// prefixes an un-prefixed browser URL with.
func (r *TenantRepo) SlugByID(ctx context.Context, tenantID string) (string, error) {
	var slug string
	err := r.db.QueryRowContext(ctx,
		`SELECT slug FROM tenants WHERE id = $1`, tenantID,
	).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get tenant slug: %w", err)
	}
	return slug, nil
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

// NamesByIDs resolves display names for a set of tenant ids — the
// tenant picker/switcher's one lookup (webauth choose.go, #25). Ids
// with no row are simply absent from the map; callers skip them (a
// tenant deleted since the session was sealed).
func (r *TenantRepo) NamesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	names := make(map[string]string, len(ids))
	// One indexed point-lookup per id, not an ANY(array) bind: the set
	// is a user's accessible tenants (a handful at most), and this repo
	// has no array-parameter precedent to lean on — a wrong uuid[]/text[]
	// cast here would fail only at runtime.
	for _, id := range ids {
		var name string
		err := r.db.QueryRowContext(ctx,
			`SELECT name FROM tenants WHERE id = $1`, id,
		).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get tenant name: %w", err)
		}
		names[id] = name
	}
	return names, nil
}

// GetZitadelOrgID is SetZitadelOrgID's read side: which Zitadel org (if
// any) tenantID is linked to. The members page (internal/api/members.go)
// calls this per request to scope its management-API calls — a tenant
// with no linked org gets the page's "unavailable" state, the same
// degrade-visibly contract as an unconfigured client, since there is no
// org to manage members in.
func (r *TenantRepo) GetZitadelOrgID(ctx context.Context, tenantID string) (string, error) {
	var orgID sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT zitadel_org_id FROM tenants WHERE id = $1`,
		tenantID,
	).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get tenant zitadel org id: %w", err)
	}
	return orgID.String, nil
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
