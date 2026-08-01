package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Same fresh-database-per-test convention as internal/kernel/foundation's
// own tests (and testexec.FreshDatabase): a real Postgres, isolated per
// test, dropped on cleanup — RBAC resolution is exactly the kind of
// query-shape logic mocking a database would fake-verify.
func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_authz_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

// fixture publishes foundation and returns the raw engine plus a
// definition lookup — the raw engine stands in for "system setup"
// (seeding roles/permissions), exactly the split the package comment
// describes.
type fixture struct {
	db     *sql.DB
	engine *crud.Engine
	defs   map[string]*entity.Definition
	actor  audit.Actor
	t      *testing.T
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "test-admin"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	return &fixture{
		db:     tenantDB,
		engine: crud.NewEngine(tenantDB),
		defs:   map[string]*entity.Definition{},
		actor:  actor,
		t:      t,
	}
}

func (f *fixture) def(entityType string) *entity.Definition {
	f.t.Helper()
	if d, ok := f.defs[entityType]; ok {
		return d
	}
	v, err := data.NewEntityDefinitionRepo(f.db).GetPublished(context.Background(), entityType)
	if err != nil {
		f.t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		f.t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	f.defs[entityType] = d
	return d
}

func (f *fixture) create(entityType string, fields map[string]any) data.Record {
	f.t.Helper()
	rec, err := f.engine.Create(context.Background(), f.def(entityType), fields, f.actor)
	if err != nil {
		f.t.Fatalf("create %s: %v", entityType, err)
	}
	return rec
}

func (f *fixture) role(code string) data.Record {
	return f.create("Role", map[string]any{"code": code, "name": code})
}

func (f *fixture) grant(userID, roleID string) {
	f.create("UserRole", map[string]any{"user_id": userID, "role_id": roleID})
}

func (f *fixture) permit(roleID, entityType string, canRead, canWrite bool) {
	f.create("Permission", map[string]any{
		"role_id": roleID, "entity_type": entityType,
		"can_read": canRead, "can_write": canWrite,
	})
}

func humanResolver(f *fixture, userID string) *Resolver {
	return NewResolver(f.db, audit.Actor{Type: audit.ActorHuman, ID: userID}, false)
}

func mustCan(t *testing.T, got bool, err error, want bool, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// No Permission rows for an entity type -> not opted into RBAC -> fully
// accessible, even for a user with zero roles. This IS the
// backward-compatibility guarantee: every tenant provisioned before
// this package existed has zero Permission rows everywhere.
func TestResolver_NoRules_AllowsEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := humanResolver(f, "nobody-with-no-roles")

	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "CanRead(Party) with no rules")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "CanWrite(Party) with no rules")
}

// One Permission row flips the entity type to deny-unless-granted:
// ungranted users lose access, the granted role gets exactly what its
// row says (read here, not write).
func TestResolver_RulesExist_DenyUnlessGranted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.permit(clerk.ID, "Party", true, false)

	// The granted role: read yes, write no.
	r := humanResolver(f, "user-clerk")
	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "clerk CanRead(Party)")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, false, "clerk CanWrite(Party)")

	// A user with no roles at all: denied both ways now.
	r = humanResolver(f, "user-outsider")
	got, err = r.CanRead(ctx, "Party")
	mustCan(t, got, err, false, "outsider CanRead(Party)")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, false, "outsider CanWrite(Party)")

	// Another entity type stays un-opted-in and open for everyone.
	got, err = r.CanRead(ctx, "Address")
	mustCan(t, got, err, true, "outsider CanRead(Address) — no rules there")
}

// Grants are additive: read-only from one role + write-only from
// another = both, for a user holding both.
func TestResolver_UnionAcrossRoles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	reader := f.role("reader")
	writer := f.role("writer")
	f.grant("user-both", reader.ID)
	f.grant("user-both", writer.ID)
	f.permit(reader.ID, "Party", true, false)
	f.permit(writer.ID, "Party", false, true)

	r := humanResolver(f, "user-both")
	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "union CanRead(Party)")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "union CanWrite(Party)")

	// Holding only the read half really is only the read half.
	f.grant("user-reader", reader.ID)
	r = humanResolver(f, "user-reader")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, false, "reader-only CanWrite(Party)")
}

