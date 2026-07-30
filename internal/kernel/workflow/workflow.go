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
	// TriggerScheduled fires on a cron expression instead of a record
	// event (R18). The expression lives in Trigger.Cron and is validated
	// at publish time, not at fire time — a bad schedule must be a
	// rejected Definition, not a worker that discovers the problem at 3am
	// and either spins or silently never runs.
	TriggerScheduled TriggerType = "scheduled"
)

// Trigger declares what starts a workflow run.
type Trigger struct {
	Type       TriggerType `json:"type"`
	EntityType string      `json:"entity_type,omitempty"` // required for on_create/on_update
	// Cron is a 5-field UTC expression, required for and only meaningful
	// to TriggerScheduled — see cron.go, including why it is a strict
	// subset of cron and why it is UTC rather than tenant-local.
	Cron string `json:"cron,omitempty"`
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
		if d.Trigger.Cron != "" {
			return fmt.Errorf("trigger %q must not set a cron expression (it fires on a record event, not a schedule)", d.Trigger.Type)
		}
	case TriggerManual:
		if d.Trigger.Cron != "" {
			return fmt.Errorf("trigger %q must not set a cron expression", d.Trigger.Type)
		}
	case TriggerScheduled:
		if d.Trigger.Cron == "" {
			return fmt.Errorf("trigger %q requires a cron expression", d.Trigger.Type)
		}
		if d.Trigger.EntityType != "" {
			// A scheduled workflow fires on the clock, not on a record
			// event. A leftover entity_type — from a UI that changed the
			// trigger type without clearing the other field — would look
			// meaningful and be silently ignored.
			return fmt.Errorf("trigger %q must not set an entity_type (it fires on a schedule, not on a record event)", d.Trigger.Type)
		}
		// Parsed here so a malformed schedule is rejected at publish time.
		// The alternative — validating when the worker first evaluates it
		// — means a typo ships, sits silently, and surfaces as a job that
		// never runs, which is the hardest kind of failure to notice.
		sched, err := ParseSchedule(d.Trigger.Cron)
		if err != nil {
			return fmt.Errorf("workflow %q: %w", d.Name, err)
		}
		// And proved to actually occur. "0 0 30 2 *" parses cleanly and
		// never happens; catching that here turns an invisible
		// never-fires into a refused publish.
		//
		// Searched from a FIXED epoch, not time.Now(): validation that
		// reads the clock is not reproducible, and independent review
		// proved an unchanged Definition could pass today and fail the
		// identical check decades later purely from the passage of time.
		if _, err := sched.Next(validationEpoch); err != nil {
			return fmt.Errorf("workflow %q: %w", d.Name, err)
		}
	default:
		return fmt.Errorf("unknown trigger type %q", d.Trigger.Type)
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("workflow %q has no steps", d.Name)
	}
	for i, s := range d.Steps {
		switch s.Kind {
		case StepRequireApproval:
			// role is optional (an unset role means "anyone may approve"
			// — see internal/api/workflow.go's approveWorkflowJob), but if
			// the key is present at all it must be a real, non-empty Role
			// code, not e.g. a number or an empty string: this param is
			// what internal/api's approval gate reads to decide who may
			// resume the job, so a malformed value here must fail the
			// definition at publish time rather than silently resolving
			// to "no restriction" the moment someone tries to approve it.
			if raw, ok := s.Params["role"]; ok {
				code, isString := raw.(string)
				if !isString || code == "" {
					return fmt.Errorf("step %d: require_approval's \"role\" param must be a non-empty string, got %#v", i, raw)
				}
			}
		case StepNotify:
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
