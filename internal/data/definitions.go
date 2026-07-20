package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// Definition status values, matching the entity_definitions/
// form_definitions/workflow_definitions CHECK constraints (003_definition_
// registry.sql, 001_init.sql). A definition never skips a state: draft ->
// approved -> published, and only a published version can be rolled_back.
const (
	StatusDraft      = "draft"
	StatusApproved   = "approved"
	StatusPublished  = "published"
	StatusRolledBack = "rolled_back"
)

// ErrInvalidStatusTransition is returned when a caller tries to approve,
// publish, or roll back a definition version that isn't currently in the
// state that transition requires (e.g. publishing a draft that was never
// approved) — the UPDATE's own WHERE clause enforces this atomically, so
// this error means "no row matched", not a read-then-write race.
var ErrInvalidStatusTransition = errors.New("data: definition is not in the required status for this transition")

// DefinitionVersion is one row of any of the three definition-registry
// tables — the registry's own bookkeeping (status, who authored it, when)
// plus the raw definition JSON. Deliberately []byte, not a typed
// entity.Definition/form.Definition/workflow.Definition: this package
// stays generic and never imports internal/kernel/{entity,form,workflow}
// (workflow already imports this package, so the reverse would cycle;
// keeping entity_definitions/form_definitions symmetric with that
// constraint rather than typed for two of three and raw for the third).
// Callers decode via the matching kernel package's Unmarshal function.
type DefinitionVersion struct {
	ID            string
	TenantID      string
	Key           string // entity_type for entity/form definitions, name for workflow definitions
	Version       int
	Status        string
	Definition    []byte
	CreatedByType string
	CreatedBy     string
	ApprovedBy    string // empty until Approve
}

// definitionRepo is the shared implementation behind
// EntityDefinitionRepo/FormDefinitionRepo/WorkflowDefinitionRepo — the
// three tables are identical in shape (only the table name and the
// entity_type-vs-name key column differ), so the SQL lives once here
// rather than three near-identical copies. table and keyColumn are
// always one of a fixed, hardcoded set of internal constants (never
// caller/user input), so building query text with them is safe — not the
// SQL-injection-shaped string interpolation this would be if either came
// from a request.
type definitionRepo struct {
	db        *sql.DB
	audit     *AuditRepo
	table     string // "entity_definitions" | "form_definitions" | "workflow_definitions"
	keyColumn string // "entity_type" | "name"
	// auditEntityType prefixes the audit_log entity_type column so a
	// definition mutation is never confused with a mutation of an actual
	// record of that entity type (e.g. "entity_definition:PurchaseOrder"
	// vs. plain "PurchaseOrder").
	auditEntityType func(key string) string
}

func newDefinitionRepo(db *sql.DB, table, keyColumn, auditPrefix string) *definitionRepo {
	return &definitionRepo{
		db:        db,
		audit:     NewAuditRepo(db),
		table:     table,
		keyColumn: keyColumn,
		auditEntityType: func(key string) string {
			return auditPrefix + ":" + key
		},
	}
}

