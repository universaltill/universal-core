// Package authz enforces the Core-owned RBAC model (ADR-0005's
// Role/UserRole data + ADR-0006's Permission rules) on the generic CRUD
// paths. It is the enforcement mechanism ADR-0005 deliberately deferred:
// a per-request Resolver answers "may this user read/write this entity
// type," and a GuardedEngine wraps crud.Engine so every handler-facing
// CRUD call passes through that answer without any handler having to
// remember to ask.
//
// Semantics (ADR-0006, restated here because this package IS the
// implementation of record):
//
//   - Grants are additive: effective access is the union over the
//     user's Roles. There are no deny rows.
//   - A write grant implies read: can_write without can_read is not a
//     supported state (you cannot meaningfully edit what you cannot
//     read, and honoring it literally would let an update commit and
//     then 403 on its own read-back — a mutation the client is told
//     failed). resolve() folds can_write into canRead.
//   - RBAC is opt-in per entity type: zero Permission rows for an
//     entity type -> that type is not under RBAC and stays fully
//     accessible (every tenant provisioned before this package existed
//     keeps working unchanged). One or more rows -> deny-unless-granted.
//   - FIELD level (FieldPermission rows): a field is hidden from a user
//     only when EVERY role they hold hides it — the same union-of-
//     visibility reading additive grants imply, since a role with no
//     rule for a field implicitly sees it. A hidden field is stripped
//     from every read (API JSON, rendered form, list columns, CSV
//     export) and is not writable: a payload that tries to set it to
//     anything other than its stored value is denied, and one that
//     simply omits it has the stored value restored rather than wiped
//     (crud.Update is a full replacement, so silent restoration is what
//     keeps field hiding from becoming field deletion — the same
//     data-loss failure formrender.buildHiddenFields exists to prevent).
//     A user holding NO roles has no field rules applying to them and
//     sees everything the entity level lets them see; the entity-level
//     opt-in, not this, is what gates a roleless user out of a type.
//   - EXCEPT the RBAC control plane itself (Role, UserRole, Permission,
//     FieldPermission): once a tenant has configured RBAC at all (any
//     tenant_admin grant, or any Permission/FieldPermission row),
//     WRITES to these four types are deny-unless-granted even with
//     zero rows naming them — otherwise any tenant_member could author
//     themselves a tenant_admin grant and dissolve every rule in the
//     tenant. Reads keep the normal opt-in semantics, and a completely
//     unconfigured tenant (no admin, no rules — the bootstrap state,
//     equivalent to the pre-RBAC status quo) keeps its control plane
//     open so the first admin can be created at all. Explicit
//     Permission rows on these types still work, so an admin can
//     delegate role management to a non-admin role deliberately.
//   - The role CODE "tenant_admin" always has full access — the
//     lockout-prevention convention (a tenant admin who authors
//     Permission rows without granting themselves would otherwise lock
//     themselves out of the very screens needed to undo it). This keys
//     on Role.code (tenant data), not an entity type — the engine
//     stays generic.
//   - Machine actors (svcauth's service tokens) bypass entity/field
//     RBAC: they are already coarse-gated by Zitadel's
//     tenant_integration role, and per-integration fine-graining is
//     future work, not silently half-built here.
//
// This package sits beside crud, not inside it: crud.Engine stays the
// raw, identity-free mechanism system paths (workflow steps, seeding,
// provisioning) call directly — those are not user requests and must
// not be subject to a user's permissions.
package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// ErrDenied is the sentinel every RBAC refusal wraps — handlers map
// errors.Is(err, ErrDenied) to HTTP 403, the same errors.Is convention
// crud.ErrInvalidTransition already established for 400.
var ErrDenied = errors.New("access denied")

// AdminRoleCode is the Role.code that always resolves to full access
// (see the package comment's lockout-prevention rationale).
const AdminRoleCode = "tenant_admin"

// controlPlaneTypes are the entity types RBAC itself is made of —
// writes to them are what grant/revoke access, so they get the
// deny-unless-granted-once-configured posture the package comment
// describes instead of plain opt-in. This is authz protecting its own
// model, not business logic for an operational entity type — the
// generic engines (entity/form/workflow) still know nothing about them.
var controlPlaneTypes = map[string]bool{
	"Role":            true,
	"UserRole":        true,
	"Permission":      true,
	"FieldPermission": true,
}

// entityPerm is the memoized per-entity-type resolution result.
type entityPerm struct {
	rulesExist bool
	canRead    bool
	canWrite   bool
}

