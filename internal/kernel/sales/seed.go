package sales

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

// Publish brings a tenant's Sales module online — same mechanism as
// purchasing.Publish (see that function's own doc comment for the
// idempotency/resume/concurrency contract this inherits unchanged, and
// for why the caller — not this function — decides whether a tenant has
// actually licensed Sales).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	items := make([]moduleseed.Item, 0, len(All()))
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("sales definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Sales Form Definitions online — separate
// from Publish for the same reason purchasing.PublishForms is separate
// from purchasing.Publish.
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("sales form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// statusSpec is one Status row within a StatusType's graph — the same
// shape purchasing.PublishStatuses declares inline, pulled out here since
// this package seeds two StatusTypes (sales_order_status,
// customer_invoice_status) rather than purchasing's one, and the
// getOrCreate-by-natural-key logic underneath is identical for both —
// the second real example purchasing.PurchaseOrder's own doc comment
// says to wait for before generalizing anything.
type statusSpec struct {
	code, name            string
	sequence              float64
	isInitial, isTerminal bool
}

// seedStatusGraph seeds one StatusType, its Status rows, and its
// StatusTransition edges — idempotent, same getOrCreate-by-natural-key
// discipline as purchasing.PublishStatuses and cmd/seed-demo-data's own
// seeder.getOrCreate. Returns the status code -> id map so the caller can
// build edges or hand it back for tests.
func seedStatusGraph(
	ctx context.Context,
	engine *crud.Engine,
	statusTypeDef, statusDef, transitionDef *entity.Definition,
	entityType, code, name string,
	statuses []statusSpec,
	edges [][2]string,
	actor audit.Actor,
) (map[string]string, error) {
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

	statusTypeID, err := getOrCreate(statusTypeDef, "code", code, map[string]any{
		"entity_type": entityType, "code": code, "name": name,
	})
	if err != nil {
		return nil, fmt.Errorf("seed %s StatusType: %w", code, err)
	}

	// Scoped by status_type_id, not just code: this function seeds more
	// than one StatusType per tenant (sales_order_status *and*
	// customer_invoice_status), and both declare a "draft" Status among
	// others — a plain code-only getOrCreate would find
	// sales_order_status's "draft" row first and silently reuse its id
	// for customer_invoice_status's own "draft" instead of creating a
	// second, correctly-scoped one (caught by
	// TestPublishStatuses_SeedsBothGraphs actually asserting each
	// graph's own Status count, not just that Publish "succeeded").
	existingByCode := map[string]string{}
	existingStatuses, err := engine.ListByField(ctx, statusDef, "status_type_id", statusTypeID)
	if err != nil {
		return nil, fmt.Errorf("list existing Status for %s: %w", code, err)
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
			return nil, fmt.Errorf("seed %s Status: %w", s.code, err)
		}
		statusIDs[s.code] = rec.ID
	}

	for _, edge := range edges {
		from, to := statusIDs[edge[0]], statusIDs[edge[1]]
		existing, err := engine.ListByField(ctx, transitionDef, "from_status_id", from)
		if err != nil {
			return nil, fmt.Errorf("list StatusTransition by from_status_id: %w", err)
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
			return nil, fmt.Errorf("seed StatusTransition %s->%s: %w", edge[0], edge[1], err)
		}
	}
	return statusIDs, nil
}

// PublishStatuses seeds the actual StatusType/Status/StatusTransition
// *records* both SalesOrder.StatusTypeCode ("sales_order_status") and
// CustomerInvoice.StatusTypeCode ("customer_invoice_status") need before
// crud.Engine.ValidateStatusTransition stops rejecting every
// create/update of either entity — same required-module-setup reasoning
// as purchasing.PublishStatuses's own doc comment (this is real tenant
// data, not optional sample data, and requires foundation already
// published — ADR-0001 §8; cmd/provision-tenant always publishes
// foundation first).
//
// sales_order_status: draft is the only is_initial status; draft->
// confirmed->fulfilled->invoiced is the happy path; cancellation is
// reachable from draft or confirmed but not from fulfilled or invoiced —
// once an order has actually shipped or been billed, reversing that is a
// return/credit-note event, not a status edit (the identical reasoning
// PurchaseOrder's own doc comment gives for "received" being a dead end
// for cancellation).
//
// customer_invoice_status: draft is the only is_initial status; draft->
// issued->paid is the happy path; void is reachable from draft or issued
// but not from paid — money has already changed hands at that point, so
// undoing it is a credit note / refund, not a status edit.
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

	if _, err := seedStatusGraph(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"SalesOrder", "sales_order_status", "Sales Order Status",
		[]statusSpec{
			{"draft", "Draft", 1, true, false},
			{"confirmed", "Confirmed", 2, false, false},
			{"fulfilled", "Fulfilled", 3, false, false},
			{"invoiced", "Invoiced", 4, false, true},
			{"cancelled", "Cancelled", 5, false, true},
		},
		[][2]string{
			{"draft", "confirmed"},
			{"confirmed", "fulfilled"},
			{"fulfilled", "invoiced"},
			{"draft", "cancelled"},
			{"confirmed", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed sales_order_status: %w", err)
	}

	if _, err := seedStatusGraph(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"CustomerInvoice", "customer_invoice_status", "Customer Invoice Status",
		[]statusSpec{
			{"draft", "Draft", 1, true, false},
			{"issued", "Issued", 2, false, false},
			{"paid", "Paid", 3, false, true},
			{"void", "Void", 4, false, true},
		},
		[][2]string{
			{"draft", "issued"},
			{"issued", "paid"},
			{"draft", "void"},
			{"issued", "void"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed customer_invoice_status: %w", err)
	}

	return nil
}
