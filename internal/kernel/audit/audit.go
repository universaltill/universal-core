// Package audit implements AI-actor-aware audit logging (ADR-0017 §14,
// §16): every mutation is attributed to an actor, and when that actor is
// an AI agent, the model version and input hash are recorded alongside
// it — not folded into a generic "system" entry, and not retrofitted
// after the fact.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ActorType distinguishes a human from an AI agent as the author of a
// change. Never add a third bucket like "system" that hides which of the
// two actually made the change.
type ActorType string

const (
	ActorHuman ActorType = "human"
	ActorAgent ActorType = "ai_agent"
)

// Action is the kind of mutation being recorded.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// Actor identifies who or what made a change.
type Actor struct {
	Type ActorType
	// ID is the human user id, or the agent's stable identifier (e.g.
	// "universal-core-kernel-agent").
	ID string
	// ModelVersion is required when Type == ActorAgent (e.g.
	// "claude-fable-5"); empty for human actors.
	ModelVersion string
	// Input is the raw prompt/request that produced this change, hashed
	// (never stored raw) so a specific draft can later be correlated
	// without retaining potentially sensitive free text in the audit log.
	Input string
}

var (
	ErrMissingActorID      = errors.New("audit: actor id is required")
	ErrMissingModelVersion = errors.New("audit: ai_agent actor requires a model_version")
)

// Validate checks that an Actor is well-formed before it's allowed to
// author an audit entry.
func (a Actor) Validate() error {
	if a.ID == "" {
		return ErrMissingActorID
	}
	if a.Type == ActorAgent && a.ModelVersion == "" {
		return ErrMissingModelVersion
	}
	return nil
}

// InputHash returns the SHA-256 hash of the actor's input, or "" if there
// is no input to hash (e.g. a human actor with no captured prompt).
func (a Actor) InputHash() string {
	if a.Input == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(a.Input))
	return hex.EncodeToString(sum[:])
}

// Entry is one audit_log row, ready to be persisted by a repository. This
// package defines the shape and the invariant (Actor.Validate); the SQL
// lives in internal/data per the repository-pattern rule.
type Entry struct {
	TenantID   string
	EntityType string
	RecordID   string
	Action     Action
	Actor      Actor
	Diff       map[string]any
	CreatedAt  time.Time
}

// New builds an Entry, validating the actor. Every mutation path in the
// kernel must go through this — there is no direct-insert shortcut.
func New(tenantID, entityType, recordID string, action Action, actor Actor, diff map[string]any) (Entry, error) {
	if err := actor.Validate(); err != nil {
		return Entry{}, err
	}
	return Entry{
		TenantID:   tenantID,
		EntityType: entityType,
		RecordID:   recordID,
		Action:     action,
		Actor:      actor,
		Diff:       diff,
		CreatedAt:  time.Now().UTC(),
	}, nil
}
