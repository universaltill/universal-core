package modules

import (
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/modulebundle"
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
	if _, ok := Publishers[FoundationKey]; ok {
		t.Error("foundation must not be in Publishers — it is unconditional, not opt-in (ADR-0001 §8)")
	}
}
