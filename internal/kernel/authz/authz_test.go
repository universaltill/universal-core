package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
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

// createUnconstrained writes a record directly via data.RecordRepo,
// skipping only crud.Engine's audit row and record_unique_keys bookkeeping
// (uc-infra#121's SystemOfRecord.Unique is enforced by
// crud.Engine.Create/Update, not by this lower layer) — named
// "unconstrained," not "raw," because this package's OWN established
// meaning of "raw" is the unguarded *crud.Engine itself (sorFixture's doc
// comment: "seeds, via the RAW engine"; sor.go's "raw-engine side-
// channel"), a different thing from bypassing crud.Engine entirely.
//
// This is exactly what a row written before that Definition version bump
// reached a tenant looks like: the "most restrictive wins" fallback these
// tests exercise exists FOR that pre-migration case (SystemOfRecord's own
// doc comment), so simulating it here — rather than going through
// f.create, which the real Unique constraint now correctly rejects for a
// live duplicate — is the honest way to keep testing the fallback without
// testing something the write path no longer allows to happen for real.
//
// Still runs entity.ValidateRecord first, even though crud.Engine.Create
// would otherwise do that: skipping the audit/unique-key side effects is
// the point, not skipping shape validation — an invalid fields map (e.g.
// an unrecognized enum value) reaching record_unique_keys' bookkeeping
// bypass would silently also bypass the shape check ordinary Create
// would have caught, quietly weakening what these tests are actually
// exercising (independent review, uc-infra#121 follow-up).
func (f *fixture) createUnconstrained(entityType string, fields map[string]any) data.Record {
	f.t.Helper()
	if err := entity.ValidateRecord(f.def(entityType), fields); err != nil {
		f.t.Fatalf("createUnconstrained %s: invalid fields: %v", entityType, err)
	}
	rec, err := data.NewRecordRepo(f.db).Create(context.Background(), entityType, fields)
	if err != nil {
		f.t.Fatalf("createUnconstrained %s: %v", entityType, err)
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

// TestResolver_AIProviderConnection_SystemOnlyWrite confirms the
// uc-infra#180 gap is actually closed: AIProviderConnection is not a
// controlPlaneType (it grants no authority over anything else), but it
// IS systemOnlyWriteTypes — no user-driven write path may touch it,
// bootstrap window or not, admin or not — because the Unique constraint
// its foundation.go doc comment describes is only sound if
// internal/api/aiprovidersettings.go's own raw-engine write is the sole
// writer. Reads stay open throughout, same as ExternalIdentity: nothing
// here is secret, only not user-editable.
func TestResolver_AIProviderConnection_SystemOnlyWrite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Bootstrap window: nothing configured yet.
	r := humanResolver(f, "user-first")
	got, err := r.CanWrite(ctx, "AIProviderConnection")
	mustCan(t, got, err, false, "bootstrap CanWrite(AIProviderConnection)")
	got, err = r.CanRead(ctx, "AIProviderConnection")
	mustCan(t, got, err, true, "bootstrap CanRead(AIProviderConnection)")

	// Configure RBAC with a real tenant_admin grant — still denied, even
	// to the admin, same posture as ExternalIdentity.
	admin := f.role(AdminRoleCode)
	f.grant("user-admin", admin.ID)
	r = humanResolver(f, "user-admin")
	got, err = r.CanWrite(ctx, "AIProviderConnection")
	mustCan(t, got, err, false, "admin CanWrite(AIProviderConnection) — system-only")
	got, err = r.CanRead(ctx, "AIProviderConnection")
	mustCan(t, got, err, true, "admin CanRead(AIProviderConnection)")

	// An ordinary, non-admin member: also denied, same reasoning.
	member := f.role("member")
	f.grant("user-mallory", member.ID)
	r = humanResolver(f, "user-mallory")
	got, err = r.CanWrite(ctx, "AIProviderConnection")
	mustCan(t, got, err, false, "member CanWrite(AIProviderConnection) — system-only")
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

	// EXCEPT systemOnlyWriteTypes (uc-infra#180 independent review):
	// those types have no legitimate machine writer through THIS
	// Resolver at all (their real writers are raw-engine system-path
	// bypasses that never call CanWrite), so a machine/service-token
	// caller reaching the generic API must be denied exactly like a
	// human — CanWrite checks systemOnlyWriteTypes before the machine
	// bypass, not after, specifically so this stays true.
	got, err = r.CanWrite(ctx, "ExternalIdentity")
	mustCan(t, got, err, false, "machine CanWrite(ExternalIdentity) — system-only overrides the machine bypass")
	got, err = r.CanWrite(ctx, "AIProviderConnection")
	mustCan(t, got, err, false, "machine CanWrite(AIProviderConnection) — system-only overrides the machine bypass")
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

// TestGuardedEngine_ResolveReferenceFilter_DropsUnreadableEntityJoinCondition
// (uc-infra#78, independent review finding): a caller who was not
// granted CanRead("PartyRole") must not have PartyRole membership
// disclosed to them via the reference-picker's own narrowing — an
// entity-join TargetFilter condition whose Entity they cannot read is
// dropped rather than resolved, while the field's read-permitted
// remains unaffected.
func TestGuardedEngine_ResolveReferenceFilter_DropsUnreadableEntityJoinCondition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	party := f.create("Party", map[string]any{
		"party_type": "person", "name": "Some Employee", "status": "active",
	})
	f.create("PartyRole", map[string]any{"party_id": party.ID, "role_type": "employee"})

	locked := f.role("no-party-role-read")
	f.permit(locked.ID, "Party", true, false)
	f.permit(locked.ID, "PartyRole", false, false) // explicitly denied
	f.grant("user-locked", locked.ID)

	field := entity.Field{
		Name: "employee_id", Type: entity.FieldReference, Target: "Party",
		TargetFilter: []entity.TargetFilterCondition{
			{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "employee"},
		},
	}

	g := Guard(f.engine, humanResolver(f, "user-locked"))
	opts, err := g.ResolveReferenceFilter(ctx, nil, field, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter: %v", err)
	}
	if len(opts.JoinFilters) != 0 {
		t.Fatalf("expected no JoinFilters narrowing when the caller cannot read the join entity, got %+v", opts.JoinFilters)
	}

	// The same field, for a caller who CAN read PartyRole, still narrows
	// normally — confirms the drop above is permission-specific, not a
	// blanket regression of the mechanism itself.
	open := f.role("party-role-reader")
	f.permit(open.ID, "Party", true, false)
	f.permit(open.ID, "PartyRole", true, false)
	f.grant("user-open", open.ID)
	gOpen := Guard(f.engine, humanResolver(f, "user-open"))
	opts2, err := gOpen.ResolveReferenceFilter(ctx, nil, field, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter (open): %v", err)
	}
	if len(opts2.JoinFilters) != 1 {
		t.Fatalf("expected JoinFilters narrowing when the caller CAN read the join entity, got %+v", opts2.JoinFilters)
	}
}

// TestGuardedEngine_ResolveReferenceFilter_DropsEntityJoinConditionOnHiddenJoinField
// (uc-infra#106): CanRead(cond.Entity) alone is not enough — a caller who
// CAN read PartyRole in general but has a hidden=true FieldPermission on
// the specific field the join condition names must still not have that
// field used to narrow the picker's candidate set. Whether a given Party
// id appears in the narrowed results is itself a same-class oracle as the
// entity-level one uc-infra#78 already closed: it discloses, one
// candidate at a time, which PartyRole rows carry a given (hidden)
// party_id, exactly what the FieldPermission was meant to hide.
//
// Covers both fields a join condition names on the OTHER entity —
// EntityField (the field expected to equal the candidate's own id) and
// Field (the Definition-constant condition, e.g. role_type) — since
// either one being hidden makes the same information leak through the
// EXISTS narrowing; rejectHiddenSortFilter's own doc comment already
// makes this "gate uniformly regardless of which shape produced the
// condition" argument for the primary-entity case, so the join case
// keeps the same discipline.
func TestGuardedEngine_ResolveReferenceFilter_DropsEntityJoinConditionOnHiddenJoinField(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	party := f.create("Party", map[string]any{
		"party_type": "person", "name": "Some Employee", "status": "active",
	})
	f.create("PartyRole", map[string]any{"party_id": party.ID, "role_type": "employee"})

	field := entity.Field{
		Name: "employee_id", Type: entity.FieldReference, Target: "Party",
		TargetFilter: []entity.TargetFilterCondition{
			{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "employee"},
		},
	}

	// Caller CAN read PartyRole at the entity level, but PartyRole.party_id
	// (the EntityField the join condition names) is hidden from every role
	// they hold.
	hiddenEntityField := f.role("hides-party-role-party-id")
	f.permit(hiddenEntityField.ID, "Party", true, false)
	f.permit(hiddenEntityField.ID, "PartyRole", true, false)
	f.hide(hiddenEntityField.ID, "PartyRole", "party_id")
	f.grant("user-hidden-entity-field", hiddenEntityField.ID)

	g := Guard(f.engine, humanResolver(f, "user-hidden-entity-field"))
	opts, err := g.ResolveReferenceFilter(ctx, nil, field, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter (hidden EntityField): %v", err)
	}
	if len(opts.JoinFilters) != 0 {
		t.Fatalf("expected no JoinFilters narrowing when the join's EntityField is hidden, got %+v", opts.JoinFilters)
	}

	// Same shape, but this time PartyRole.role_type (the join condition's
	// Field) is what's hidden instead of EntityField.
	hiddenField := f.role("hides-party-role-role-type")
	f.permit(hiddenField.ID, "Party", true, false)
	f.permit(hiddenField.ID, "PartyRole", true, false)
	f.hide(hiddenField.ID, "PartyRole", "role_type")
	f.grant("user-hidden-field", hiddenField.ID)

	g2 := Guard(f.engine, humanResolver(f, "user-hidden-field"))
	opts2, err := g2.ResolveReferenceFilter(ctx, nil, field, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter (hidden Field): %v", err)
	}
	if len(opts2.JoinFilters) != 0 {
		t.Fatalf("expected no JoinFilters narrowing when the join's Field is hidden, got %+v", opts2.JoinFilters)
	}

	// The same field, for a caller with no hidden FieldPermission at all on
	// PartyRole, still narrows normally — confirms the drop above is
	// field-permission-specific, not a blanket regression of the mechanism.
	open := f.role("party-role-reader-no-hidden-fields")
	f.permit(open.ID, "Party", true, false)
	f.permit(open.ID, "PartyRole", true, false)
	f.grant("user-open-fields", open.ID)
	gOpen := Guard(f.engine, humanResolver(f, "user-open-fields"))
	opts3, err := gOpen.ResolveReferenceFilter(ctx, nil, field, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter (no hidden fields): %v", err)
	}
	if len(opts3.JoinFilters) != 1 {
		t.Fatalf("expected JoinFilters narrowing when neither join field is hidden, got %+v", opts3.JoinFilters)
	}

	// Independent-review follow-up: a field with MULTIPLE TargetFilter
	// conditions — one entity-join (hidden), one direct-field (not) — must
	// drop only the affected join condition, leaving its sibling intact.
	// Proves the `continue` inside ResolveReferenceFilter's loop skips just
	// the one condition rather than the whole field's narrowing collapsing
	// to nothing whenever any single condition is hidden.
	multiField := entity.Field{
		Name: "employee_id", Type: entity.FieldReference, Target: "Party",
		TargetFilter: []entity.TargetFilterCondition{
			{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "employee"},
			{Field: "status", Value: "active"},
		},
	}
	gMulti := Guard(f.engine, humanResolver(f, "user-hidden-entity-field"))
	opts4, err := gMulti.ResolveReferenceFilter(ctx, nil, multiField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter (multi-condition, hidden join field): %v", err)
	}
	if len(opts4.JoinFilters) != 0 {
		t.Fatalf("expected the hidden join condition dropped, got JoinFilters %+v", opts4.JoinFilters)
	}
	if len(opts4.EqualsFilters) != 1 || opts4.EqualsFilters[0].Field != "status" {
		t.Fatalf("expected the sibling direct-field condition to survive untouched, got EqualsFilters %+v", opts4.EqualsFilters)
	}
}

// TestGuardedEngine_Create_TargetConstraintError_StripsJoinDetailWhenCallerCannotReadJoinEntity
// is the regression test for independent review finding #5:
// crud.Engine's write-time rejection ran unguarded against the full
// Definition, and its error message named the joined entity/field/value
// verbatim regardless of whether the caller could read that joined
// entity at all — a caller denied CanRead("PartyRole") could still probe
// its membership via repeated create-and-rollback attempts reading the
// message text. GuardedEngine.Create now generalizes the message via
// sanitizeTargetConstraintError when CanRead(JoinEntity) is false, while
// a caller who CAN read the joined entity still gets the full,
// specific detail — same "least surprise" split ResolveReferenceFilter's
// own narrowing already makes for the READ path.
func TestGuardedEngine_Create_TargetConstraintError_StripsJoinDetailWhenCallerCannotReadJoinEntity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	vendor := f.create("Party", map[string]any{
		"party_type": "organization", "name": "Vendor Co", "status": "active",
	})
	f.create("PartyRole", map[string]any{"party_id": vendor.ID, "role_type": "vendor"})

	assignmentDef := &entity.Definition{
		EntityType: "Assignment",
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldString, Required: true},
			{Name: "assignee_id", Type: entity.FieldReference, Target: "Party", TargetFilter: []entity.TargetFilterCondition{
				{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "employee"},
			}},
		},
	}

	locked := f.role("no-party-role-read-2")
	f.permit(locked.ID, "Assignment", true, true)
	f.permit(locked.ID, "PartyRole", false, false) // explicitly denied
	f.grant("user-locked-2", locked.ID)

	g := Guard(f.engine, humanResolver(f, "user-locked-2"))
	_, err := g.Create(ctx, assignmentDef, map[string]any{"title": "x", "assignee_id": vendor.ID},
		audit.Actor{Type: audit.ActorHuman, ID: "user-locked-2"})
	if !errors.Is(err, crud.ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation, got %v", err)
	}
	if strings.Contains(err.Error(), "PartyRole") || strings.Contains(err.Error(), "role_type") || strings.Contains(err.Error(), "employee") {
		t.Fatalf("expected the joined entity/field/value detail to be stripped for a caller who cannot read PartyRole, got: %v", err)
	}

	// The same field, for a caller who CAN read PartyRole, still gets the
	// full, specific detail — confirms the stripping above is
	// permission-specific, not a blanket regression of the message.
	open := f.role("party-role-reader-2")
	f.permit(open.ID, "Assignment", true, true)
	f.permit(open.ID, "PartyRole", true, false)
	f.grant("user-open-2", open.ID)
	gOpen := Guard(f.engine, humanResolver(f, "user-open-2"))
	_, err2 := gOpen.Create(ctx, assignmentDef, map[string]any{"title": "x", "assignee_id": vendor.ID},
		audit.Actor{Type: audit.ActorHuman, ID: "user-open-2"})
	if !errors.Is(err2, crud.ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation, got %v", err2)
	}
	if !strings.Contains(err2.Error(), "PartyRole") {
		t.Fatalf("expected full join detail for a caller who CAN read PartyRole, got: %v", err2)
	}
}
