package modules

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/modulebundle"
	"github.com/universaltill/universal-core/internal/testexec"
)

// TestPublishers_MatchReservedModules is the parity test
// modulebundle.ReservedModules' own comment once claimed existed and
// did not. Without it the two lists drifted by three modules, and an
// independent review showed the consequence: a bundle declaring an
// unlisted built-in key installs its own Definition, after which the
// real module's Publish silently no-ops and reports success.
//
// It lives here rather than in cmd/provision-tenant because the map it
// guards moved here — and it stays a *test* rather than becoming
// structural because modulebundle is kernel code that must not depend
// on this composition-root package, and because this very file imports
// modulebundle, so the reverse direction would cycle the test binary
// (ADR-0017 §4).
func TestPublishers_MatchReservedModules(t *testing.T) {
	// foundation is always published and has no Publishers entry, so it
	// is the one legitimate difference.
	want := map[string]bool{FoundationKey: true}
	for key := range Publishers {
		want[key] = true
	}
	for key := range want {
		if !modulebundle.ReservedModules[key] {
			t.Errorf("built-in module %q is missing from modulebundle.ReservedModules — a bundle could claim that key and silently pre-empt the real module", key)
		}
	}
	for key := range modulebundle.ReservedModules {
		if !want[key] {
			t.Errorf("modulebundle.ReservedModules has %q, which is not a built-in module — bundles are needlessly barred from that key", key)
		}
	}
}

// Every module must supply all three of Publish, PublishForms and
// Definitions. PublishStatuses is legitimately nil (finance has no
// status-managed entity), but the other three are not optional, and a
// nil Definitions would make cmd/sync-tenant-modules' dry run silently
// report "already current" for that module — a wrong answer that looks
// like a right one.
func TestPublishers_AreFullyPopulated(t *testing.T) {
	for key, p := range Publishers {
		if p.Publish == nil {
			t.Errorf("module %q has no Publish", key)
		}
		if p.PublishForms == nil {
			t.Errorf("module %q has no PublishForms", key)
		}
		if p.Definitions == nil {
			t.Errorf("module %q has no Definitions — sync's dry run would report it as already current", key)
		}
	}
}

// Definitions must actually return this module's own entities. A
// copy-paste slip in the map literal (crm pointing at hr.All) is
// invisible to every other test here, and would make the dry run
// confidently wrong.
func TestPublishers_DefinitionsBelongToTheirModule(t *testing.T) {
	for key, p := range Publishers {
		defs := p.Definitions()
		if len(defs) == 0 {
			t.Errorf("module %q returned no Definitions", key)
			continue
		}
		for _, d := range defs {
			if d.Module != key {
				t.Errorf("module %q's Definitions include %s, which declares module %q — the map entry points at the wrong package's All()",
					key, d.EntityType, d.Module)
			}
		}
	}
}

func TestKeys_AreSortedAndComplete(t *testing.T) {
	keys := Keys()
	if len(keys) != len(Publishers) {
		t.Fatalf("Keys() returned %d keys, want %d", len(keys), len(Publishers))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Errorf("Keys() is not sorted: %q before %q", keys[i-1], keys[i])
		}
	}
	for _, k := range keys {
		if _, ok := Publishers[k]; !ok {
			t.Errorf("Keys() returned %q, which is not a Publishers key", k)
		}
	}
	if _, ok := Publishers[FoundationKey]; ok {
		t.Error("foundation must not be in Publishers — it is unconditional, not opt-in (ADR-0001 §8)")
	}
}

// TestSortStrings is the direct test for sortStrings that
// TestKeys_AreSortedAndComplete only exercises indirectly, over
// whatever order Go's randomized map iteration happens to hand it for
// the fixed 7-entry Publishers map — not the empty/single-element/
// duplicate inputs a hand-rolled insertion sort actually needs covering
// (found by uc-infra#111's independent review).
func TestSortStrings(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{}, []string{}},
		{"single", []string{"a"}, []string{"a"}},
		{"already sorted", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"reversed", []string{"c", "b", "a"}, []string{"a", "b", "c"}},
		{"duplicates", []string{"b", "a", "b", "a"}, []string{"a", "a", "b", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]string, len(tc.in))
			copy(got, tc.in)
			sortStrings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sortStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// freshTenantDB creates a brand-new tenant database with the real tenant
// schema applied, the same pattern internal/kernel/finance/seed_test.go
// uses. The helper itself is new (this file predates testexec), so it
// calls testexec directly rather than duplicating finance's own
// pre-testexec copy of the same three steps.
func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := testexec.FreshDatabase(t, "uc_test_modules")
	tenantDB := testexec.Open(t, dsn)
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "modules-test"}
}

