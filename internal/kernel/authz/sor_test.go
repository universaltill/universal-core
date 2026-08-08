package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/sqlsource"
)

// SoRReadOnlyError must match the sentinel under errors.Is (the contract
// internal/api will map to a translated message later) and expose its
// fields under errors.As — pure unit test, no database.
func TestSoRReadOnlyError_ErrorsIsAndAs(t *testing.T) {
	var err error = &SoRReadOnlyError{EntityType: "Party", SourceID: "src-1", SourceName: "NAV mirror"}
	if !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatal("SoRReadOnlyError should match ErrSystemOfRecordReadOnly under errors.Is")
	}
	if errors.Is(err, ErrSystemOfRecordModeReserved) || errors.Is(err, ErrDenied) {
		t.Fatal("SoRReadOnlyError must not match unrelated sentinels")
	}
	var sre *SoRReadOnlyError
	if !errors.As(err, &sre) || sre.SourceName != "NAV mirror" {
		t.Fatalf("errors.As should surface the wrapper with its fields, got %+v", sre)
	}
}

// sorFixture seeds, via the RAW engine (system setup, the same split the
// package comment describes — and, for ExternalIdentity, the ONLY
// possible writer: authz.systemOnlyWriteTypes denies its user-driven
// writes even to admins), the uc-infra#102 scenario: an external source,
// two Party records, an identity row marking one of them as imported
// from that source, and a read_only SystemOfRecord declaration for Party.
type sorFixture struct {
	*fixture
	source    data.Record
	imported  data.Record // has an ExternalIdentity row → hand-edits blocked
	handMade  data.Record // no identity row → stays editable
	partyEdit map[string]any
	editActor audit.Actor
}

func newSoRFixture(t *testing.T) *sorFixture {
	t.Helper()
	f := newFixture(t)
	source := f.create("ExternalSQLSource", map[string]any{
		"name": "NAV mirror", "driver": "postgres",
		"host": "legacy.example.internal", "database": "navmirror",
	})
	imported := f.create("Party", map[string]any{
		"name": "Imported Vendor", "party_type": "organization", "status": "active",
	})
	handMade := f.create("Party", map[string]any{
		"name": "Hand-Made Customer", "party_type": "organization", "status": "active",
	})
	f.create("ExternalIdentity", map[string]any{
		"source_id": source.ID, "source_relation": "dbo.CRONUS$Vendor",
		"entity_type": "Party", "record_id": imported.ID, "external_key": "V-1000",
	})
	f.create("SystemOfRecord", map[string]any{
		"entity_type": "Party", "source_id": source.ID, "mode": "read_only",
	})
	return &sorFixture{
		fixture:  f,
		source:   source,
		imported: imported,
		handMade: handMade,
		partyEdit: map[string]any{
			"name": "Edited Name", "party_type": "organization", "status": "active",
		},
		editActor: audit.Actor{Type: audit.ActorHuman, ID: "user-editor"},
	}
}

// guardedFor builds a fresh per-request GuardedEngine for userID —
// Party has no Permission rows in these fixtures, so RBAC lets every
// user through and what's exercised is purely the system-of-record layer.
func (sf *sorFixture) guardedFor(userID string) *GuardedEngine {
	return Guard(sf.engine, humanResolver(sf.fixture, userID))
}