// createDraft inserts a new draft version and its audit entry atomically
// — a definition version can never exist without an audit trail (same
// discipline as crud.Engine.Create for records).
func (r *definitionRepo) createDraft(ctx context.Context, tenantID, key string, version int, definition []byte, actor audit.Actor) (DefinitionVersion, error) {
	if err := actor.Validate(); err != nil {
		return DefinitionVersion{}, fmt.Errorf("create %s draft: %w", r.table, err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DefinitionVersion{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	var id string
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (tenant_id, %s, version, definition, created_by_type, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`, r.table, r.keyColumn),
		tenantID, key, version, definition, string(actor.Type), actor.ID,
	).Scan(&id)
	if err != nil {
		return DefinitionVersion{}, fmt.Errorf("insert %s: %w", r.table, err)
	}

	var diff map[string]any
	if err := json.Unmarshal(definition, &diff); err != nil {
		return DefinitionVersion{}, fmt.Errorf("unmarshal definition for audit diff: %w", err)
	}
	auditEntry, err := audit.New(tenantID, r.auditEntityType(key), id, audit.ActionCreate, actor, diff)
	if err != nil {
		return DefinitionVersion{}, fmt.Errorf("build audit entry: %w", err)
	}
	if err := r.audit.Insert(ctx, tx, auditEntry); err != nil {
		return DefinitionVersion{}, fmt.Errorf("write audit entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DefinitionVersion{}, fmt.Errorf("commit tx: %w", err)
	}
	return DefinitionVersion{
		ID: id, TenantID: tenantID, Key: key, Version: version, Status: StatusDraft,
		Definition: definition, CreatedByType: string(actor.Type), CreatedBy: actor.ID,
	}, nil
}

// transition moves a version from fromStatus to toStatus, guarded by the
// UPDATE's own WHERE clause (atomic check-and-set, not read-then-write),
// and writes the matching audit entry in the same transaction. approvedBy
// is set only on the draft->approved transition; every other transition
// passes it as nil (a no-op UPDATE `approved_by = COALESCE($x, approved_by)`
// would also work, but an explicit nil is clearer about which transitions
// actually touch that column).
func (r *definitionRepo) transition(ctx context.Context, tenantID, key string, version int, fromStatus, toStatus string, approvedBy string, actor audit.Actor, action audit.Action) error {
	if err := actor.Validate(); err != nil {
		return fmt.Errorf("transition %s: %w", r.table, err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var approvedByArg any
	setApprovedBy := ""
	if approvedBy != "" {
		approvedByArg = approvedBy
		setApprovedBy = ", approved_by = $6"
	}

	query := fmt.Sprintf(
		`UPDATE %s SET status = $1%s
		 WHERE tenant_id = $2 AND %s = $3 AND version = $4 AND status = $5`,
		r.table, setApprovedBy, r.keyColumn,
	)
	args := []any{toStatus, tenantID, key, version, fromStatus}
	if approvedByArg != nil {
		args = append(args, approvedByArg)
	}

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update %s status: %w", r.table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrInvalidStatusTransition
	}

	var id string
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND %s = $2 AND version = $3`, r.table, r.keyColumn),
		tenantID, key, version,
	).Scan(&id); err != nil {
		return fmt.Errorf("look up %s id for audit: %w", r.table, err)
	}

	auditEntry, err := audit.New(tenantID, r.auditEntityType(key), id, action, actor, map[string]any{"status": toStatus})
	if err != nil {
		return fmt.Errorf("build audit entry: %w", err)
	}
	if err := r.audit.Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}

	return tx.Commit()
}

// getPublished returns the highest-versioned published row — publishing a
// new version never has to touch older published rows (they simply stop
// being "current" once a higher version exists), and rolling back the
// current version naturally falls back to the next-highest published one.
func (r *definitionRepo) getPublished(ctx context.Context, tenantID, key string) (DefinitionVersion, error) {
	return r.getOne(ctx,
		fmt.Sprintf(
			`SELECT id, version, status, definition, created_by_type, created_by, approved_by
			 FROM %s
			 WHERE tenant_id = $1 AND %s = $2 AND status = 'published'
			 ORDER BY version DESC LIMIT 1`, r.table, r.keyColumn),
		tenantID, key,
	)
}

// listPublishedKeys returns every distinct key (entity_type, for
// EntityDefinitionRepo) with at least one published version for
// tenantID — what a landing page needs to say "here's what you can
// actually open", without hardcoding any entity type into the generic
// engine (CLAUDE.md's kernel/deterministic-core boundary rule). DISTINCT
// because a key can have more than one published row at once (an older
// version stays 'published' even after a newer one is; see
// getPublished's own doc comment on "current" meaning highest version,
// not "only" version) — this only needs the type names, not versions.
func (r *definitionRepo) listPublishedKeys(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT DISTINCT %s FROM %s WHERE tenant_id = $1 AND status = 'published' ORDER BY %s`,
			r.keyColumn, r.table, r.keyColumn),
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list published %s: %w", r.table, err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan %s: %w", r.table, err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (r *definitionRepo) getVersion(ctx context.Context, tenantID, key string, version int) (DefinitionVersion, error) {
	return r.getOne(ctx,
		fmt.Sprintf(
			`SELECT id, version, status, definition, created_by_type, created_by, approved_by
			 FROM %s
			 WHERE tenant_id = $1 AND %s = $2 AND version = $3`, r.table, r.keyColumn),
		tenantID, key, version,
	)
}

func (r *definitionRepo) getOne(ctx context.Context, query string, args ...any) (DefinitionVersion, error) {
	var v DefinitionVersion
	var approvedBy sql.NullString
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&v.ID, &v.Version, &v.Status, &v.Definition, &v.CreatedByType, &v.CreatedBy, &approvedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DefinitionVersion{}, ErrNotFound
	}
	if err != nil {
		return DefinitionVersion{}, fmt.Errorf("get %s: %w", r.table, err)
	}
	if approvedBy.Valid {
		v.ApprovedBy = approvedBy.String
	}
	return v, nil
}

// EntityDefinitionRepo is the repository for entity_definitions.
type EntityDefinitionRepo struct{ r *definitionRepo }

func NewEntityDefinitionRepo(db *sql.DB) *EntityDefinitionRepo {
	return &EntityDefinitionRepo{r: newDefinitionRepo(db, "entity_definitions", "entity_type", "entity_definition")}
}

func (e *EntityDefinitionRepo) CreateDraft(ctx context.Context, tenantID, entityType string, version int, definition []byte, actor audit.Actor) (DefinitionVersion, error) {
	return e.r.createDraft(ctx, tenantID, entityType, version, definition, actor)
}

func (e *EntityDefinitionRepo) Approve(ctx context.Context, tenantID, entityType string, version int, actor audit.Actor) error {
	return e.r.transition(ctx, tenantID, entityType, version, StatusDraft, StatusApproved, actor.ID, actor, audit.ActionUpdate)
}

func (e *EntityDefinitionRepo) Publish(ctx context.Context, tenantID, entityType string, version int, actor audit.Actor) error {
	return e.r.transition(ctx, tenantID, entityType, version, StatusApproved, StatusPublished, "", actor, audit.ActionUpdate)
}

func (e *EntityDefinitionRepo) Rollback(ctx context.Context, tenantID, entityType string, version int, actor audit.Actor) error {
	return e.r.transition(ctx, tenantID, entityType, version, StatusPublished, StatusRolledBack, "", actor, audit.ActionUpdate)
}

func (e *EntityDefinitionRepo) GetPublished(ctx context.Context, tenantID, entityType string) (DefinitionVersion, error) {
	return e.r.getPublished(ctx, tenantID, entityType)
}

func (e *EntityDefinitionRepo) GetVersion(ctx context.Context, tenantID, entityType string, version int) (DefinitionVersion, error) {
	return e.r.getVersion(ctx, tenantID, entityType, version)
}

// ListPublishedEntityTypes returns every entity type tenantID currently
// has at least one published Definition for — the landing page's data
// source (internal/api's dashboard handler), reading the registry
// instead of hardcoding a module list.
func (e *EntityDefinitionRepo) ListPublishedEntityTypes(ctx context.Context, tenantID string) ([]string, error) {
	return e.r.listPublishedKeys(ctx, tenantID)
}

// FormDefinitionRepo is the repository for form_definitions.
type FormDefinitionRepo struct{ r *definitionRepo }

func NewFormDefinitionRepo(db *sql.DB) *FormDefinitionRepo {
	return &FormDefinitionRepo{r: newDefinitionRepo(db, "form_definitions", "entity_type", "form_definition")}
}

func (f *FormDefinitionRepo) CreateDraft(ctx context.Context, tenantID, entityType string, version int, definition []byte, actor audit.Actor) (DefinitionVersion, error) {
	return f.r.createDraft(ctx, tenantID, entityType, version, definition, actor)
}

func (f *FormDefinitionRepo) Approve(ctx context.Context, tenantID, entityType string, version int, actor audit.Actor) error {
	return f.r.transition(ctx, tenantID, entityType, version, StatusDraft, StatusApproved, actor.ID, actor, audit.ActionUpdate)
}

func (f *FormDefinitionRepo) Publish(ctx context.Context, tenantID, entityType string, version int, actor audit.Actor) error {
	return f.r.transition(ctx, tenantID, entityType, version, StatusApproved, StatusPublished, "", actor, audit.ActionUpdate)
}

func (f *FormDefinitionRepo) Rollback(ctx context.Context, tenantID, entityType string, version int, actor audit.Actor) error {
	return f.r.transition(ctx, tenantID, entityType, version, StatusPublished, StatusRolledBack, "", actor, audit.ActionUpdate)
}

func (f *FormDefinitionRepo) GetPublished(ctx context.Context, tenantID, entityType string) (DefinitionVersion, error) {
	return f.r.getPublished(ctx, tenantID, entityType)
}

func (f *FormDefinitionRepo) GetVersion(ctx context.Context, tenantID, entityType string, version int) (DefinitionVersion, error) {
	return f.r.getVersion(ctx, tenantID, entityType, version)
}

// WorkflowDefinitionRepo is the repository for workflow_definitions.
// Keyed by name, not entity_type — a workflow.Definition's own identity
// is its Name (see internal/kernel/workflow.Definition).
type WorkflowDefinitionRepo struct{ r *definitionRepo }

func NewWorkflowDefinitionRepo(db *sql.DB) *WorkflowDefinitionRepo {
	return &WorkflowDefinitionRepo{r: newDefinitionRepo(db, "workflow_definitions", "name", "workflow_definition")}
}

func (w *WorkflowDefinitionRepo) CreateDraft(ctx context.Context, tenantID, name string, version int, definition []byte, actor audit.Actor) (DefinitionVersion, error) {
	return w.r.createDraft(ctx, tenantID, name, version, definition, actor)
}

func (w *WorkflowDefinitionRepo) Approve(ctx context.Context, tenantID, name string, version int, actor audit.Actor) error {
	return w.r.transition(ctx, tenantID, name, version, StatusDraft, StatusApproved, actor.ID, actor, audit.ActionUpdate)
}

func (w *WorkflowDefinitionRepo) Publish(ctx context.Context, tenantID, name string, version int, actor audit.Actor) error {
	return w.r.transition(ctx, tenantID, name, version, StatusApproved, StatusPublished, "", actor, audit.ActionUpdate)
}

func (w *WorkflowDefinitionRepo) Rollback(ctx context.Context, tenantID, name string, version int, actor audit.Actor) error {
	return w.r.transition(ctx, tenantID, name, version, StatusPublished, StatusRolledBack, "", actor, audit.ActionUpdate)
}

func (w *WorkflowDefinitionRepo) GetPublished(ctx context.Context, tenantID, name string) (DefinitionVersion, error) {
	return w.r.getPublished(ctx, tenantID, name)
}

func (w *WorkflowDefinitionRepo) GetVersion(ctx context.Context, tenantID, name string, version int) (DefinitionVersion, error) {
	return w.r.getVersion(ctx, tenantID, name, version)
}
