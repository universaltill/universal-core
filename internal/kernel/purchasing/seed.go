package purchasing

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

// Publish brings tenantID's Purchasing module online: every All()
// Definition, published into the entity_definitions registry via the
// normal draft -> approve -> publish lifecycle — same mechanism as
// internal/kernel/foundation.Publish (see that function's doc comment
// for the full rationale: idempotent, resumable from a partial prior
// failure, not safe for concurrent same-tenant calls but every collision
// is a clean retryable error, never corruption).
//
// This is a second, independent copy of that logic rather than a shared
// helper — deliberately: this codebase's kernel packages already favor
// independence over sharing between similar-looking engines when the
// alternative is threading one module's concerns through another (see
// queue.go's own comment on why Execute and Queue.ProcessOne don't share
// step-iteration code). Two occurrences doesn't meet that bar yet; if a
// third module needs this exact pattern, extract a shared helper then
// (e.g. into a new internal/kernel/moduleseed package) rather than
// duplicating a third time — don't extract speculatively before that.
//
// Unlike foundation, Purchasing is NOT part of every tenant's baseline —
// ADR-0001 §8 draws the "always present" line specifically around the
// foundation set. Call this only for a tenant that has actually licensed
// the Purchasing module (module-gating itself isn't built yet — this
// function doesn't check anything, the caller decides whether to call it
// — see QUEUE.md).
func Publish(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("purchasing definition %s is invalid: %w", def.EntityType, err)
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
