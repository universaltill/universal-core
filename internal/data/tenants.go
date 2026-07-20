package data

import (
	"context"
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
