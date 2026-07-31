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
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
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

// statusSpec aliases the shared seeder's Spec — the third consumer
// (internal/kernel/modulebundle) pulled the twin purchasing/sales
// copies into internal/kernel/statusgraph, the same threshold that
// created moduleseed. A kernel dependency, not the cross-module import
// the original duplication note ruled out.
type statusSpec = statusgraph.Spec

// PublishStatuses seeds the actual StatusType/Status/StatusTransition
// *records* PurchaseOrder's StatusTypeCode ("purchase_order_status") and
// VendorInvoice's StatusTypeCode ("vendor_invoice_status") need to
// resolve before crud.Engine.ValidateStatusTransition stops rejecting
// every create/update of either with "status type ... is not published
// for this tenant". foundation.go's StatusType/Status/StatusTransition
// are ordinary entity Definitions — real records via crud.Engine, not a
// bespoke table (see that package's doc comment) — so unlike
// Publish/PublishForms above (registry metadata, identical for every
// tenant) this is genuine tenant data, and unlike cmd/seed-demo-data's
// sample business data (optional, safe to skip) it's required module
// setup: no tenant can create a real PurchaseOrder or VendorInvoice
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
// purchase_order_status: draft is the only is_initial status; draft->
// submitted->approved->received is the happy path; cancellation is
// reachable from draft, submitted, or approved but not from received
// (goods already arrived — reversing that is a return/credit-note
// event, a different real-world action, not a status edit) or cancelled
// itself. received and cancelled are both is_terminal.
//
// vendor_invoice_status (VendorInvoice's own doc comment): draft is the
// only is_initial status; draft->matched->paid is the happy path — the
// "matched" transition is where purchasing.MatchVendorInvoiceOnUpdate
// (ledger.go) runs the 3-way match and rejects the transition outright
// (nothing written) if the invoice doesn't agree with what was actually
// received against its PurchaseOrder. void is reachable from draft or
// matched but not paid, same "money already moved, that's a credit
// note/reversal event not a status edit" reasoning as
// sales.PublishStatuses gives for CustomerInvoice's identical shape.
//
// rfq_status (RequestForQuotation's own doc comment, #9): draft is the
// only is_initial status — an RFQ being drafted, vendors/lines still
// being assembled. draft->sent is "the RFQ was actually sent to its
// invited vendors" (a real-world action, not a data edit — nothing else
// in this Definition forces vendors/lines to be non-empty at that
// point, same "declared, not schema-enforced" trust this kernel already
// places in a form submission generally). sent->quotes_received is "at
// least one vendor has responded and staff started recording quote
// lines" — deliberately not auto-derived from RequestForQuotationQuoteLine
// rows existing (this package's generic engine has no cross-record
// "does a child exist" trigger, and forcing one in here would be exactly
// the kind of entity-specific machinery CLAUDE.md's kernel-boundary rule
// warns against outside a Definition); a human/staff action moves it,
// same as every other status transition in this kernel.
// quotes_received->closed is "a vendor was chosen (or the RFQ abandoned
// with an answer in hand) and comparison is done" — deliberately NOT
// "converts to a PurchaseOrder": that conversion is explicit non-goal
// scope for #9 (a future task's job), so "closed" only means "this RFQ's
// own lifecycle is finished," not "a PO now exists." cancellation is
// reachable from draft, sent, or quotes_received but not from closed or
// cancelled itself — once closed, reopening/cancelling is a new RFQ, the
// same "the real-world event has already happened, that's not a status
// edit" reasoning purchase_order_status gives for excluding "received".
// closed and cancelled are both is_terminal.
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
		"PurchaseOrder", "purchase_order_status", "Purchase Order Status",
		[]statusSpec{
			{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true, IsTerminal: false},
			{Code: "submitted", Name: "Submitted", Sequence: 2, IsInitial: false, IsTerminal: false},
			{Code: "approved", Name: "Approved", Sequence: 3, IsInitial: false, IsTerminal: false},
			{Code: "received", Name: "Received", Sequence: 4, IsInitial: false, IsTerminal: true},
			{Code: "cancelled", Name: "Cancelled", Sequence: 5, IsInitial: false, IsTerminal: true},
		},
		[][2]string{
			{"draft", "submitted"},
			{"submitted", "approved"},
			{"approved", "received"},
			{"draft", "cancelled"},
			{"submitted", "cancelled"},
			{"approved", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed purchase_order_status: %w", err)
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"VendorInvoice", "vendor_invoice_status", "Vendor Invoice Status",
		[]statusSpec{
			{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true, IsTerminal: false},
			{Code: "matched", Name: "Matched", Sequence: 2, IsInitial: false, IsTerminal: false},
			{Code: "paid", Name: "Paid", Sequence: 3, IsInitial: false, IsTerminal: true},
			{Code: "void", Name: "Void", Sequence: 4, IsInitial: false, IsTerminal: true},
		},
		[][2]string{
			{"draft", "matched"},
			{"matched", "paid"},
			{"draft", "void"},
			{"matched", "void"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed vendor_invoice_status: %w", err)
	}

	if _, err := seedStatusGraph(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"RequestForQuotation", "rfq_status", "RFQ Status",
		[]statusSpec{
			{"draft", "Draft", 1, true, false},
			{"sent", "Sent", 2, false, false},
			{"quotes_received", "Quotes Received", 3, false, false},
			{"closed", "Closed", 4, false, true},
			{"cancelled", "Cancelled", 5, false, true},
		},
		[][2]string{
			{"draft", "sent"},
			{"sent", "quotes_received"},
			{"quotes_received", "closed"},
			{"draft", "cancelled"},
			{"sent", "cancelled"},
			{"quotes_received", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed rfq_status: %w", err)
	}

	return nil
}
