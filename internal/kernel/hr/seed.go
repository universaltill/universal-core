package hr

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

// Publish brings a tenant's HR module online — same mechanism as
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
			return fmt.Errorf("hr definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's HR Form Definitions online —
// separate from Publish for the same reason every other module keeps
// them separate (a form is a presentation choice, not the entity
// guarantee).
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("hr form %s is invalid: %w", f.EntityType, err)
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
// Employee's and LeaveRequest's StatusTypeCodes need before the guarded
// engine will accept a create (the shared seeder is
// internal/kernel/statusgraph).
//
// employee_status is a LIFECYCLE, not an approval chain: probation ->
// active is the normal path, and both can go to on_leave and back
// (extended leave is not a termination). terminated is terminal, and
// deliberately reachable from every live state — someone can leave
// during probation, while active, or while on leave. Nothing returns
// from terminated: ADR-0013 makes a rehire a NEW Employee record
// against the same Party, so that the first employment's dates and
// history survive intact.
//
// leave_request_status IS an approval chain, and is the intended
// subject of the R9 approval workflow (department-scoped routing,
// delegation and escalation, all built in Phase 2 and all generic over
// entity type — nothing extra is needed here to use them). draft ->
// submitted -> approved is the happy path; rejected and cancelled are
// the other two ends. cancelled is reachable from approved as well as
// from the earlier states, because plans change after approval and
// withdrawing booked leave is an ordinary thing to do — unlike an
// approved purchase order, no money has moved.
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
		"Employee", "employee_status", "Employee Status",
		[]statusgraph.Spec{
			{Code: "probation", Name: "Probation", Sequence: 1, IsInitial: true},
			{Code: "active", Name: "Active", Sequence: 2},
			{Code: "on_leave", Name: "On Leave", Sequence: 3},
			{Code: "terminated", Name: "Terminated", Sequence: 4, IsTerminal: true},
		},
		[][2]string{
			{"probation", "active"},
			{"active", "on_leave"},
			{"on_leave", "active"},
			// Leaving can happen from any live state.
			{"probation", "terminated"},
			{"active", "terminated"},
			{"on_leave", "terminated"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed employee_status: %w", err)
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"LeaveRequest", "leave_request_status", "Leave Request Status",
		[]statusgraph.Spec{
			{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true},
			{Code: "submitted", Name: "Submitted", Sequence: 2},
			{Code: "approved", Name: "Approved", Sequence: 3, IsTerminal: true},
			{Code: "rejected", Name: "Rejected", Sequence: 4, IsTerminal: true},
			{Code: "cancelled", Name: "Cancelled", Sequence: 5, IsTerminal: true},
		},
		[][2]string{
			{"draft", "submitted"},
			{"submitted", "approved"},
			{"submitted", "rejected"},
			{"draft", "cancelled"},
			{"submitted", "cancelled"},
			// Withdrawing approved leave: plans change, and no money has
			// moved (unlike an approved purchase order).
			{"approved", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed leave_request_status: %w", err)
	}
	return nil
}