// permRule / fieldRule are one Permission / FieldPermission row reduced
// to the fields resolution actually uses, grouped by entity_type at load
// time (see loadRules).
type permRule struct {
	roleID   string
	canRead  bool
	canWrite bool
}

type fieldRule struct {
	roleID    string
	fieldName string
	hidden    bool
}

// Resolver answers permission questions for one authenticated actor
// against one tenant database, for the duration of one request. It is
// deliberately lazy: a request that never touches a guarded path never
// queries roles at all, and each entity type resolves at most once per
// request (the per-request memo R6's caching ladder starts from —
// in-process caching across requests is a later rung, not this).
//
// Not safe for concurrent use; each request builds its own (same
// lifecycle as tenantScope, which already follows that rule).
type Resolver struct {
	db      *sql.DB
	userID  string
	machine bool

	rolesLoaded bool
	admin       bool
	roleIDs     map[string]bool

	// rulesLoaded/perms/fields hold every policy row in the tenant,
	// grouped by entity_type, loaded at most once per request — see
	// loadRules on why this is one query per policy type rather than one
	// per entity type touched.
	rulesLoaded bool
	perms       map[string][]permRule
	fields      map[string][]fieldRule
	anyRules    bool

	configuredLoaded bool
	configured       bool

	memo       map[string]entityPerm
	hiddenMemo map[string]map[string]bool
}

// NewResolver builds a Resolver for one request. actor is the
// authenticated identity (audit.Actor.ID == the Zitadel sub, the same
// value UserRole.user_id carries — ADR-0005). machine marks svcauth
// service-token requests, which bypass RBAC (package comment).
func NewResolver(db *sql.DB, actor audit.Actor, machine bool) *Resolver {
	return &Resolver{
		db:         db,
		userID:     actor.ID,
		machine:    machine,
		memo:       make(map[string]entityPerm),
		hiddenMemo: make(map[string]map[string]bool),
	}
}

func (r *Resolver) loadRoles(ctx context.Context) error {
	if r.rolesLoaded {
		return nil
	}
	grants, err := foundation.RoleGrantsForUser(ctx, r.db, r.userID)
	if err != nil {
		return fmt.Errorf("resolve roles for %s: %w", r.userID, err)
	}
	r.roleIDs = make(map[string]bool, len(grants))
	for _, g := range grants {
		r.roleIDs[g.ID] = true
		if g.Code == AdminRoleCode {
			r.admin = true
		}
	}
	r.rolesLoaded = true
	return nil
}

// loadRules reads every Permission and FieldPermission row in the tenant
// once per request and groups them by entity_type.
//
// Two whole-table reads rather than a filtered read per entity type
// touched: policy rows are a handful per tenant (one per role/entity-type
// pair an admin deliberately authored), while the number of entity types
// a single request resolves is now unbounded — nav/menu filtering asks
// CanRead about EVERY published entity type on EVERY page. The
// per-entity-type query this replaced would have turned that into ~20
// round trips per page render; grouping in memory makes it two, and
// makes rbacConfigured's own row counts free.
func (r *Resolver) loadRules(ctx context.Context) error {
	if r.rulesLoaded {
		return nil
	}
	records := data.NewRecordRepo(r.db)

	permRows, err := records.List(ctx, "Permission")
	if err != nil {
		return fmt.Errorf("list Permission rules: %w", err)
	}
	r.perms = make(map[string][]permRule)
	for _, row := range permRows {
		et, _ := row.Data["entity_type"].(string)
		if et == "" {
			// An inert rule (ADR-0006's recorded limitation): entity_type
			// is a plain string, so a blank/typo'd one names no type. Kept
			// out of the grouping rather than silently applied to
			// something.
			continue
		}
		rid, _ := row.Data["role_id"].(string)
		canRead, _ := row.Data["can_read"].(bool)
		canWrite, _ := row.Data["can_write"].(bool)
		r.perms[et] = append(r.perms[et], permRule{roleID: rid, canRead: canRead, canWrite: canWrite})
	}

	fieldRows, err := records.List(ctx, "FieldPermission")
	if err != nil {
		return fmt.Errorf("list FieldPermission rules: %w", err)
	}
	r.fields = make(map[string][]fieldRule)
	for _, row := range fieldRows {
		et, _ := row.Data["entity_type"].(string)
		if et == "" {
			continue
		}
		rid, _ := row.Data["role_id"].(string)
		name, _ := row.Data["field_name"].(string)
		hidden, _ := row.Data["hidden"].(bool)
		r.fields[et] = append(r.fields[et], fieldRule{roleID: rid, fieldName: name, hidden: hidden})
	}

	// Counted before the entity_type filtering above, deliberately: a
	// typo'd rule is inert for access decisions but still proves the
	// tenant has started configuring RBAC, which is what closes the
	// control plane's bootstrap window (rbacConfigured).
	r.anyRules = len(permRows) > 0 || len(fieldRows) > 0
	r.rulesLoaded = true
	return nil
}