// The core guarantee: a user-driven Update or Delete of the identified
// (imported) record is refused with the typed error; the un-identified
// record of the same entity type stays fully editable.
func TestGuardedEngine_SoRReadOnly_BlocksIdentifiedRecordOnly(t *testing.T) {
	sf := newSoRFixture(t)
	ctx := context.Background()
	g := sf.guardedFor("user-editor")

	// Update of the imported record: blocked, typed, named.
	_, err := g.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor)
	if !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("update of imported record: want ErrSystemOfRecordReadOnly, got %v", err)
	}
	var sre *SoRReadOnlyError
	if !errors.As(err, &sre) {
		t.Fatalf("want *SoRReadOnlyError via errors.As, got %v", err)
	}
	if sre.EntityType != "Party" || sre.SourceID != sf.source.ID || sre.SourceName != "NAV mirror" {
		t.Fatalf("SoRReadOnlyError fields wrong: %+v", sre)
	}
	// ...and nothing changed.
	rec, err := sf.engine.Get(ctx, sf.def("Party"), sf.imported.ID)
	if err != nil {
		t.Fatalf("re-read imported record: %v", err)
	}
	if rec.Data["name"] != "Imported Vendor" {
		t.Fatalf("blocked update still changed the record: %v", rec.Data["name"])
	}

	// Delete of the imported record: blocked the same way.
	if err := g.Delete(ctx, sf.def("Party"), sf.imported.ID, sf.editActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("delete of imported record: want ErrSystemOfRecordReadOnly, got %v", err)
	}

	// The hand-made record of the SAME type: no identity row, so both
	// operations pass straight through.
	if _, err := g.Update(ctx, sf.def("Party"), sf.handMade.ID, sf.partyEdit, nil, sf.editActor); err != nil {
		t.Fatalf("update of un-identified record should be allowed: %v", err)
	}
	if err := g.Delete(ctx, sf.def("Party"), sf.handMade.ID, sf.editActor); err != nil {
		t.Fatalf("delete of un-identified record should be allowed: %v", err)
	}
}

// Create is never system-of-record-gated: a new record cannot have been
// imported from anywhere, and a read_only declaration mirrors existing
// legacy records rather than forbidding the tenant its own new ones.
func TestGuardedEngine_SoRReadOnly_CreateStaysOpen(t *testing.T) {
	sf := newSoRFixture(t)
	g := sf.guardedFor("user-editor")
	if _, err := g.Create(context.Background(), sf.def("Party"), map[string]any{
		"name": "Brand New Local Party", "party_type": "person", "status": "active",
	}, sf.editActor); err != nil {
		t.Fatalf("create under a read_only SoR declaration should be allowed: %v", err)
	}
}

// Raw crud.Engine — what genuine system paths (workflow steps, seeding,
// provisioning) call directly — is NOT subject to the read-only block:
// it bypasses GuardedEngine entirely, by construction. Note the import
// commit path is deliberately NOT this: it writes records through the
// GUARDED engine (RBAC-checked) and holds only the scoped ForImportFrom
// bypass — TestCommitRowsUpserting_RealImportWiring below drives that
// real wiring.
func TestRawEngine_SoRReadOnly_ImportPathBypasses(t *testing.T) {
	sf := newSoRFixture(t)
	if _, err := sf.engine.Update(context.Background(), sf.def("Party"), sf.imported.ID, map[string]any{
		"name": "Renamed By Re-Import", "party_type": "organization", "status": "active",
	}, nil, sf.actor); err != nil {
		t.Fatalf("raw-engine update of the imported record must stay allowed: %v", err)
	}
}

// Admin is NOT exempt: the read-only block is data ownership (whose
// system masters this record), not privilege (what this user may do) —
// an admin's hand-edit would be clobbered by the next sync exactly like
// anyone else's, so tenant_admin's RBAC bypass deliberately does not
// reach past checkWrite into the SoR layer.
func TestGuardedEngine_SoRReadOnly_AdminNotExempt(t *testing.T) {
	sf := newSoRFixture(t)
	ctx := context.Background()
	admin := sf.role(AdminRoleCode)
	sf.grant("user-admin", admin.ID)

	g := sf.guardedFor("user-admin")
	adminActor := audit.Actor{Type: audit.ActorHuman, ID: "user-admin"}
	if _, err := g.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, adminActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("admin update of imported record: want ErrSystemOfRecordReadOnly, got %v", err)
	}
	if err := g.Delete(ctx, sf.def("Party"), sf.imported.ID, adminActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("admin delete of imported record: want ErrSystemOfRecordReadOnly, got %v", err)
	}
}