// A write grant implies read: can_write-only Permission rows still
// resolve CanRead true (the alternative lets an update commit and then
// 403 on its own read-back — ADR-0006).
func TestResolver_WriteImpliesRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	writer := f.role("writer")
	f.grant("user-writer", writer.ID)
	f.permit(writer.ID, "Party", false, true) // write only, no explicit read

	r := humanResolver(f, "user-writer")
	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "writer CanRead(Party) — implied by can_write")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "writer CanWrite(Party)")
}

// The RBAC control plane protects itself: while a tenant is completely
// unconfigured (no admin grant, no rules) writes to Role/UserRole/
// Permission/FieldPermission stay open (bootstrap), but the moment RBAC
// is configured they flip to deny-unless-granted — otherwise any
// tenant_member could author themselves a tenant_admin grant and
// dissolve every rule in the tenant (the privilege-escalation hole the
// independent review of this commit caught).
func TestResolver_ControlPlane_SelfProtection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Bootstrap window: nothing configured, control plane open.
	r := humanResolver(f, "user-first")
	got, err := r.CanWrite(ctx, "Role")
	mustCan(t, got, err, true, "bootstrap CanWrite(Role)")

	// ...except ExternalIdentity, which is denied even INSIDE the
	// bootstrap window (systemOnlyWriteTypes): the window exists so the
	// first admin can be created, and identity rows have no such
	// bootstrapping need — an open pre-RBAC write path would let any
	// member re-point the next keyed import at an arbitrary record.
	got, err = r.CanWrite(ctx, "ExternalIdentity")
	mustCan(t, got, err, false, "bootstrap CanWrite(ExternalIdentity)")
	// Reads stay open — the legacy-key map is not a secret, just not
	// user-editable.
	got, err = r.CanRead(ctx, "ExternalIdentity")
	mustCan(t, got, err, true, "bootstrap CanRead(ExternalIdentity)")

	// SystemOfRecord is control-plane (its doc comment on
	// controlPlaneTypes: the row that decides whether imported records
	// are writable must not be flippable by anyone it protects against)
	// but NOT system-only: in the bootstrap window it stays open like
	// Role — a tenant with no RBAC distinguishes no users, so
	// self-disarm protection means nothing yet.
	got, err = r.CanWrite(ctx, "SystemOfRecord")
	mustCan(t, got, err, true, "bootstrap CanWrite(SystemOfRecord)")

	// Configure RBAC: a real tenant_admin grant.
	admin := f.role(AdminRoleCode)
	f.grant("user-admin", admin.ID)

	// Non-admin writes to every control-plane type: denied now, even
	// though no Permission row names these types. ExternalIdentity is in
	// this set for its redirection-of-authority risk (uc-infra#101): an
	// identity row decides which existing record a keyed re-import will
	// silently UPDATE, so authoring one is re-pointing the next import —
	// the import engine writes identities through a raw-engine
	// side-channel instead (sqlsource.RecordEngine's doc comment).
	r = humanResolver(f, "user-mallory")
	for _, ct := range []string{"Role", "UserRole", "Permission", "FieldPermission", "Delegation", "ExternalIdentity", "SystemOfRecord"} {
		got, err = r.CanWrite(ctx, ct)
		mustCan(t, got, err, false, "mallory CanWrite("+ct+") once configured")
	}
	// Reads keep the normal opt-in semantics (no rows -> open).
	got, err = r.CanRead(ctx, "Role")
	mustCan(t, got, err, true, "mallory CanRead(Role) — reads stay opt-in")
	// Ordinary entity types are untouched by the control-plane rule.
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "mallory CanWrite(Party) — no rules there")

	// The admin, and a deliberately-delegated role, can still write.
	r = humanResolver(f, "user-admin")
	got, err = r.CanWrite(ctx, "UserRole")
	mustCan(t, got, err, true, "admin CanWrite(UserRole)")
	// ...but NOT ExternalIdentity — system-only means even admins: a
	// hand-written identity row breaks import idempotency regardless of
	// who writes it.
	got, err = r.CanWrite(ctx, "ExternalIdentity")
	mustCan(t, got, err, false, "admin CanWrite(ExternalIdentity) — system-only")
	// SystemOfRecord IS admin-writable once configured — an admin
	// declaring ownership is the feature, unlike identity rows which
	// have no legitimate hand-writer.
	got, err = r.CanWrite(ctx, "SystemOfRecord")
	mustCan(t, got, err, true, "admin CanWrite(SystemOfRecord)")

	hr := f.role("hr-manager")
	f.grant("user-hr", hr.ID)
	f.permit(hr.ID, "UserRole", true, true) // explicit delegation
	r = humanResolver(f, "user-hr")
	got, err = r.CanWrite(ctx, "UserRole")
	mustCan(t, got, err, true, "delegated hr CanWrite(UserRole)")
	got, err = r.CanWrite(ctx, "Role")
	mustCan(t, got, err, false, "delegated hr CanWrite(Role) — not delegated")
}