func (r *Resolver) resolve(ctx context.Context, entityType string) (entityPerm, error) {
	if p, ok := r.memo[entityType]; ok {
		return p, nil
	}
	if err := r.loadRoles(ctx); err != nil {
		return entityPerm{}, err
	}
	if err := r.loadRules(ctx); err != nil {
		return entityPerm{}, err
	}
	rows := r.perms[entityType]
	p := entityPerm{rulesExist: len(rows) > 0}
	for _, row := range rows {
		if row.roleID == "" || !r.roleIDs[row.roleID] {
			continue
		}
		if row.canRead {
			p.canRead = true
		}
		if row.canWrite {
			p.canWrite = true
			// A write grant implies read (package comment) — a role
			// that may change a record may also see it.
			p.canRead = true
		}
	}
	r.memo[entityType] = p
	return p, nil
}

// HiddenFields returns the set of entityType field names this actor may
// neither see nor write, resolved as the union of what their roles can
// see: a field is hidden only when EVERY role they hold has a
// hidden=true FieldPermission row for it (package comment). Returns nil
// — not an empty map — for the overwhelmingly common "nothing hidden"
// case, so callers can skip their filtering work on a plain len() check.
//
// An actor holding no roles at all has no field rules applying to them:
// "every role hides it" is not satisfiable over an empty set, and
// treating it as vacuously true would hide every ruled field from every
// roleless user, including on entity types their tenant never opted into
// RBAC. Gating a roleless user out of a type is the entity level's job.
func (r *Resolver) HiddenFields(ctx context.Context, entityType string) (map[string]bool, error) {
	if r.machine {
		return nil, nil
	}
	if err := r.loadRoles(ctx); err != nil {
		return nil, err
	}
	if r.admin || len(r.roleIDs) == 0 {
		return nil, nil
	}
	if h, ok := r.hiddenMemo[entityType]; ok {
		return h, nil
	}
	if err := r.loadRules(ctx); err != nil {
		return nil, err
	}

	// hidingRoles[field] is the set of roles THIS actor holds that hide
	// field — counted per role rather than per row so two duplicate rows
	// for the same role can't stand in for a second role's agreement.
	hidingRoles := make(map[string]map[string]bool)
	for _, fr := range r.fields[entityType] {
		if !fr.hidden || fr.fieldName == "" || fr.roleID == "" || !r.roleIDs[fr.roleID] {
			continue
		}
		set, ok := hidingRoles[fr.fieldName]
		if !ok {
			set = make(map[string]bool)
			hidingRoles[fr.fieldName] = set
		}
		set[fr.roleID] = true
	}

	var hidden map[string]bool
	for name, set := range hidingRoles {
		if len(set) != len(r.roleIDs) {
			continue // at least one held role still sees it
		}
		if hidden == nil {
			hidden = make(map[string]bool, len(hidingRoles))
		}
		hidden[name] = true
	}
	r.hiddenMemo[entityType] = hidden
	return hidden, nil
}

// rbacConfigured reports whether this tenant has set up RBAC at all:
// any Permission/FieldPermission row, or any user actually granted the
// tenant_admin role. Memoized per request; only consulted for writes
// to control-plane types, so an ordinary request never pays for it.
func (r *Resolver) rbacConfigured(ctx context.Context) (bool, error) {
	if r.configuredLoaded {
		return r.configured, nil
	}
	if err := r.loadRules(ctx); err != nil {
		return false, err
	}
	if r.anyRules {
		r.configured, r.configuredLoaded = true, true
		return true, nil
	}
	records := data.NewRecordRepo(r.db)
	adminRoles, err := records.ListByField(ctx, "Role", "code", AdminRoleCode)
	if err != nil {
		return false, fmt.Errorf("list %s roles: %w", AdminRoleCode, err)
	}
	for _, role := range adminRoles {
		grants, err := records.ListByField(ctx, "UserRole", "role_id", role.ID)
		if err != nil {
			return false, fmt.Errorf("list UserRole for %s: %w", AdminRoleCode, err)
		}
		if len(grants) > 0 {
			r.configured, r.configuredLoaded = true, true
			return true, nil
		}
	}
	r.configuredLoaded = true
	return false, nil
}

