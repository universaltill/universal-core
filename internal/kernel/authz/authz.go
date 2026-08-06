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
//
// Delegation (uc-infra#8) joins this set for the same reason as the
// original four: it is itself an access-granting record (it can make a
// user approval-eligible for standing they don't hold directly, see
// foundation.ActiveDelegatorsFor), so an unprivileged user authoring one
// naming themselves as delegate would otherwise be able to grant
// themselves someone else's approval standing the moment any tenant
// configures RBAC at all — exactly the self-grant escalation this
// deny-unless-granted posture exists to close.
var controlPlaneTypes = map[string]bool{
	"Role":            true,
	"UserRole":        true,
	"Permission":      true,
	"FieldPermission": true,
	"Delegation":      true,
	// ExternalIdentity (uc-infra#101) joins for the same
	// redirection-of-authority reason: it decides which existing record
	// a keyed legacy-import row will silently UPDATE, so an
	// unprivileged user authoring one could re-point the next import at
	// any record of that entity type. The import engine itself writes
	// identities through a raw-engine side-channel (see
	// sqlsource.RecordEngine's doc comment), the same posture workflow
	// steps and provisioning use.
	"ExternalIdentity": true,
	// SystemOfRecord (uc-infra#102) is an authority-redirecting record
	// too — it is the config that decides whether an entity type's
	// imported records are user-writable at all, so left open it is
	// self-disarming: any user could flip a read_only row to
	// platform_owned, hand-edit the "protected" record, and flip it back
	// (proven with three HTTP requests in review). Same class as
	// Delegation/ExternalIdentity above. NOT systemOnlyWriteTypes,
	// because legitimate hand-writes exist — an admin declaring
	// ownership IS the feature. And like the rest of the control plane
	// it stays open in a fully unconfigured tenant: coherent, not a
	// hole, because a tenant with no RBAC distinguishes no users, so
	// protecting the declaration from "someone less privileged" only
	// means anything once RBAC exists at all.
	"SystemOfRecord": true,
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

// systemOnlyWriteTypes are entity types no user-driven write path may
// touch, even inside the RBAC bootstrap window that keeps
// controlPlaneTypes open until a tenant configures RBAC. That window
// exists so the first admin can be created; ExternalIdentity has no
// such bootstrapping need, and leaving it open pre-configuration would
// let any authenticated user re-point the next keyed import at an
// arbitrary record (re-verification finding, 2026-08-01). Its writes
// happen exclusively through the import engine's raw-engine
// side-channel (machine/system callers bypass this resolver entirely).
// Checked before the admin bypass deliberately: an admin hand-editing
// identity rows breaks import idempotency exactly like anyone else —
// there is no legitimate hand-written identity row.
var systemOnlyWriteTypes = map[string]bool{
	"ExternalIdentity": true,
}

// CanWrite reports whether the actor may create/update/delete records
// of entityType.
func (r *Resolver) CanWrite(ctx context.Context, entityType string) (bool, error) {
	if r.machine {
		return true, nil
	}
	if systemOnlyWriteTypes[entityType] {
		return false, nil
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

	// sorMemo caches SystemOfRecord rows per entity type for this
	// instance's (per-request) lifetime — see sorRules in sor.go.
	sorMemo map[string][]sorRule

	// sorBypassSourceID, when non-empty, scopes the system-of-record
	// read-only check out for records identified against exactly this
	// ExternalSQLSource — set only by ForImportFrom (sor.go), never
	// directly. RBAC checks are unaffected by it.
	sorBypassSourceID string
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
	// No system-of-record read-only check on Create: a NEW record cannot
	// have been imported from anywhere (no ExternalIdentity row can name
	// an id that doesn't exist yet) — a read_only declaration mirrors
	// existing legacy records, it doesn't forbid the tenant creating its
	// own. Saving a reserved SoR mode is refused, though (sor.go).
	if err := checkSoRModeReserved(def, fields); err != nil {
		return data.Record{}, err
	}
	effective, err := g.EffectiveWriteFields(ctx, def, "", fields)
	if err != nil {
		return data.Record{}, err
	}
	rec, err := g.raw.Create(ctx, def, effective, actor)
	if err != nil {
		return data.Record{}, g.sanitizeTargetConstraintError(ctx, err)
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
	// System-of-record checks after RBAC (sor.go): a record imported from
	// a read_only external source cannot be hand-edited — data ownership,
	// not privilege, so admins and machine actors are NOT exempt. The one
	// sanctioned writer is that source's own import, which holds a
	// scoped bypass for exactly its own source (ForImportFrom, sor.go).
	if err := checkSoRModeReserved(def, fields); err != nil {
		return 0, err
	}
	if err := g.checkSoRReadOnly(ctx, def, id); err != nil {
		return 0, err
	}
	effective, err := g.EffectiveWriteFields(ctx, def, id, fields)
	if err != nil {
		return 0, err
	}
	version, err := g.raw.Update(ctx, def, id, effective, expectedVersion, actor)
	if err != nil {
		return 0, g.sanitizeTargetConstraintError(ctx, err)
	}
	return version, nil
}

// sanitizeTargetConstraintError generalizes a crud.TargetConstraintError's
// message when the caller cannot read the joined entity type its detail
// names (JoinEntity, set only for an entity-join TargetFilter condition —
// see TargetConstraintError's own doc comment) — closing the
// create-and-rollback membership-probe ResolveReferenceFilter's doc
// comment above describes: without this, a caller denied
// CanRead(cond.Entity) could still learn whether some candidate target
// holds a PartyRole (or any other joined-entity fact) by reading the
// verbatim entity/field/value text a rejected Create/Update carried.
//
// Every other TargetConstraintError shape (direct-field TargetFilter,
// MustMatchParentField) is left as-is: JoinEntity is empty for both, and
// neither reads an entity type the caller doesn't already hold via its
// own submitted record's fields, so there is nothing to strip.
//
// RESIDUAL RISK, stated plainly (independent review, uc-infra#78
// follow-up, not fully closed by this fix): this generalizes the
// MESSAGE, not the create-and-rollback probe itself. A caller denied
// CanRead(JoinEntity) who repeatedly attempts a create with different
// candidate target ids still learns join membership from the
// 400-vs-201 OUTCOME alone, message text aside — same information,
// slower to extract. Closing that fully would mean either never
// distinguishing this failure from any other write failure (worse UX
// for the overwhelmingly common case: a legitimate caller's own typo)
// or pre-checking CanRead(JoinEntity) up front and refusing the write
// outright when it's false (a bigger behavior change than this fix
// pass's scope — it would also 403 a write that might otherwise
// legitimately fail for an unrelated reason). Flagged here and in the
// code review doc rather than silently left implied fixed.
func (g *GuardedEngine) sanitizeTargetConstraintError(ctx context.Context, err error) error {
	var tce *crud.TargetConstraintError
	if !errors.As(err, &tce) || tce.JoinEntity == "" {
		return err
	}
	ok, checkErr := g.res.CanRead(ctx, tce.JoinEntity)
	if checkErr != nil {
		return checkErr
	}
	if ok {
		return err
	}
	return &crud.TargetConstraintError{
		EntityType: tce.EntityType,
		FieldName:  tce.FieldName,
		JoinEntity: tce.JoinEntity,
		Detail:     "target does not satisfy a reference constraint",
	}
}

func (g *GuardedEngine) Delete(ctx context.Context, def *entity.Definition, id string, actor audit.Actor) error {
	if err := g.checkWrite(ctx, def); err != nil {
		return err
	}
	// Deleting an imported read-only record is as much a hand-edit as
	// updating it — the next sync would just resurrect it (or worse,
	// re-create it under a new id, breaking every reference). Same check,
	// same non-exemptions, as Update above.
	if err := g.checkSoRReadOnly(ctx, def, id); err != nil {
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

// ListPageFiltered / CountFiltered mirror ListPage's guarding: read
// permission is checked, and rows are redacted before return. The
// list-page handler validates that a sort/filter field is a REAL field
// before calling — but a field this actor may not READ must not become a
// filter/sort oracle either, so a hidden field used as a filter or sort
// key is refused here (a field whose values you cannot see, you cannot
// sort or filter by, or you could bisect its values through page order).
func (g *GuardedEngine) ListPageFiltered(ctx context.Context, def *entity.Definition, opts data.ListPageOptions) ([]data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return nil, err
	}
	if err := g.rejectHiddenSortFilter(ctx, def, opts); err != nil {
		return nil, err
	}
	recs, err := g.raw.ListPageFiltered(ctx, def, opts)
	if err != nil {
		return nil, err
	}
	if err := g.redact(ctx, def, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func (g *GuardedEngine) CountFiltered(ctx context.Context, def *entity.Definition, opts data.ListPageOptions) (int, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return 0, err
	}
	if err := g.rejectHiddenSortFilter(ctx, def, opts); err != nil {
		return 0, err
	}
	return g.raw.CountFiltered(ctx, def, opts)
}

// rejectHiddenSortFilter denies sorting or filtering by a field this
// actor may not read — see ListPageFiltered's doc comment on the oracle.
//
// This must also cover opts.EqualsFilters, not just SortField/FilterField
// (independent review, uc-infra#78 follow-up): EqualsFilters is how a
// FieldReference's declared TargetFilter/MustMatchParentField narrowing
// reaches this query (internal/kernel/crud.Engine.ResolveReferenceFilter),
// and MustMatchParentField's entry carries the CALLER-supplied
// sibling_value query param
// (internal/api/reference_search.go) as its equality value against a
// field name that can be RBAC-hidden on this entity type (e.g.
// Task.project_id hidden from a role that can still read Task). Without
// this check, GET /api/references/Task?...&sibling_value=<project-uuid>
// turns a hidden field into a membership oracle: the response set
// reveals exactly which Tasks have that hidden project_id, letting a
// caller reconstruct the whole hidden mapping by iterating candidate
// values — the same class of leak SortField/FilterField were already
// guarded against, just reached through a newer field of
// ListPageOptions instead of an older one.
//
// The direct-field TargetFilter shape (Field.TargetFilter with
// Entity == "") ALSO lands in EqualsFilters, but with a Definition-constant
// Value the caller never supplies (e.g. "status"="active") — it can't
// itself be turned into an oracle for anything the caller doesn't already
// know from the Definition. It is still checked here rather than
// special-cased around: gating every EqualsFilters entry uniformly is
// simpler to reason about than one that depends on which of the two
// TargetFilter shapes produced it, and it costs nothing (a Definition
// legitimately narrowing by a hidden field would be unusual and worth
// surfacing, not silently allowing).
func (g *GuardedEngine) rejectHiddenSortFilter(ctx context.Context, def *entity.Definition, opts data.ListPageOptions) error {
	if opts.SortField == "" && opts.FilterField == "" && len(opts.EqualsFilters) == 0 {
		return nil
	}
	hidden, err := g.res.HiddenFields(ctx, def.EntityType)
	if err != nil {
		return err
	}
	for _, f := range []string{opts.SortField, opts.FilterField} {
		if f != "" && hidden[f] {
			return fmt.Errorf("%w: cannot sort or filter %s by hidden field %s", ErrDenied, def.EntityType, f)
		}
	}
	for _, eq := range opts.EqualsFilters {
		if hidden[eq.Field] {
			return fmt.Errorf("%w: cannot filter %s by hidden field %s", ErrDenied, def.EntityType, eq.Field)
		}
	}
	return nil
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

// ResolveReferenceFilter is NOT a bare delegate, unlike
// ValidateStatusTransition just above — an entity-join TargetFilter
// condition (Field.TargetFilter's Entity set, e.g. TimeEntry.employee_id's
// PartyRole condition) resolves by reading ANOTHER entity type's data
// (PartyRole) the caller may not hold read permission on at all. Without
// a gate here, whether a given Party id appears in the narrowed
// candidate set becomes an oracle for "does this Party hold the
// employee PartyRole" — disclosing PartyRole membership to a viewer who
// was never granted CanRead("PartyRole"), the same class of leak
// rejectHiddenSortFilter already guards against for SortField/FilterField
// (independent review, this commit — the original draft's doc comment
// here claimed "nothing here grants a read this actor didn't already
// have," which was true for MustMatchParentField's own-field comparison
// but not for this case).
//
// Fixed by dropping any Entity-join condition the caller cannot read
// before delegating — the field's OTHER conditions (a direct-field
// TargetFilter, or MustMatchParentField) still apply; only the
// unreadable join stops narrowing. This fails toward the picker simply
// narrowing LESS for that viewer for the READ (picker) path specifically.
//
// CORRECTION (independent review, uc-infra#78 follow-up): the paragraph
// above used to go on to claim this "fails toward the picker simply
// narrowing LESS... rather than toward disclosing anything ... or toward
// breaking the picker outright" full stop — that overstated what this
// function alone achieves. crud.Engine's own write-time enforcement
// (checkReferenceTargetConstraints, called from unguarded Create/Update)
// is deliberately unaffected by this gate — it must still evaluate the
// FULL Definition, including a join the picker just hid, or the
// constraint itself would stop being enforced for that viewer. Create/
// Update's error DID, before this fix, name the joined entity/field/
// value verbatim regardless of whether the caller could read that
// entity at all, which is a second, narrower oracle of the same kind:
// a caller denied CanRead(cond.Entity) could still learn PartyRole
// membership one candidate at a time via repeated create-and-reject
// probes, reading the message text (or even just the 400-vs-201 outcome)
// rather than the picker's candidate list. GuardedEngine.Create/Update
// now call sanitizeTargetConstraintError below to generalize that
// message when CanRead(JoinEntity) is false — see its own doc comment,
// including the residual risk that fix does NOT close (the outcome-only
// signal, message text aside).
//
// THIRD CORRECTION (uc-infra#106): CanRead(cond.Entity) alone is not
// enough. It gates whether the caller may read the joined entity TYPE at
// all, but says nothing about whether the specific FIELDS the condition
// names on that entity — EntityField (the field expected to equal the
// candidate's own id) and Field (the Definition-constant condition
// alongside it) — are themselves RBAC-hidden via FieldPermission. A
// caller who CAN read PartyRole in general but has a hidden=true
// FieldPermission on PartyRole.party_id specifically must not have that
// field used to narrow the candidate set regardless: the response body
// never returns it, but whether a given candidate id is included or
// excluded is still the same disclosure the hidden FieldPermission exists
// to prevent, just reached through the join instead of a direct read.
// Both EntityField and Field are checked, not only EntityField:
// rejectHiddenSortFilter's own doc comment already makes the "gate
// uniformly regardless of which shape produced the condition" argument
// for the primary entity's EqualsFilters entries, and the same reasoning
// applies here — a Field match hidden on the joined entity is just as
// much a per-candidate oracle as EntityField would be, and there is no
// cheaper, more special-cased way to tell them apart that's worth the
// complexity. That borrowed reasoning covers only WHICH fields to check,
// not the failure mode: a hidden direct-field condition (Entity == "")
// still denies the whole ListPageFiltered/CountFiltered call via
// rejectHiddenSortFilter, while a hidden join-field condition here is
// silently DROPPED, narrowing less rather than refusing outright — the
// same drop-don't-deny posture the CanRead(cond.Entity) check above
// already established for an unreadable join entity, kept consistent
// rather than introducing a third behavior. One visible consequence: the
// picker can still offer a candidate this narrowing would otherwise have
// excluded, whose subsequent Create/Update then 400s — see the note
// below on why that write-time check is unaffected by this fix.
//
// This closes the READ path only (the picker's own narrowing) — the
// same asymmetry the CORRECTION above already accepts for
// CanRead(cond.Entity). checkReferenceTargetConstraints (crud package,
// called from unguarded Create/Update) still evaluates the FULL
// Definition regardless of whether a join field is hidden, deliberately:
// the constraint itself must keep being enforced for that viewer, or a
// hidden field would double as a way to bypass validation entirely.
// sanitizeTargetConstraintError below only generalizes the error message
// when CanRead(JoinEntity) is false, never on a hidden join field, and
// crud.TargetConstraintError doesn't carry EntityField/Field to check
// against in any case — so a caller with WRITE permission on the
// referencing entity can still probe this join's condition one candidate
// at a time via repeated create-and-reject attempts, same as the
// outcome-only residual risk (400 vs. 201, no message-text leak per
// handlers.go's generic per-field error) sanitizeTargetConstraintError's
// own doc comment already discloses for the unreadable-entity case.
// Narrowing this further needs TargetConstraintError to carry the join's
// field names, not attempted here (uc-infra#106 follow-up, tracked
// separately rather than expanding this fix's scope).
func (g *GuardedEngine) ResolveReferenceFilter(ctx context.Context, f entity.Field, siblingValue string) (data.ListPageOptions, error) {
	if len(f.TargetFilter) > 0 {
		readable := make([]entity.TargetFilterCondition, 0, len(f.TargetFilter))
		for _, cond := range f.TargetFilter {
			if cond.Entity != "" {
				ok, err := g.res.CanRead(ctx, cond.Entity)
				if err != nil {
					return data.ListPageOptions{}, err
				}
				if !ok {
					continue
				}
				hidden, err := g.res.HiddenFields(ctx, cond.Entity)
				if err != nil {
					return data.ListPageOptions{}, err
				}
				if hidden[cond.EntityField] || hidden[cond.Field] {
					continue
				}
			}
			readable = append(readable, cond)
		}
		f.TargetFilter = readable
	}
	return g.raw.ResolveReferenceFilter(ctx, f, siblingValue)
}
