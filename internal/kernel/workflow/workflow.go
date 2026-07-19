// Package workflow implements the declarative workflow definitions from
// ADR-0001 §9: trigger + steps, executed in order. Definitions and the
// synchronous in-memory Execute were the first increment; Queue (queue.go)
// is the durable, transactional Postgres job queue (retries, dead-letter,
// resumable) built on top of the same Definition/Step types — see queue.go's
// doc comment for how the two executors relate.
package workflow

import (
	"encoding/json"
	"fmt"
)

// TriggerType is when a workflow fires.
type TriggerType string

const (
	TriggerOnCreate TriggerType = "on_create"
	TriggerOnUpdate TriggerType = "on_update"
	TriggerManual   TriggerType = "manual"
)

// Trigger declares what starts a workflow run.
type Trigger struct {
	Type       TriggerType `json:"type"`
	EntityType string      `json:"entity_type,omitempty"` // required for on_create/on_update
}

// StepKind is the closed set of actions a workflow step may perform —
// same guardrail as form.ActionOp (ADR-0017 §6/§9): declarative verbs
// only, real logic lives in the kernel code behind each kind, never in
// the step's own metadata.
type StepKind string

const (
	StepRequireApproval StepKind = "require_approval"
	StepNotify          StepKind = "notify"
)

// Step is one action in a workflow.
type Step struct {
	Kind   StepKind       `json:"kind"`
	Params map[string]any `json:"params,omitempty"`
}

// Definition is one version of a workflow.
type Definition struct {
	Name    string  `json:"name"`
	Version int     `json:"version"`
	Trigger Trigger `json:"trigger"`
	Steps   []Step  `json:"steps"`
}

func (d *Definition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("workflow name is required")
	}
	switch d.Trigger.Type {
	case TriggerOnCreate, TriggerOnUpdate:
		if d.Trigger.EntityType == "" {
			return fmt.Errorf("trigger %q requires an entity_type", d.Trigger.Type)
		}
	case TriggerManual:
		// no extra requirement
	default:
		return fmt.Errorf("unknown trigger type %q", d.Trigger.Type)
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("workflow %q has no steps", d.Name)
	}
	for i, s := range d.Steps {
		switch s.Kind {
		case StepRequireApproval, StepNotify:
			// no extra requirement in this first increment
		default:
			return fmt.Errorf("step %d: unknown kind %q — steps stay a closed declarative set", i, s.Kind)
		}
	}
	return nil
}

// Unmarshal decodes raw (the workflow_definitions.definition JSONB
// column, read as plain []byte by internal/data — that package stays
// generic and never imports this one; also, this package's own queue.go
// already imports internal/data, so the reverse import would cycle) into
// a Definition and validates it before returning, same discipline as
// entity.Unmarshal/form.Unmarshal.
func Unmarshal(raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("unmarshal workflow definition: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("invalid workflow definition: %w", err)
	}
	return &d, nil
}

// StepResult records the outcome of one executed step.
type StepResult struct {
	Kind   StepKind
	Status string // "pending" (require_approval) or "done" (notify)
}

// Execute runs a Definition's steps in order, synchronously, against no
// external side effects yet — a require_approval step halts the run at
// "pending" (a human must act before anything transacts, per the
// Observe->Assist->Transact->Own gate this whole platform is built on),
// and a notify step completes immediately. This is intentionally the
// simplest possible executor; the durable, retryable, Postgres-backed
// version is future work.
func Execute(def *Definition) ([]StepResult, error) {
	if err := def.Validate(); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}
	var results []StepResult
	for _, s := range def.Steps {
		switch s.Kind {
		case StepRequireApproval:
			results = append(results, StepResult{Kind: s.Kind, Status: "pending"})
			// A pending approval halts the run here — no further step
			// executes until a human approves (not modeled yet in this
			// synchronous spike executor).
			return results, nil
		case StepNotify:
			results = append(results, StepResult{Kind: s.Kind, Status: "done"})
		}
	}
	return results, nil
}