// CanRead reports whether the actor may read records of entityType.
func (r *Resolver) CanRead(ctx context.Context, entityType string) (bool, error) {
	if r.machine {
		return true, nil
	}
	if err := r.loadRoles(ctx); err != nil {
		return false, err
	}
	if r.admin {
		return true, nil
	}
	p, err := r.resolve(ctx, entityType)
	if err != nil {
		return false, err
	}
	if !p.rulesExist {
		return true, nil
	}
	return p.canRead, nil
}

// CanWrite reports whether the actor may create/update/delete records
// of entityType.
func (r *Resolver) CanWrite(ctx context.Context, entityType string) (bool, error) {
	if r.machine {
		return true, nil
	}
	if err := r.loadRoles(ctx); err != nil {
		return false, err
	}
	if r.admin {
		return true, nil
	}
	p, err := r.resolve(ctx, entityType)
	if err != nil {
		return false, err
	}
	if !p.rulesExist {
		if controlPlaneTypes[entityType] {
			// No explicit rules for this control-plane type: open only
			// while the tenant hasn't configured RBAC at all (the
			// bootstrap window — see the package comment).
			configured, err := r.rbacConfigured(ctx)
			if err != nil {
				return false, err
			}
			return !configured, nil
		}
		return true, nil
	}
	return p.canWrite, nil
}

// GuardedEngine wraps crud.Engine with the Resolver's answers: reads
// require CanRead, mutations require CanWrite, everything else is
// delegated untouched. tenantScope holds one of these instead of the
// raw engine, so every handler CRUD call is guarded structurally — a
// new handler cannot forget a check it never had to write.
type GuardedEngine struct {
	raw *crud.Engine
	res *Resolver
}

// Guard wraps raw with res.
func Guard(raw *crud.Engine, res *Resolver) *GuardedEngine {
	return &GuardedEngine{raw: raw, res: res}
}

func (g *GuardedEngine) checkRead(ctx context.Context, def *entity.Definition) error {
	ok, err := g.res.CanRead(ctx, def.EntityType)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no read permission on %s", ErrDenied, def.EntityType)
	}
	return nil
}

func (g *GuardedEngine) checkWrite(ctx context.Context, def *entity.Definition) error {
	ok, err := g.res.CanWrite(ctx, def.EntityType)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no write permission on %s", ErrDenied, def.EntityType)
	}
	return nil
}

// CanRead / CanWrite / HiddenFields expose the underlying Resolver's
// answers to callers that need them for something other than a CRUD call
// — nav/menu filtering (which entity types to link to at all) and the
// presentation layers that render a record's fields themselves (list
// columns, CSV export columns, generated form fields). They exist here
// rather than handing internal/api a second reference to the Resolver so
// there is exactly one object a request handler asks about permissions,
// and no way to end up asking a DIFFERENT resolver than the one guarding
// the engine it is about to call.
func (g *GuardedEngine) CanRead(ctx context.Context, entityType string) (bool, error) {
	return g.res.CanRead(ctx, entityType)
}

func (g *GuardedEngine) CanWrite(ctx context.Context, entityType string) (bool, error) {
	return g.res.CanWrite(ctx, entityType)
}

func (g *GuardedEngine) HiddenFields(ctx context.Context, entityType string) (map[string]bool, error) {
	return g.res.HiddenFields(ctx, entityType)
}

// redact removes every field this actor may not see from recs, in place.
// data.RecordRepo builds a fresh map per row on every read (each is
// json.Unmarshal'd out of the records table), so there is no shared or
// cached map for this to corrupt.
func (g *GuardedEngine) redact(ctx context.Context, def *entity.Definition, recs []data.Record) error {
	hidden, err := g.res.HiddenFields(ctx, def.EntityType)
	if err != nil {
		return err
	}
	if len(hidden) == 0 {
		return nil
	}
	for _, rec := range recs {
		for name := range hidden {
			delete(rec.Data, name)
		}
	}
	return nil
}

