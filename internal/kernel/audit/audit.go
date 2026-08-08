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
	"strconv"
	"strings"
	"time"
)

// ActorType records who authored a change.
//
// The original rule here was "never add a third bucket like system that
// hides which of the two actually made the change". That rule is NARROWED
// by ADR-0008, not discarded, and the danger it named is still real: a
// `system` bucket becomes a laundry chute where a human approval or an
// AI-drafted change gets recorded as "the system did it", destroying
// exactly the accountability ADR-0001 §14 exists to preserve.
//
// The test for ActorSystem is therefore: did a person or a model cause
// this change, however indirectly? If yes it is human or ai_agent, never
// system. Publishing a scheduled workflow is a human act; only the
// clock-driven firing is system.
type ActorType string

const (
	ActorHuman ActorType = "human"
	ActorAgent ActorType = "ai_agent"
	// ActorSystem is the kernel acting on its own schedule — a scheduled
	// workflow run (R18), not a person and not a model. Introduced rather
	// than reusing one of the other two: `human` would invent someone who
	// did not act and `ai_agent` would claim a model was involved, and an
	// audit trail that misattributes an automated 3am run is worse than a
	// third enum value. See migrations/tenant/0003_system_actor.sql.
	ActorSystem ActorType = "system"
)

// Action is the kind of mutation being recorded.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	// ActionExport records a bulk disclosure rather than a mutation —
	// a statutory audit-file export (SAF-T) is a deliberate handover of
	// a tenant's whole ledger, which is exactly the kind of act ADR-0001
	// §14's actor accountability exists for even though no row changed.
	// Added with migrations/tenant/0004_audit_action_export.sql (the
	// audit_log CHECK constraint enumerates actions).
	ActionExport Action = "export"
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
	ErrMissingInput        = errors.New("audit: ai_agent actor requires an input")
)

// Validate checks that an Actor is well-formed before it's allowed to
// author an audit entry.
//
// Every ActorAgent must carry a non-empty Input (uc-infra#161): ADR-0001
// §14 names input_hash a required part of an ai_agent audit row
// alongside model_version, and until uc-infra#161 this was only true for
// the 7 CLI binaries uc-infra#124 fixed — internal/kernel/entity
// (Definition drafting), internal/kernel/csvimport, and
// internal/kernel/sqlsource could still construct a valid ActorAgent
// actor with no Input, so this check stayed deliberately loose rather
// than break them. uc-infra#161's audit found every real (non-test)
// ActorAgent construction site already populates Input: the 7 CLI
// binaries via CLIInvocationInput (see below); internal/kernel/entity's
// only production ActorAgent callers are those same CLI binaries
// (install-module, sync-tenant-modules, provision-tenant) publishing
// through internal/kernel/moduleseed; and internal/kernel/csvimport and
// internal/kernel/sqlsource have no production ActorAgent caller at
// all — their only production callers (internal/api's import/
// extsqlimport handlers) always pass the request's rc.Actor, which
// every httpx.RequestContext construction site sets to ActorHuman, never
// ActorAgent. So enforcing this here closes the gap without changing
// any production caller.
func (a Actor) Validate() error {
	if a.ID == "" {
		return ErrMissingActorID
	}
	if a.Type == ActorAgent && a.ModelVersion == "" {
		return ErrMissingModelVersion
	}
	if a.Type == ActorAgent && a.Input == "" {
		return ErrMissingInput
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

// CLIInvocationInput returns a deterministic string representing a CLI
// invocation, for use as Actor.Input when the actor is an ai_agent CLI
// caller with no natural-language prompt of its own — a
// provision-tenant/seed-demo-data/backfill-* run doesn't draft anything,
// but the resolved argument list it was invoked with is the closest
// analogue: it's the actual input that produced this run's changes, and
// the same invocation always hashes the same way. Callers pass the
// program's arguments (e.g. os.Args[1:] — excluding the program name
// itself, which varies by how the binary was invoked and carries no
// information about what it did).
//
// Each argument is quoted (%q) before joining, not just space-separated
// — a plain strings.Join would hash ["-name", "a b"] and
// ["-name", "a", "b"] identically (independent review, uc-infra#124),
// which would silently collapse two different invocations onto the same
// input_hash.
func CLIInvocationInput(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	return strings.Join(quoted, " ")
}

// Entry is one audit_log row, ready to be persisted by a repository. This
// package defines the shape and the invariant (Actor.Validate); the SQL
// lives in internal/data per the repository-pattern rule.
type Entry struct {
	EntityType string
	RecordID   string
	Action     Action
	Actor      Actor
	Diff       map[string]any
	CreatedAt  time.Time
}

// New builds an Entry, validating the actor. Every mutation path in the
// kernel must go through this — there is no direct-insert shortcut.
func New(entityType, recordID string, action Action, actor Actor, diff map[string]any) (Entry, error) {
	if err := actor.Validate(); err != nil {
		return Entry{}, err
	}
	return Entry{
		EntityType: entityType,
		RecordID:   recordID,
		Action:     action,
		Actor:      actor,
		Diff:       diff,
		CreatedAt:  time.Now().UTC(),
	}, nil
}
