package workflow

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
)

// RegistryDefinitionLookup builds a DefinitionLookup backed by
// data.WorkflowDefinitionRepo — the real implementation
// Queue.ProcessOne's caller (a worker process, not built yet — see
// QUEUE.md) should use, in place of the hand-built stub every test in
// this package constructs. It looks up the exact (tenantID, name,
// version) a job was enqueued against — not just "whatever's currently
// published" — since a running job must keep executing the Definition it
// started against even if a newer version gets published mid-run.
func RegistryDefinitionLookup(db *sql.DB) DefinitionLookup {
	repo := data.NewWorkflowDefinitionRepo(db)
	return func(ctx context.Context, tenantID, name string, version int) (*Definition, error) {
		v, err := repo.GetVersion(ctx, tenantID, name, version)
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