// platform_owned rows and entity types with no SystemOfRecord row at all
// impose nothing — even on a record that HAS an identity row (a record
// imported once into a type the tenant has since declared its own stays
// editable).
func TestGuardedEngine_SoR_PlatformOwnedAndUndeclaredStayOpen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	source := f.create("ExternalSQLSource", map[string]any{
		"name": "old CRM", "driver": "postgres", "host": "h", "database": "d",
	})

	// Address: platform_owned declaration + an identity row on the record.
	party := f.create("Party", map[string]any{
		"name": "Owner", "party_type": "organization", "status": "active",
	})
	addr := f.create("Address", map[string]any{
		"party_id": party.ID, "address_type": "billing",
		"line1": "1 Old Street", "city": "Doha", "country_code": "QA",
	})
	f.create("ExternalIdentity", map[string]any{
		"source_id": source.ID, "source_relation": "dbo.Address",
		"entity_type": "Address", "record_id": addr.ID, "external_key": "A-1",
	})
	f.create("SystemOfRecord", map[string]any{
		"entity_type": "Address", "source_id": source.ID, "mode": "platform_owned",
	})

	actor := audit.Actor{Type: audit.ActorHuman, ID: "user-editor"}
	g := Guard(f.engine, humanResolver(f, "user-editor"))
	if _, err := g.Update(ctx, f.def("Address"), addr.ID, map[string]any{
		"party_id": party.ID, "address_type": "billing",
		"line1": "2 New Street", "city": "Doha", "country_code": "QA",
	}, nil, actor); err != nil {
		t.Fatalf("update under platform_owned should be allowed: %v", err)
	}

	// Party: identity row but NO SystemOfRecord row of any kind.
	f.create("ExternalIdentity", map[string]any{
		"source_id": source.ID, "source_relation": "dbo.Customer",
		"entity_type": "Party", "record_id": party.ID, "external_key": "C-1",
	})
	if _, err := g.Update(ctx, f.def("Party"), party.ID, map[string]any{
		"name": "Renamed Owner", "party_type": "organization", "status": "active",
	}, nil, actor); err != nil {
		t.Fatalf("update with no SoR declaration should be allowed: %v", err)
	}
}

// Several SystemOfRecord rows with mixed modes for one entity type: the
// most restrictive wins — any read_only row blocks the identified record
// no matter what the others say (fail-safe, foundation.SystemOfRecord's
// documented convention).
func TestGuardedEngine_SoR_MixedModes_ReadOnlyWins(t *testing.T) {
	sf := newSoRFixture(t)
	ctx := context.Background()
	// A second, contradictory declaration for the SAME type AND source —
	// createUnconstrained, not create: uc-infra#121's Unique constraint on
	// SystemOfRecord's (entity_type, source_id) pair now rejects a second
	// LIVE row for ("Party", sf.source.ID) through the ordinary write
	// path (a genuinely redundant/contradictory re-declaration, not the
	// legitimate multi-source case that constraint deliberately still
	// allows), so this simulates the pre-migration data the "most
	// restrictive wins" fallback below still exists to handle for a
	// tenant whose sync hasn't reached this Version bump yet (see
	// createUnconstrained's own doc comment).
	sf.createUnconstrained("SystemOfRecord", map[string]any{
		"entity_type": "Party", "source_id": sf.source.ID, "mode": "platform_owned",
	})

	g := sf.guardedFor("user-editor")
	if _, err := g.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("mixed modes: want read_only to win with ErrSystemOfRecordReadOnly, got %v", err)
	}
}

// A read_only row whose optional source_id is blank (an ownership claim
// naming no owner — the entity level can't forbid it, source_id being
// optional for platform_owned's sake) is read the fail-safe way: every
// identified/imported record of the type is blocked, regardless of source.
func TestGuardedEngine_SoR_BlankSourceReadOnly_BlocksAnyImportedRecord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	source := f.create("ExternalSQLSource", map[string]any{
		"name": "NAV mirror", "driver": "postgres", "host": "h", "database": "d",
	})
	imported := f.create("Party", map[string]any{
		"name": "Imported", "party_type": "organization", "status": "active",
	})
	f.create("ExternalIdentity", map[string]any{
		"source_id": source.ID, "source_relation": "dbo.CRONUS$Vendor",
		"entity_type": "Party", "record_id": imported.ID, "external_key": "V-1",
	})
	f.create("SystemOfRecord", map[string]any{
		"entity_type": "Party", "mode": "read_only", // no source_id
	})

	g := Guard(f.engine, humanResolver(f, "user-editor"))
	actor := audit.Actor{Type: audit.ActorHuman, ID: "user-editor"}
	_, err := g.Update(ctx, f.def("Party"), imported.ID, map[string]any{
		"name": "Edited", "party_type": "organization", "status": "active",
	}, nil, actor)
	if !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("blank-source read_only: want ErrSystemOfRecordReadOnly, got %v", err)
	}
	// A hand-made record still stays editable — blank-source only widens
	// WHICH sources block, never blocks records that were never imported.
	handMade := f.create("Party", map[string]any{
		"name": "Local", "party_type": "person", "status": "active",
	})
	if _, err := g.Update(ctx, f.def("Party"), handMade.ID, map[string]any{
		"name": "Local Renamed", "party_type": "person", "status": "active",
	}, nil, actor); err != nil {
		t.Fatalf("blank-source read_only must not block un-imported records: %v", err)
	}
}

