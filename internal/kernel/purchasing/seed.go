package purchasing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
)

// Publish brings tenantID's Purchasing module online: every All()
// Definition, published into the entity_definitions registry via the
// normal draft -> approve -> publish lifecycle (moduleseed.PublishAll,
// shared with internal/kernel/foundation.Publish — see that package's
// doc comment for the idempotency/resume/concurrency contract this
// inherits unchanged).
//
// Unlike foundation, Purchasing is NOT part of every tenant's baseline —
// ADR-0001 §8 draws the "always present" line specifically around the
// foundation set. Call this only for a tenant that has actually licensed
// the Purchasing module (module-gating itself isn't built yet — this
// function doesn't check anything, the caller decides whether to call it
// — see QUEUE.md).
func Publish(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	items := make([]moduleseed.Item, 0, len(All()))
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("purchasing definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, tenantID, items, actor)
}

// PublishForms brings tenantID's Purchasing Form Definitions online —
// separate from Publish for the same reason
// foundation.PublishForms is separate from foundation.Publish (a form is
// a presentation choice, not the "always present" entity guarantee).
func PublishForms(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := []*form.Definition{ItemForm(), PurchaseOrderForm(), POLineForm(), InventoryItemForm()}
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("purchasing form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, tenantID, items, actor)
}
