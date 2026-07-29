package purchasing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
)

// Publish brings a tenant's Purchasing module online (the *sql.DB passed
// in is already resolved to that tenant's own database — ADR-0003):
// every All() Definition, published into the entity_definitions registry
// via the normal draft -> approve -> publish lifecycle
// (moduleseed.PublishAll, shared with internal/kernel/foundation.Publish
// — see that package's doc comment for the idempotency/resume/
// concurrency contract this inherits unchanged).
//
// Unlike foundation, Purchasing is NOT part of every tenant's baseline —
// ADR-0001 §8 draws the "always present" line specifically around the
// foundation set. Call this only for a tenant that has actually licensed
// the Purchasing module (module-gating itself isn't built yet — this
// function doesn't check anything, the caller decides whether to call it
// — see QUEUE.md).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
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
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Purchasing Form Definitions online —
// separate from Publish for the same reason
// foundation.PublishForms is separate from foundation.Publish (a form is
// a presentation choice, not the "always present" entity guarantee).
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
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
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishStatuses seeds the actual StatusType/Status/StatusTransition
// *records* PurchaseOrder's StatusTypeCode ("purchase_order_status")
// needs to resolve before crud.Engine.ValidateStatusTransition stops
// rejecting every PurchaseOrder create/update with "status type ... is
// not published for this tenant". foundation.go's StatusType/Status/
// StatusTransition are ordinary entity Definitions — real records via
// crud.Engine, not a bespoke table (see that package's doc comment) — so
// unlike Publish/PublishForms above (registry metadata, identical for
// every tenant) this is genuine tenant data, and unlike
// cmd/seed-demo-data's sample business data (optional, safe to skip)
// it's required module setup: no tenant can create a real PurchaseOrder
// through internal/api without it.
//
// Looks StatusType/Status/StatusTransition up through the registry
// (data.EntityDefinitionRepo.GetPublished), the same as
// cmd/seed-demo-data's seeder.def helper, rather than importing
// foundation.StatusType()/Status()/StatusTransition() directly —
// purchasing has no Go-level dependency on foundation (PurchaseOrder's
// own doc comment: the dependency is "foundation published for this
// tenant" at runtime, resolved through the registry, same as every
// other cross-module reference field in this kernel). Requires
// foundation to already be published in this database (ADR-0001 §8;
// cmd/provision-tenant always publishes foundation before any module).
//
// The transition graph is PurchaseOrder's own doc comment's design call:
// draft is the only is_initial status; draft->submitted->approved->
// received is the happy path; cancellation is reachable from draft,
// submitted, or approved but not from received (goods already arrived —
// reversing that is a return/credit-note event, a different real-world
// action, not a status edit) or cancelled itself. received and cancelled
// are both is_terminal.
//
// Idempotent: every StatusType/Status looked up by its code, every
// StatusTransition by its from_status_id/to_status_id pair, same
// getOrCreate-by-natural-key shape cmd/seed-demo-data's seeder already
// uses — safe to call on a tenant that's already been seeded (a repeat
// module publish, say).
func PublishStatuses(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)

	def := func(entityType string) (*entity.Definition, error) {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			return nil, fmt.Errorf("look up published %s: %w", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			return nil, fmt.Errorf("unmarshal %s definition: %w", entityType, err)
		}
		return d, nil
	}
	getOrCreate := func(d *entity.Definition, keyField, keyValue string, fields map[string]any) (string, error) {
		existing, err := engine.ListByField(ctx, d, keyField, keyValue)
		if err != nil {
			return "", fmt.Errorf("list %s by %s: %w", d.EntityType, keyField, err)
		}
		if len(existing) > 0 {
			return existing[0].ID, nil
		}
		rec, err := engine.Create(ctx, d, fields, actor)
		if err != nil {
			return "", fmt.Errorf("create %s %v: %w", d.EntityType, fields, err)
		}
		return rec.ID, nil
	}

	statusTypeDef, err := def("StatusType")
	if err != nil {
		return err
	}
	statusDef, err := def("Status")
	if err != nil {
		return err
	}
	transitionDef, err := def("StatusTransition")
	if err != nil {
		return err
	}

	statusTypeID, err := getOrCreate(statusTypeDef, "code", "purchase_order_status", map[string]any{
		"entity_type": "PurchaseOrder", "code": "purchase_order_status", "name": "Purchase Order Status",
	})
	if err != nil {
		return fmt.Errorf("seed purchase_order_status StatusType: %w", err)
	}

	statuses := []struct {
		code, name            string
		sequence              float64
		isInitial, isTerminal bool
	}{
		{"draft", "Draft", 1, true, false},
		{"submitted", "Submitted", 2, false, false},
		{"approved", "Approved", 3, false, false},
		{"received", "Received", 4, false, true},
		{"cancelled", "Cancelled", 5, false, true},
	}
	// Scoped by status_type_id, not just code: sales.PublishStatuses
	// (internal/kernel/sales/seed.go) now seeds sales_order_status and
	// customer_invoice_status, both of which declare their own "draft"
	// and (sales_order_status) "cancelled" Status rows — a plain
	// code-only getOrCreate here could find one of *their* rows first
	// (RecordRepo.ListByField orders by created_at, not by
	// status_type_id) and silently reuse its id instead of creating
	// purchase_order_status's own, corrupting this graph. Found by
	// independent review while adding the sales module, which hit the
	// identical bug in its own seedStatusGraph first — see that
	// function's own doc comment for the full explanation this mirrors.
	existingByCode := map[string]string{}
	existingStatuses, err := engine.ListByField(ctx, statusDef, "status_type_id", statusTypeID)
	if err != nil {
		return fmt.Errorf("list existing Status for purchase_order_status: %w", err)
	}
	for _, s := range existingStatuses {
		if c, _ := s.Data["code"].(string); c != "" {
			existingByCode[c] = s.ID
		}
	}

	statusIDs := make(map[string]string, len(statuses))
	for _, s := range statuses {
		if id, ok := existingByCode[s.code]; ok {
			statusIDs[s.code] = id
			continue
		}
		rec, err := engine.Create(ctx, statusDef, map[string]any{
			"status_type_id": statusTypeID, "code": s.code, "name": s.name,
			"sequence": s.sequence, "is_initial": s.isInitial, "is_terminal": s.isTerminal,
		}, actor)
		if err != nil {
			return fmt.Errorf("seed %s Status: %w", s.code, err)
		}
		statusIDs[s.code] = rec.ID
	}

	for _, edge := range [][2]string{
		{"draft", "submitted"},
		{"submitted", "approved"},
		{"approved", "received"},
		{"draft", "cancelled"},
		{"submitted", "cancelled"},
		{"approved", "cancelled"},
	} {
		from, to := statusIDs[edge[0]], statusIDs[edge[1]]
		existing, err := engine.ListByField(ctx, transitionDef, "from_status_id", from)
		if err != nil {
			return fmt.Errorf("list StatusTransition by from_status_id: %w", err)
		}
		found := false
		for _, t := range existing {
			if to2, _ := t.Data["to_status_id"].(string); to2 == to {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if _, err := engine.Create(ctx, transitionDef, map[string]any{
			"status_type_id": statusTypeID, "from_status_id": from, "to_status_id": to,
		}, actor); err != nil {
			return fmt.Errorf("seed StatusTransition %s->%s: %w", edge[0], edge[1], err)
		}
	}
	return nil
}