// Saving a SystemOfRecord row with the reserved "bidirectional" mode is
// refused on the guarded Create AND Update paths with the typed
// ErrSystemOfRecordModeReserved — config validation in authz, not an
// entity-enum restriction (the entity accepts the value; see
// foundation.SystemOfRecord's doc comment and the foundation-side
// TestSystemOfRecord_ModeEnum).
func TestGuardedEngine_SoR_BidirectionalModeReserved(t *testing.T) {
	sf := newSoRFixture(t)
	ctx := context.Background()
	g := sf.guardedFor("user-editor")

	_, err := g.Create(ctx, sf.def("SystemOfRecord"), map[string]any{
		"entity_type": "Address", "source_id": sf.source.ID, "mode": "bidirectional",
	}, sf.editActor)
	if !errors.Is(err, ErrSystemOfRecordModeReserved) {
		t.Fatalf("create with reserved mode: want ErrSystemOfRecordModeReserved, got %v", err)
	}

	// A legal row can be created, then not edited INTO the reserved mode.
	row, err := g.Create(ctx, sf.def("SystemOfRecord"), map[string]any{
		"entity_type": "Address", "source_id": sf.source.ID, "mode": "platform_owned",
	}, sf.editActor)
	if err != nil {
		t.Fatalf("create with a non-reserved mode should be allowed: %v", err)
	}
	_, err = g.Update(ctx, sf.def("SystemOfRecord"), row.ID, map[string]any{
		"entity_type": "Address", "source_id": sf.source.ID, "mode": "bidirectional",
	}, nil, sf.editActor)
	if !errors.Is(err, ErrSystemOfRecordModeReserved) {
		t.Fatalf("update to reserved mode: want ErrSystemOfRecordModeReserved, got %v", err)
	}
	// The reserved refusal must not read as an RBAC denial or a read-only
	// block — internal/api maps each sentinel differently.
	if errors.Is(err, ErrDenied) || errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("reserved-mode error matched an unrelated sentinel: %v", err)
	}
}

// Anti-over-blocking: a read_only rule names source A, but the record's
// identity says it came from source B — no rule governs B, so the record
// stays editable, both through the plain guarded engine and through an
// import engine scoped to B.
func TestGuardedEngine_SoR_ReadOnlyRuleForDifferentSource_DoesNotBlock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sourceA := f.create("ExternalSQLSource", map[string]any{
		"name": "NAV mirror", "driver": "postgres", "host": "h", "database": "nav",
	})
	sourceB := f.create("ExternalSQLSource", map[string]any{
		"name": "old CRM", "driver": "postgres", "host": "h", "database": "crm",
	})
	rec := f.create("Party", map[string]any{
		"name": "From CRM", "party_type": "organization", "status": "active",
	})
	f.create("ExternalIdentity", map[string]any{
		"source_id": sourceB.ID, "source_relation": "dbo.Company",
		"entity_type": "Party", "record_id": rec.ID, "external_key": "C-9",
	})
	f.create("SystemOfRecord", map[string]any{
		"entity_type": "Party", "source_id": sourceA.ID, "mode": "read_only",
	})

	actor := audit.Actor{Type: audit.ActorHuman, ID: "user-editor"}
	edit := map[string]any{"name": "From CRM (edited)", "party_type": "organization", "status": "active"}
	g := Guard(f.engine, humanResolver(f, "user-editor"))
	if _, err := g.Update(ctx, f.def("Party"), rec.ID, edit, nil, actor); err != nil {
		t.Fatalf("record from ungoverned source B must stay editable: %v", err)
	}
	if _, err := g.ForImportFrom(sourceB.ID).Update(ctx, f.def("Party"), rec.ID, edit, nil, actor); err != nil {
		t.Fatalf("import from source B must be able to write B's own ungoverned record: %v", err)
	}
}

