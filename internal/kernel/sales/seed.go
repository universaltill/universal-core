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
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
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

// statusSpec aliases the shared seeder's Spec — see
// internal/kernel/statusgraph's doc comment for the third-consumer
// extraction (purchasing has the twin note).
type statusSpec = statusgraph.Spec

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

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"SalesOrder", "sales_order_status", "Sales Order Status",
		[]statusSpec{
			{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true, IsTerminal: false},
			{Code: "confirmed", Name: "Confirmed", Sequence: 2, IsInitial: false, IsTerminal: false},
			{Code: "fulfilled", Name: "Fulfilled", Sequence: 3, IsInitial: false, IsTerminal: false},
			{Code: "invoiced", Name: "Invoiced", Sequence: 4, IsInitial: false, IsTerminal: true},
			{Code: "cancelled", Name: "Cancelled", Sequence: 5, IsInitial: false, IsTerminal: true},
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

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"CustomerInvoice", "customer_invoice_status", "Customer Invoice Status",
		[]statusSpec{
			{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true, IsTerminal: false},
			{Code: "issued", Name: "Issued", Sequence: 2, IsInitial: false, IsTerminal: false},
			{Code: "paid", Name: "Paid", Sequence: 3, IsInitial: false, IsTerminal: true},
			{Code: "void", Name: "Void", Sequence: 4, IsInitial: false, IsTerminal: true},
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
