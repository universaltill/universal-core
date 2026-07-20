package foundation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
)

// Publish brings tenantID's foundation layer online: every All()
// Definition, published into the entity_definitions registry via the
// normal draft -> approve -> publish lifecycle (moduleseed.PublishAll),
// the same path any Definition takes — not a bypass or a direct INSERT.
// This is the "Foundation entities used by every module are always
// present" requirement (ADR-0001 §8) actually happening for a given
// tenant, rather than just existing as Go values only tests construct.
//
// See moduleseed.PublishAll's doc comment for the idempotency/resume/
// concurrency contract this inherits unchanged.
func Publish(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	items := make([]moduleseed.Item, 0, len(All()))
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("foundation definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, tenantID, items, actor)
}

// PublishForms brings tenantID's foundation Form Definitions online —
// deliberately separate from Publish (a form is a presentation choice,
// not the same "always present" guarantee the entities themselves get;
// see forms.go's own doc comment) and separate from the entity registry
// (form_definitions, not entity_definitions). Without ever calling this,
// GET /forms/{entityType}/... 404s for every entity even after Publish —
// there was no code path that called it in production until
// cmd/provision-tenant wired one up (found by dogfooding the purchasing
// module: nothing had ever needed a real, non-test form publish before).
func PublishForms(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("foundation form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, tenantID, items, actor)
}