// The tenant_admin role code bypasses everything — the
// lockout-prevention convention (ADR-0006).
func TestResolver_TenantAdminBypasses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.role(AdminRoleCode)
	locked := f.role("locked-down")
	f.grant("user-admin", admin.ID)
	// Rules exist for Party and grant nothing to admin's own role row.
	f.permit(locked.ID, "Party", false, false)

	r := humanResolver(f, "user-admin")
	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "tenant_admin CanRead(Party)")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "tenant_admin CanWrite(Party)")
}

// Machine (svcauth) actors bypass RBAC entirely — they're coarse-gated
// by Zitadel's tenant_integration role upstream (ADR-0006's
// machine-actor posture).
func TestResolver_MachineActorBypasses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	locked := f.role("locked-down")
	f.permit(locked.ID, "Party", false, false)

	r := NewResolver(f.db, audit.Actor{Type: audit.ActorHuman, ID: "svc:connector"}, true)
	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "machine CanRead(Party)")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "machine CanWrite(Party)")
}

// The guarded engine end to end: denied calls fail with ErrDenied (the
// errors.Is contract handlers map to 403) and write nothing; allowed
// calls behave exactly like the raw engine, audit row included.
func TestGuardedEngine_EnforcesAndDelegates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.permit(clerk.ID, "Party", true, false) // read yes, write no

	g := Guard(f.engine, humanResolver(f, "user-clerk"))

	// Read allowed: sees the fixture's own writes.
	if _, err := g.List(ctx, f.def("Party")); err != nil {
		t.Fatalf("guarded List(Party) should be allowed: %v", err)
	}

	// Write denied: ErrDenied, and nothing lands in the table.
	before, err := f.engine.Count(ctx, f.def("Party"))
	if err != nil {
		t.Fatalf("count before: %v", err)
	}
	_, err = g.Create(ctx, f.def("Party"), map[string]any{
		"name": "Should Never Exist", "party_type": "organization", "status": "active",
	}, audit.Actor{Type: audit.ActorHuman, ID: "user-clerk"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("guarded Create should be ErrDenied, got %v", err)
	}
	after, err := f.engine.Count(ctx, f.def("Party"))
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("denied Create still wrote a record: %d -> %d", before, after)
	}

	// Read denied for an outsider, on every read shape.
	gOut := Guard(f.engine, humanResolver(f, "user-outsider"))
	if _, err := gOut.List(ctx, f.def("Party")); !errors.Is(err, ErrDenied) {
		t.Fatalf("outsider List should be ErrDenied, got %v", err)
	}
	if _, err := gOut.Count(ctx, f.def("Party")); !errors.Is(err, ErrDenied) {
		t.Fatalf("outsider Count should be ErrDenied, got %v", err)
	}
	if _, err := gOut.ListPage(ctx, f.def("Party"), 10, 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("outsider ListPage should be ErrDenied, got %v", err)
	}
	if _, err := gOut.ListByField(ctx, f.def("Party"), "name", "x"); !errors.Is(err, ErrDenied) {
		t.Fatalf("outsider ListByField should be ErrDenied, got %v", err)
	}
	if _, err := gOut.Get(ctx, f.def("Party"), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrDenied) {
		t.Fatalf("outsider Get should be ErrDenied, got %v", err)
	}

	// Fully-open entity type through the same guarded engine: untouched
	// behavior, audit row written by the raw engine as always. (The
	// parent Party is seeded via the raw engine — system setup — since
	// Party itself is locked down for this outsider.)
	party := f.create("Party", map[string]any{
		"name": "Address Owner", "party_type": "organization", "status": "active",
	})
	rec, err := gOut.Create(ctx, f.def("Address"), map[string]any{
		"party_id": party.ID, "address_type": "billing",
		"line1": "1 Test Street", "city": "Testville", "country_code": "GB",
	}, audit.Actor{Type: audit.ActorHuman, ID: "user-outsider"})
	if err != nil {
		t.Fatalf("Create(Address) with no rules should pass through: %v", err)
	}
	var audits int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE entity_type = 'Address' AND record_id = $1`, rec.ID,
	).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 1 {
		t.Fatalf("expected 1 audit row for pass-through create, got %d", audits)
	}
}

// TestResolver_DeletedRole_RevokesAccess is the regression test for a
// bug independent review found while reviewing an unrelated change to
// foundation.RoleGrantsForUser (department-scoped UserRole grants,
// erp/BACKLOG-TASKS.md's "Department-scoped approval routing" task): a
// draft of that rewrite stopped filtering out UserRole rows whose
// role_id no longer resolves to a live Role record (crud.Engine.Delete
// soft-deletes with no cascade to referencing rows, so this is a real,
// reachable state, not hypothetical), which meant deleting a Role
// silently left every Permission grant it had authorized still active —
// an admin revoking a role got no actual revocation. Confirms both
// halves of that blast radius: entity-level CanRead/CanWrite and
// field-level HiddenFields both go back to "as if the grant never
// existed" the moment the Role record is deleted.
func TestResolver_DeletedRole_RevokesAccess(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.permit(clerk.ID, "Party", true, true)
	f.create("FieldPermission", map[string]any{
		"role_id": clerk.ID, "entity_type": "Party", "field_name": "name", "hidden": true,
	})

	r := humanResolver(f, "user-clerk")
	got, err := r.CanRead(ctx, "Party")
	mustCan(t, got, err, true, "clerk CanRead(Party) before Role deletion")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, true, "clerk CanWrite(Party) before Role deletion")
	hidden, err := r.HiddenFields(ctx, "Party")
	if err != nil {
		t.Fatalf("HiddenFields before Role deletion: %v", err)
	}
	if !hidden["name"] {
		t.Fatalf("expected Party.name hidden before Role deletion, got %v", hidden)
	}

	if err := f.engine.Delete(ctx, f.def("Role"), clerk.ID, f.actor); err != nil {
		t.Fatalf("delete Role clerk: %v", err)
	}

	// A fresh Resolver — loadRoles memoizes per-instance, and the point
	// is a new request (a fresh Resolver, same as every real HTTP
	// request gets) sees the revocation, not that a live instance's
	// cache invalidates mid-request.
	r = humanResolver(f, "user-clerk")
	got, err = r.CanRead(ctx, "Party")
	mustCan(t, got, err, false, "clerk CanRead(Party) after Role deletion")
	got, err = r.CanWrite(ctx, "Party")
	mustCan(t, got, err, false, "clerk CanWrite(Party) after Role deletion")
	hidden, err = r.HiddenFields(ctx, "Party")
	if err != nil {
		t.Fatalf("HiddenFields after Role deletion: %v", err)
	}
	if hidden["name"] {
		t.Fatalf("expected Party.name NOT hidden after Role deletion (the only role hiding it is gone), got %v", hidden)
	}
}

// A denied writer still gets validation-first behavior on
// ValidateStatusTransition (deliberately ungated — see its doc
// comment); the write itself is what carries the 403.
func TestGuardedEngine_ValidateStatusTransitionDelegates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	locked := f.role("locked-down")
	f.permit(locked.ID, "Party", false, false)

	g := Guard(f.engine, humanResolver(f, "user-outsider"))
	// Party has no StatusTypeCode, so this is a no-op nil either way —
	// the point is it must NOT return ErrDenied.
	if err := g.ValidateStatusTransition(ctx, f.def("Party"), "", map[string]any{}, true, nil); err != nil {
		t.Fatalf("ValidateStatusTransition should delegate ungated: %v", err)
	}
}
