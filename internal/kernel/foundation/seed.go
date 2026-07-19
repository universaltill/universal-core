package foundation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// Publish brings tenantID's foundation layer online: every All()
// Definition, published into the entity_definitions registry via the
// normal draft -> approve -> publish lifecycle (internal/data), the same
// path any Definition takes — not a bypass or a direct INSERT. This is
// the "Foundation entities used by every module are always present"
// requirement (ADR-0001 §8) actually happening for a given tenant,
// rather than just existing as Go values only tests construct.
//
// Idempotent, and resumable from a partial prior failure: each
// Definition's registry row is driven forward from whatever state it's
// already in (not present, draft, or approved) to published, rather than
// assuming a prior call either fully completed or never ran — a process
// that crashed between CreateDraft and Publish on an earlier call would
// otherwise leave that Definition stuck in draft forever, since a
// naive "does a row already exist" check would find one and skip it
// without ever finishing the job. A row already published, or
// deliberately rolled_back, is left alone either way.
//
// Not safe to call concurrently for the SAME tenant: publishOne is
// check-then-act (GetVersion, then conditionally write), not a single
// atomic operation. Two racing calls can collide — a duplicate
// CreateDraft hits entity_definitions' UNIQUE (tenant_id, entity_type,
// version) constraint, a duplicate Approve/Publish hits
// definitionRepo.transition's atomic status-guarded UPDATE — but every
// collision is a clean error on the losing call, never a wrong
// ordering or a corrupted row, and Publish is idempotent, so simply
// retrying a call that errored this way converges to the same
// fully-published end state. Expected call pattern is once per tenant
// onboarding (a cold path), where this doesn't come up in practice.
func Publish(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("foundation definition %s is invalid: %w", def.EntityType, err)
		}
		if err := publishOne(ctx, repo, tenantID, def, actor); err != nil {
			return err
		}
	}
	return nil
}

func publishOne(ctx context.Context, repo *data.EntityDefinitionRepo, tenantID string, def *entity.Definition, actor audit.Actor) error {
	v, err := repo.GetVersion(ctx, tenantID, def.EntityType, def.Version)
	status := v.Status
	switch {
	case errors.Is(err, data.ErrNotFound):
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		if _, err := repo.CreateDraft(ctx, tenantID, def.EntityType, def.Version, raw, actor); err != nil {
			return fmt.Errorf("draft %s: %w", def.EntityType, err)
		}
		status = data.StatusDraft
	case err != nil:
		return fmt.Errorf("check existing %s v%d: %w", def.EntityType, def.Version, err)
	}

	if status == data.StatusPublished || status == data.StatusRolledBack {
		return nil
	}
	if status == data.StatusDraft {
		if err := repo.Approve(ctx, tenantID, def.EntityType, def.Version, actor); err != nil {
			return fmt.Errorf("approve %s: %w", def.EntityType, err)
		}
	}
	if err := repo.Publish(ctx, tenantID, def.EntityType, def.Version, actor); err != nil {
		return fmt.Errorf("publish %s: %w", def.EntityType, err)
	}
	return nil
}