// TestFoundationDefinitions_MatchesFoundationAll is the direct
// regression test uc-infra#111 found missing: FoundationDefinitions is a
// one-line wrapper (`return foundation.All()`) that nothing in this
// package's own test suite called, so a future edit that broke the
// wrapper — pointing it at the wrong package, or dropping the call
// entirely — would pass every other test here silently.
//
// reflect.DeepEqual is safe and order-sensitive here: entity.Definition
// holds no func fields, and foundation.All() builds a fresh slice of
// fresh values on every call (Party(), PartyRole(), ... — see
// foundation.go), so this compares real structural equality rather than
// pointer identity. An earlier version of this test compared only
// EntityType/Version/Module through a map, which could not tell a
// FoundationDefinitions() with a duplicated/dropped EntityType, or one
// returning field-stripped copies, from a correct one.
func TestFoundationDefinitions_MatchesFoundationAll(t *testing.T) {
	got := FoundationDefinitions()
	if len(got) == 0 {
		t.Fatal("FoundationDefinitions() returned no Definitions — test would pass vacuously")
	}
	if !reflect.DeepEqual(got, foundation.All()) {
		t.Errorf("FoundationDefinitions() does not match foundation.All()")
	}
	for _, d := range got {
		if d.Module != FoundationKey {
			t.Errorf("%s: expected Module %q, got %q", d.EntityType, FoundationKey, d.Module)
		}
	}
}

// TestPublishFoundation_PublishesEveryDefinitionAndForm is the direct
// test for the other under-tested sibling: PublishFoundation is
// exercised transitively by cmd/sync-tenant-modules and
// cmd/provision-tenant's own tests, but nothing in this package calls it
// directly, so this package's own coverage report never reflected that
// it actually works — see uc-infra#111.
func TestPublishFoundation_PublishesEveryDefinitionAndForm(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if err := PublishFoundation(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("PublishFoundation: %v", err)
	}

	defRepo := data.NewEntityDefinitionRepo(tenantDB)
	for _, d := range FoundationDefinitions() {
		v, err := defRepo.GetPublished(ctx, d.EntityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", d.EntityType, err)
		}
		if v.Version != d.Version {
			t.Fatalf("%s: expected published version %d, got %d", d.EntityType, d.Version, v.Version)
		}
	}

	formRepo := data.NewFormDefinitionRepo(tenantDB)
	forms := foundation.AllForms()
	if len(forms) == 0 {
		t.Fatal("foundation.AllForms() returned no Definitions — test would pass vacuously")
	}
	for _, f := range forms {
		v, err := formRepo.GetPublished(ctx, f.EntityType)
		if err != nil {
			t.Fatalf("GetPublished form(%s): %v", f.EntityType, err)
		}
		if v.Version != f.Version {
			t.Fatalf("%s: expected published form version %d, got %d", f.EntityType, f.Version, v.Version)
		}
	}
}

// TestPublishFoundation_IsIdempotent mirrors every other module's own
// Publish/PublishForms idempotency test (e.g.
// internal/kernel/finance/seed_test.go's TestPublish_IsIdempotent):
// PublishFoundation is called on every cmd/sync-tenant-modules run and
// cmd/provision-tenant run, so a second call against an
// already-published tenant must be a safe no-op, not an error.
func TestPublishFoundation_IsIdempotent(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if err := PublishFoundation(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("first PublishFoundation: %v", err)
	}
	if err := PublishFoundation(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("second PublishFoundation should be a no-op, got: %v", err)
	}
}

// TestPublishFoundation_StopsAtFirstError is the direct test for the
// one thing that makes PublishFoundation more than an alias: it calls
// foundation.Publish, then foundation.PublishForms, and returns early on
// the first error rather than running both unconditionally. Without this
// test, that ordering had no coverage of its own in this package — the
// two tests above only ever exercise the succeed/succeed path (found by
// uc-infra#111's independent review).
//
// Publishing entities is left to fail by giving PublishFoundation a
// tenant database with no schema applied at all — entity_definitions
// does not exist, so foundation.Publish errors immediately. form_definitions
// doesn't exist either in that state, so a plain "did PublishForms also
// error" assertion wouldn't distinguish "PublishForms ran and failed"
// from "PublishForms never ran" — the actual short-circuit contract this
// test exists to pin down. Applying the real tenant schema and then
// dropping only entity_definitions isolates that: form_definitions stays
// queryable, so this test can assert it never received a foundation form.
func TestPublishFoundation_StopsAtFirstError(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if _, err := tenantDB.ExecContext(ctx, `DROP TABLE entity_definitions`); err != nil {
		t.Fatalf("drop entity_definitions: %v", err)
	}

	err := PublishFoundation(ctx, tenantDB, humanActor())
	if err == nil {
		t.Fatal("PublishFoundation: expected an error with entity_definitions missing, got nil")
	}

	forms := foundation.AllForms()
	if len(forms) == 0 {
		t.Fatal("foundation.AllForms() returned no Definitions — test would pass vacuously")
	}
	formRepo := data.NewFormDefinitionRepo(tenantDB)
	if _, err := formRepo.GetPublished(ctx, forms[0].EntityType); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("GetPublished(%s) after a failed Publish: expected %v (PublishForms should never have run), got %v",
			forms[0].EntityType, data.ErrNotFound, err)
	}
}
