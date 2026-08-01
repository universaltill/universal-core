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
// Case's StatusTypeCode needs before the guarded engine will accept a
// create (the shared seeder is internal/kernel/statusgraph).
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
	return nil
}