// EffectiveWriteFields returns the field map a write of def/id should
// actually apply, given what this actor may see. For every hidden field:
// a submitted value that differs from what is already stored is denied
// (a user cannot write a field they cannot read), and an omitted one is
// restored from the stored record rather than left absent.
//
// The restoration is the important half. data.RecordRepo.UpdateTx
// replaces a record's whole data blob rather than patching it, so a form
// that legitimately omits a hidden field would otherwise ERASE it on
// every save — turning "hide this field from the sales clerk" into "let
// the sales clerk delete this field," which is a worse outcome than not
// hiding it at all. (Same failure mode, same fix, as
// formrender.buildHiddenFields' own off-form-field preservation.)
//
// Idempotent by construction: re-running it on its own output restores
// the same stored values and finds nothing that differs. That is what
// lets internal/api call it BEFORE its pre-write entity.ValidateRecord
// (so a hidden required field is present for validation and a denied
// user still gets 403 rather than a misleading 400 about a field they
// cannot see) while Create/Update below still call it themselves — the
// guarantee that a handler cannot forget an authorization step, which is
// the whole reason this engine exists, does not depend on that handler
// courtesy.
//
// id is "" for a create, where there is no stored record: any submitted
// value for a hidden field is a write of an unreadable field and is
// denied outright.
//
// Checks entity-level write permission FIRST, before resolving field
// rules or reading anything. Independent review caught the ordering:
// without this, a user with NO access to the entity type at all — who
// merely happens to hold some role carrying a FieldPermission rule for
// it — reached the g.raw.Get below (an unauthorized read of the real
// record) and could tell an existing id from a missing one by whether
// they got 403 or 404. An authorization function must not be the thing
// that leaks; a caller that runs this before its own checks has to get
// the refusal here, not after.
func (g *GuardedEngine) EffectiveWriteFields(ctx context.Context, def *entity.Definition, id string, fields map[string]any) (map[string]any, error) {
	if err := g.checkWrite(ctx, def); err != nil {
		return nil, err
	}
	hidden, err := g.res.HiddenFields(ctx, def.EntityType)
	if err != nil {
		return nil, err
	}
	if len(hidden) == 0 {
		return fields, nil
	}

	var stored map[string]any
	if id != "" {
		// Deliberately g.raw, not g.Get: this needs the record as actually
		// stored, including the very fields the caller may not see.
		rec, err := g.raw.Get(ctx, def, id)
		if err != nil {
			return nil, err
		}
		stored = rec.Data
	}

	out := make(map[string]any, len(fields))
	maps.Copy(out, fields)
	for name := range hidden {
		submitted, present := out[name]
		current := stored[name]
		if present && !sameFieldValue(submitted, current) {
			return nil, fmt.Errorf("%w: no permission to write field %s.%s", ErrDenied, def.EntityType, name)
		}
		if current == nil {
			// Never stored, or a create — leave it unset rather than
			// writing an explicit nil the record would then carry.
			delete(out, name)
			continue
		}
		out[name] = current
	}
	return out, nil
}

