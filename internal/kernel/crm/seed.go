package crm

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

// Publish brings a tenant's CRM module online — same mechanism as
// purchasing.Publish/sales.Publish (see purchasing's own doc comment
// for the idempotency/resume/concurrency contract this inherits
// unchanged, and for why the caller decides whether a tenant has
// licensed the module rather than this function checking).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	defs := All()
	items := make([]moduleseed.Item, 0, len(defs))
	for _, def := range defs {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("crm definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's CRM Form Definitions online —
// separate from Publish for the same reason every other module keeps
// them separate (a form is a presentation choice, not the entity
// guarantee).
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("crm form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishStatuses seeds the StatusType/Status/StatusTransition records
// this module's four StatusTypeCodes need before the guarded engine
// will accept a create (the shared seeder is internal/kernel/
// statusgraph, which scopes status codes by status_type_id — so
// `new` meaning a fresh Case and `new` meaning a fresh Lead do not
// collide, the bug fixed across purchasing/sales on 2026-07-29).
//
// case_status is a support workflow, which is neither a pure lifecycle
// nor an approval chain: new -> in_progress -> resolved -> closed is
// the happy path, with two edges that matter more than the happy path
// does.
//
// waiting_customer is reachable from in_progress and back again,
// because "we asked them a question" is the single commonest reason a
// case stalls, and an agent's queue is worthless if it cannot
// distinguish that from work in flight.
//
// resolved -> in_progress exists because the customer, not the agent,
// decides whether a problem is fixed. Forcing a new case for a
// reopening would scatter one problem's history across two records and
// silently flatter every "time to resolution" measure — the same
// reasoning projects.Task's done -> in_progress edge has.
//
// closed is terminal: it is the state an agent moves a case to when the
// customer has confirmed, or when the SLA clock should stop for good.
// cancelled is the other terminal end, for a case raised in error or
// duplicated, and is reachable from every live state but not from
// closed.
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
		"Case", "case_status", "Case Status",
		[]statusgraph.Spec{
			{Code: "new", Name: "New", Sequence: 1, IsInitial: true},
			{Code: "in_progress", Name: "In Progress", Sequence: 2},
			{Code: "waiting_customer", Name: "Waiting on Customer", Sequence: 3},
			{Code: "resolved", Name: "Resolved", Sequence: 4},
			{Code: "closed", Name: "Closed", Sequence: 5, IsTerminal: true},
			{Code: "cancelled", Name: "Cancelled", Sequence: 6, IsTerminal: true},
		},
		[][2]string{
			{"new", "in_progress"},
			{"in_progress", "waiting_customer"},
			{"waiting_customer", "in_progress"},
			{"in_progress", "resolved"},
			// The customer decides whether it is fixed.
			{"resolved", "in_progress"},
			{"resolved", "closed"},
			{"new", "cancelled"},
			{"in_progress", "cancelled"},
			{"waiting_customer", "cancelled"},
			// Including from resolved: an agent who resolves a case and
			// then finds it was a duplicate must be able to cancel it.
			// Without this edge the only routes were resolved -> closed
			// (recording a real resolution that never happened) or a
			// detour through in_progress — a spurious reopen, which
			// corrupts exactly the resolution-time measure the reopen
			// edge above exists to protect (independent review).
			{"resolved", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed case_status: %w", err)
	}

	// lead_status is a qualification funnel with two exits and no way
	// back in. converted is where a lead stops being a lead: a real
	// Party now exists (Lead.converted_party_id records which), and
	// anything further happens on the Opportunity. disqualified is the
	// other end — wrong fit, no budget, or a duplicate.
	//
	// Both are terminal, which means **a disqualified lead that comes
	// back is a new lead**, not a reopened one. That is deliberate and
	// it is the opposite of Case's resolved -> in_progress edge, for a
	// reason worth stating: reopening a case preserves one problem's
	// history, whereas a prospect returning eighteen months later is a
	// genuinely new opportunity to win, and folding it into the old
	// record would date the lead to the first contact and quietly
	// corrupt every time-to-qualify measure.
	//
	// There is no path back from qualified to contacted either. Nothing
	// is lost by leaving a stalled lead qualified, and an edge that
	// exists only to let someone undo a misclick is an edge a report
	// then has to reason about.
	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"Lead", "lead_status", "Lead Status",
		[]statusgraph.Spec{
			{Code: "new", Name: "New", Sequence: 1, IsInitial: true},
			{Code: "contacted", Name: "Contacted", Sequence: 2},
			{Code: "qualified", Name: "Qualified", Sequence: 3},
			{Code: "converted", Name: "Converted", Sequence: 4, IsTerminal: true},
			{Code: "disqualified", Name: "Disqualified", Sequence: 5, IsTerminal: true},
		},
		[][2]string{
			{"new", "contacted"},
			{"contacted", "qualified"},
			{"qualified", "converted"},
			// Reachable from every live state: a lead can turn out to be
			// a dead end at any point, including before anyone calls.
			{"new", "disqualified"},
			{"contacted", "disqualified"},
			{"qualified", "disqualified"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed lead_status: %w", err)
	}

	// opportunity_stage is the pipeline. The forward path is the
	// familiar funnel; the edges that matter are the two backward ones
	// and the breadth of `lost`.
	//
	// negotiation -> proposal is real and common: the customer asks for
	// a revised quote, which puts the deal back in proposal rather than
	// killing it. proposal -> qualification covers the requirements
	// changing under you. Without those, a rep's only honest options
	// are to leave the stage wrong or mark the deal lost and open a
	// second one — and a pipeline where stage regression is impossible
	// is a pipeline that reports optimistically by construction.
	//
	// won is reachable from proposal as well as negotiation, because a
	// customer accepting a proposal outright is not an anomaly to be
	// modelled around. lost is reachable from every live stage,
	// including prospecting.
	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"Opportunity", "opportunity_stage", "Opportunity Stage",
		[]statusgraph.Spec{
			{Code: "prospecting", Name: "Prospecting", Sequence: 1, IsInitial: true},
			{Code: "qualification", Name: "Qualification", Sequence: 2},
			{Code: "proposal", Name: "Proposal", Sequence: 3},
			{Code: "negotiation", Name: "Negotiation", Sequence: 4},
			{Code: "won", Name: "Won", Sequence: 5, IsTerminal: true},
			{Code: "lost", Name: "Lost", Sequence: 6, IsTerminal: true},
		},
		[][2]string{
			{"prospecting", "qualification"},
			{"qualification", "proposal"},
			{"proposal", "negotiation"},
			{"negotiation", "won"},
			{"proposal", "won"},
			// The customer asks for a revised quote; the requirements move.
			{"negotiation", "proposal"},
			{"proposal", "qualification"},
			{"prospecting", "lost"},
			{"qualification", "lost"},
			{"proposal", "lost"},
			{"negotiation", "lost"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed opportunity_stage: %w", err)
	}

	// campaign_status is a lifecycle, and it is a real one rather than
	// something derivable from start_date/end_date: a campaign cancelled
	// before it ran and a campaign that ran to its end date are
	// different facts, and the dates cannot tell them apart. Modelled as
	// a graph rather than a plain enum for the same reason
	// SalesOrder.status_id is — this codebase has already paid for the
	// enum-to-graph migration once (PurchaseOrder).
	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"Campaign", "campaign_status", "Campaign Status",
		[]statusgraph.Spec{
			{Code: "planned", Name: "Planned", Sequence: 1, IsInitial: true},
			{Code: "active", Name: "Active", Sequence: 2},
			{Code: "completed", Name: "Completed", Sequence: 3, IsTerminal: true},
			{Code: "cancelled", Name: "Cancelled", Sequence: 4, IsTerminal: true},
		},
		[][2]string{
			{"planned", "active"},
			{"active", "completed"},
			{"planned", "cancelled"},
			{"active", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed campaign_status: %w", err)
	}
	return nil
}