// Anti-over-blocking: an ExternalIdentity row sharing the record_id but
// naming a DIFFERENT entity_type is out of the identity scope and must
// not block — the Go-side entity_type filter is load-bearing.
func TestGuardedEngine_SoR_IdentityForDifferentEntityType_DoesNotBlock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	source := f.create("ExternalSQLSource", map[string]any{
		"name": "NAV mirror", "driver": "postgres", "host": "h", "database": "nav",
	})
	rec := f.create("Party", map[string]any{
		"name": "Local Party", "party_type": "organization", "status": "active",
	})
	// Same record_id, but the identity claims an Address import.
	f.create("ExternalIdentity", map[string]any{
		"source_id": source.ID, "source_relation": "dbo.Address",
		"entity_type": "Address", "record_id": rec.ID, "external_key": "A-7",
	})
	f.create("SystemOfRecord", map[string]any{
		"entity_type": "Party", "source_id": source.ID, "mode": "read_only",
	})

	g := Guard(f.engine, humanResolver(f, "user-editor"))
	if _, err := g.Update(ctx, f.def("Party"), rec.ID, map[string]any{
		"name": "Renamed", "party_type": "organization", "status": "active",
	}, nil, audit.Actor{Type: audit.ActorHuman, ID: "user-editor"}); err != nil {
		t.Fatalf("identity row for a different entity_type must not block: %v", err)
	}
}

// ForImportFrom's exact semantics: the bypass is scoped to the one
// source being imported — its own records open up, another source's
// records stay blocked, the original engine is untouched (copy, not
// mutation), blank-source rules are bypassed by the identity row's
// source, and RBAC is not bypassed at all.
func TestGuardedEngine_ForImportFrom_ScopedBypass(t *testing.T) {
	sf := newSoRFixture(t)
	ctx := context.Background()
	g := sf.guardedFor("user-editor")

	// Own source: the read-only block opens for exactly this writer.
	imp := g.ForImportFrom(sf.source.ID)
	if _, err := imp.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); err != nil {
		t.Fatalf("import engine for the owning source must be able to update its record: %v", err)
	}

	// The ORIGINAL engine is unchanged — ForImportFrom copied, it didn't
	// disarm the shared per-request engine.
	if _, err := g.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("original engine must still block after ForImportFrom: got %v", err)
	}

	// A different source's import touching this record: still blocked.
	other := sf.create("ExternalSQLSource", map[string]any{
		"name": "other source", "driver": "postgres", "host": "h", "database": "other",
	})
	if _, err := g.ForImportFrom(other.ID).Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("import from a different source must NOT bypass the block: got %v", err)
	}

	// Blank-source read_only rule: the rule has no source to compare, so
	// the bypass is decided by the identity row's source — the owning
	// source's import still writes, anyone else still doesn't. Plain
	// create, not createRaw: uc-infra#121's Unique declaration is on
	// (entity_type, source_id) together, and this row's source_id is
	// absent — absent/nil exempts a record from the constraint entirely
	// (mirrors SQL NULL semantics), so a second blank-source row for
	// "Party" does not collide with the fixture's first (sourced) row.
	sf.create("SystemOfRecord", map[string]any{
		"entity_type": "Party", "mode": "read_only", // no source_id
	})
	freshImp := sf.guardedFor("user-editor").ForImportFrom(sf.source.ID) // fresh memo — the SoR rows changed
	if _, err := freshImp.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); err != nil {
		t.Fatalf("owning source's import must bypass a blank-source rule via the identity row: %v", err)
	}
	if _, err := sf.guardedFor("user-editor").Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); !errors.Is(err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("blank-source rule must still block non-import writes: got %v", err)
	}
}

// ForImportFrom scopes out ONLY the SoR read-only block — RBAC entity
// checks still run, so an import committed by a user with no write grant
// on the target type is refused exactly like any other write of theirs.
func TestGuardedEngine_ForImportFrom_DoesNotBypassRBAC(t *testing.T) {
	sf := newSoRFixture(t)
	ctx := context.Background()
	// Opt Party into RBAC with a grant the editor does not hold.
	priv := sf.role("privileged")
	sf.permit(priv.ID, "Party", true, true)

	imp := sf.guardedFor("user-editor").ForImportFrom(sf.source.ID)
	if _, err := imp.Update(ctx, sf.def("Party"), sf.imported.ID, sf.partyEdit, nil, sf.editActor); !errors.Is(err, ErrDenied) {
		t.Fatalf("ForImportFrom must not bypass RBAC: want ErrDenied, got %v", err)
	}
}

