package workflow

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
)

// RegistryDefinitionLookup builds a DefinitionLookup backed by
// data.WorkflowDefinitionRepo, against one tenant's own database
// (ADR-0003) — the real implementation internal/worker's per-tenant
// Queue should use, in place of the hand-built stub every test in this
// package constructs. It looks up the exact (name, version) a job was
// enqueued against — not just "whatever's currently published" — since a
// running job must keep executing the Definition it started against
// even if a newer version gets published mid-run.
func RegistryDefinitionLookup(db *sql.DB) DefinitionLookup {
	repo := data.NewWorkflowDefinitionRepo(db)
	return func(ctx context.Context, name string, version int) (*Definition, error) {
		v, err := repo.GetVersion(ctx, name, version)
		if err != nil {
			return nil, fmt.Errorf("look up workflow definition %s v%d: %w", name, version, err)
		}
		def, err := Unmarshal(v.Definition)
		if err != nil {
			return nil, fmt.Errorf("workflow definition %s v%d: %w", name, version, err)
		}
		return def, nil
	}
}
