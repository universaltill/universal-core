package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
)

// Publish brings a tenant's Finance module online — same mechanism as
// purchasing.Publish/sales.Publish (see purchasing.Publish's own doc
// comment for the idempotency/resume/concurrency contract this inherits
// unchanged). No PublishStatuses counterpart: none of this module's
// entities opted into the foundation Status/StatusType pattern yet (see
// finance.go's own doc comments on why FiscalYear/Period use a plain
// FieldEnum for now).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	items := make([]moduleseed.Item, 0, len(All()))
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("finance definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Finance Form Definitions online —
// separate from Publish for the same reason purchasing.PublishForms is
// separate from purchasing.Publish.
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("finance form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}
