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

	configuredLoaded bool
	configured       bool

	memo map[string]entityPerm
}

// NewResolver builds a Resolver for one request. actor is the
// authenticated identity (audit.Actor.ID == the Zitadel sub, the same
// value UserRole.user_id carries — ADR-0005). machine marks svcauth
// service-token requests, which bypass RBAC (package comment).
func NewResolver(db *sql.DB, actor audit.Actor, machine bool) *Resolver {
	return &Resolver{
		db:      db,
		userID:  actor.ID,
		machine: machine,
		memo:    make(map[string]entityPerm),
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

func (r *Resolver) resolve(ctx context.Context, entityType string) (entityPerm, error) {
	if p, ok := r.memo[entityType]; ok {
		return p, nil
	}
	if err := r.loadRoles(ctx); err != nil {
		return entityPerm{}, err
	}
	records := data.NewRecordRepo(r.db)
	rows, err := records.ListByField(ctx, "Permission", "entity_type", entityType)
	if err != nil {
		return entityPerm{}, fmt.Errorf("list Permission for %s: %w", entityType, err)
	}
	p := entityPerm{rulesExist: len(rows) > 0}
	for _, row := range rows {
		rid, _ := row.Data["role_id"].(string)
		if rid == "" || !r.roleIDs[rid] {
			continue
		}
		if b, _ := row.Data["can_read"].(bool); b {
			p.canRead = true
		}
		if b, _ := row.Data["can_write"].(bool); b {
			p.canWrite = true
			// A write grant implies read (package comment) — a role
			// that may change a record may also see it.
			p.canRead = true
		}
	}
	r.memo[entityType] = p
	return p, nil
}

// rbacConfigured reports whether this tenant has set up RBAC at all:
// any Permission/FieldPermission row, or any user actually granted the
// tenant_admin role. Memoized per request; only consulted for writes
// to control-plane types, so an ordinary request never pays for it.
func (r *Resolver) rbacConfigured(ctx context.Context) (bool, error) {
	if r.configuredLoaded {
		return r.configured, nil
	}
	records := data.NewRecordRepo(r.db)
	for _, t := range []string{"Permission", "FieldPermission"} {
		n, err := records.CountByEntityType(ctx, t)
		if err != nil {
			return false, fmt.Errorf("count %s: %w", t, err)
		}
		if n > 0 {
			r.configured, r.configuredLoaded = true, true
			return true, nil
		}
	}
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

func (g *GuardedEngine) Create(ctx context.Context, def *entity.Definition, fields map[string]any, actor audit.Actor) (data.Record, error) {
	if err := g.checkWrite(ctx, def); err != nil {
		return data.Record{}, err
	}
	return g.raw.Create(ctx, def, fields, actor)
}

func (g *GuardedEngine) Update(ctx context.Context, def *entity.Definition, id string, fields map[string]any, expectedVersion *int, actor audit.Actor) (int, error) {
	if err := g.checkWrite(ctx, def); err != nil {
		return 0, err
	}
	return g.raw.Update(ctx, def, id, fields, expectedVersion, actor)
}

func (g *GuardedEngine) Delete(ctx context.Context, def *entity.Definition, id string, actor audit.Actor) error {
	if err := g.checkWrite(ctx, def); err != nil {
		return err
	}
	return g.raw.Delete(ctx, def, id, actor)
}

func (g *GuardedEngine) Get(ctx context.Context, def *entity.Definition, id string) (data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return data.Record{}, err
	}
	return g.raw.Get(ctx, def, id)
}

func (g *GuardedEngine) List(ctx context.Context, def *entity.Definition) ([]data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return nil, err
	}
	return g.raw.List(ctx, def)
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
	return g.raw.ListPage(ctx, def, limit, offset)
}

func (g *GuardedEngine) ListByField(ctx context.Context, def *entity.Definition, fieldName, value string) ([]data.Record, error) {
	if err := g.checkRead(ctx, def); err != nil {
		return nil, err
	}
	return g.raw.ListByField(ctx, def, fieldName, value)
}

// ValidateStatusTransition is validation, not data access — it runs
// before a write the guarded Create/Update will still check, so it
// delegates without its own gate (a denied writer gets their 403 from
// the write itself; validating first leaks nothing).
func (g *GuardedEngine) ValidateStatusTransition(ctx context.Context, def *entity.Definition, id string, fields map[string]any, isCreate bool, expectedVersion *int) error {
	return g.raw.ValidateStatusTransition(ctx, def, id, fields, isCreate, expectedVersion)
}