// The REAL import wiring (blocker-1 regression pin): sqlsource.
// CommitRowsUpserting with the guarded engine for records — as
// internal/api's import route passes it — and the raw engine for
// identity rows, under a read_only SystemOfRecord declaration. With
// ForImportFrom(source) the keyed re-import UPDATES the record it
// created last run; the very same call WITHOUT ForImportFrom is blocked
// per-row by the SoR check — which is exactly the bug this pins: the
// importer does NOT use the raw engine for records, so an unscoped
// guarded engine would freeze the mirror at its first snapshot.
func TestCommitRowsUpserting_RealImportWiring(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	source := f.create("ExternalSQLSource", map[string]any{
		"name": "NAV mirror", "driver": "postgres", "host": "h", "database": "nav",
	})
	f.create("SystemOfRecord", map[string]any{
		"entity_type": "UnitOfMeasure", "source_id": source.ID, "mode": "read_only",
	})

	headers := []string{"Code", "Name"}
	mapping := csvimport.ColumnMapping{"Code": "code", "Name": "name"}
	relation := "dbo.Unit of Measure"
	uomDef := f.def("UnitOfMeasure")
	identityDef := f.def("ExternalIdentity")
	actor := audit.Actor{Type: audit.ActorHuman, ID: "user-importer"}
	importEngine := func() *GuardedEngine { // fresh per "request", like the real route
		return Guard(f.engine, humanResolver(f, "user-importer")).ForImportFrom(source.ID)
	}

	// Run 1: creates the record and its identity row.
	res, err := sqlsource.CommitRowsUpserting(ctx, headers, [][]string{{"ea", "Each"}},
		uomDef, mapping, "Code", source.ID, relation, importEngine(), f.engine, identityDef, actor)
	if err != nil {
		t.Fatalf("first import run: %v", err)
	}
	if res[0].Err != nil || res[0].RecordID == "" || res[0].Updated {
		t.Fatalf("first run should create: %+v (err %v)", res[0], res[0].Err)
	}
	recordID := res[0].RecordID

	// Run 2: the keyed re-import must UPDATE the governed record.
	res, err = sqlsource.CommitRowsUpserting(ctx, headers, [][]string{{"ea", "Each (refreshed)"}},
		uomDef, mapping, "Code", source.ID, relation, importEngine(), f.engine, identityDef, actor)
	if err != nil {
		t.Fatalf("re-import run: %v", err)
	}
	if res[0].Err != nil {
		t.Fatalf("re-import of a read_only type must succeed through ForImportFrom, got row error: %v", res[0].Err)
	}
	if !res[0].Updated || res[0].RecordID != recordID {
		t.Fatalf("re-import should update the same record: %+v", res[0])
	}
	rec, err := f.engine.Get(ctx, uomDef, recordID)
	if err != nil {
		t.Fatalf("re-read record: %v", err)
	}
	if rec.Data["name"] != "Each (refreshed)" {
		t.Fatalf("re-import did not land: name = %v", rec.Data["name"])
	}

	// The same call WITHOUT ForImportFrom: blocked per-row.
	res, err = sqlsource.CommitRowsUpserting(ctx, headers, [][]string{{"ea", "Each (frozen)"}},
		uomDef, mapping, "Code", source.ID, relation,
		Guard(f.engine, humanResolver(f, "user-importer")), f.engine, identityDef, actor)
	if err != nil {
		t.Fatalf("unscoped import run should fail per-row, not wholesale: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSystemOfRecordReadOnly) {
		t.Fatalf("unscoped guarded engine must hit the SoR block per-row: got %v", res[0].Err)
	}
	rec, err = f.engine.Get(ctx, uomDef, recordID)
	if err != nil {
		t.Fatalf("re-read record after blocked run: %v", err)
	}
	if rec.Data["name"] != "Each (refreshed)" {
		t.Fatalf("blocked run still changed the record: name = %v", rec.Data["name"])
	}
}