// sameFieldValue reports whether a submitted field value is the same as
// the stored one, for the purpose of deciding "this isn't a write."
//
// Numbers are compared as float64 regardless of their Go type. Stored
// values always come back from json.Unmarshal as float64, and every
// value that reaches here over HTTP does too (encoding/json for a JSON
// body, csvimport.Coerce for a form or CSV cell) — but a Go caller
// writing map[string]any{"minor_unit": 2} produces an int, and a
// DeepEqual against the stored float64(2) would refuse an update that
// changes nothing. Over-denial is the safe direction, but a permission
// check that rejects a genuine no-op is still a bug, and independent
// review proved this one reachable from any in-process caller.
//
// Everything else falls back to reflect.DeepEqual: it never panics on
// the uncomparable types a stored JSON blob could in principle hold
// (slices, maps), where == would.
func sameFieldValue(submitted, stored any) bool {
	if a, ok := numericValue(submitted); ok {
		if b, ok := numericValue(stored); ok {
			return a == b
		}
		return false
	}
	return reflect.DeepEqual(submitted, stored)
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func (g *GuardedEngine) Create(ctx context.Context, def *entity.Definition, fields map[string]any, actor audit.Actor) (data.Record, error) {
	if err := g.checkWrite(ctx, def); err != nil {
		return data.Record{}, err
	}
	effective, err := g.EffectiveWriteFields(ctx, def, "", fields)
	if err != nil {
		return data.Record{}, err
	}
	rec, err := g.raw.Create(ctx, def, effective, actor)
	if err != nil {
		return data.Record{}, err
	}
	// The created record is echoed straight back to the caller (API JSON,
	// re-rendered form), so it goes through the same redaction a read of
	// it would — a create must not become a way to see a hidden field's
	// server-side default.
	if err := g.redact(ctx, def, []data.Record{rec}); err != nil {
		return data.Record{}, err
	}
	return rec, nil
}

func (g *GuardedEngine) Update(ctx context.Context, def *entity.Definition, id string, fields map[string]any, expectedVersion *int, actor audit.Actor) (int, error) {
	if err := g.checkWrite(ctx, def); err != nil {
		return 0, err
	}
	effective, err := g.EffectiveWriteFields(ctx, def, id, fields)
	if err != nil {
		return 0, err
	}
	return g.raw.Update(ctx, def, id, effective, expectedVersion, actor)
}

func (g *GuardedEngine) Delete(ctx context.Context, def *entity.Definition, id string, actor audit.Actor) error {
	if err := g.checkWrite(ctx, def); err != nil {
		return err
	}
	return g.raw.Delete(ctx, def, id, actor)
}

// SystemFieldForRouting reads one field off a record WITHOUT this actor's
// field/entity permissions applied — the same unguarded g.raw path
// EffectiveWriteFields already uses, and for the same reason: this is the
// kernel making a decision (here, which department an approval routes to),
// not the user reading data. Routing must not depend on whether the
// approver happens to be allowed to see the routing field: a
// FieldPermission hiding department_id from their role would otherwise
// wrongly deny a legitimate approver, and no entity-read grant would 500
// them (both found by independent review of R17 department routing).
//
// Deliberately returns only the ONE named field's string value, never the
// whole record, so this cannot become a general RBAC bypass — a caller
// gets exactly the routing key it asked for and nothing else. Absent or
// non-string yields "".
func (g *GuardedEngine) SystemFieldForRouting(ctx context.Context, def *entity.Definition, id, field string) (string, error) {
	rec, err := g.raw.Get(ctx, def, id)
	if err != nil {
		return "", err
	}
	v, _ := rec.Data[field].(string)
	return v, nil
}

func (g *GuardedEngine) Get(ctx context.Context, def *entity.Definition, id string) (data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return data.Record{}, err
	}
	rec, err := g.raw.Get(ctx, def, id)
	if err != nil {
		return data.Record{}, err
	}
	if err := g.redact(ctx, def, []data.Record{rec}); err != nil {
		return data.Record{}, err
	}
	return rec, nil
}

func (g *GuardedEngine) List(ctx context.Context, def *entity.Definition) ([]data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return nil, err
	}
	recs, err := g.raw.List(ctx, def)
	if err != nil {
		return nil, err
	}
	if err := g.redact(ctx, def, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func (g *GuardedEngine) Count(ctx context.Context, def *entity.Definition) (int, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return 0, err
	}
	return g.raw.Count(ctx, def)
}

func (g *GuardedEngine) ListPage(ctx context.Context, def *entity.Definition, limit, offset int) ([]data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return nil, err
	}
	recs, err := g.raw.ListPage(ctx, def, limit, offset)
	if err != nil {
		return nil, err
	}
	if err := g.redact(ctx, def, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// ListByField filters on fieldName server-side, so a hidden field is
// still usable as a FILTER here even though its value is redacted from
// the rows returned. That is deliberate and load-bearing: the only
// caller is the master-detail child fetch, which filters on the child's
// parent-link field — hiding that field from a role would otherwise make
// the parent's whole lines table render empty rather than merely
// value-redacted. Matching on a value the caller already supplied
// discloses nothing they did not already have.
func (g *GuardedEngine) ListByField(ctx context.Context, def *entity.Definition, fieldName, value string) ([]data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return nil, err
	}
	recs, err := g.raw.ListByField(ctx, def, fieldName, value)
	if err != nil {
		return nil, err
	}
	if err := g.redact(ctx, def, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// ValidateStatusTransition is validation, not data access — it runs
// before a write the guarded Create/Update will still check, so it
// delegates without its own gate (a denied writer gets their 403 from
// the write itself; validating first leaks nothing).
func (g *GuardedEngine) ValidateStatusTransition(ctx context.Context, def *entity.Definition, id string, fields map[string]any, isCreate bool, expectedVersion *int) error {
	return g.raw.ValidateStatusTransition(ctx, def, id, fields, isCreate, expectedVersion)
}
