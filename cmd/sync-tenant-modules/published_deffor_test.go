// Regression tests for uc-infra#159: syncTenant's real-run defFor closure
// (now extracted as publishedDefFor) collapsed a genuine GetPublished
// error and a json.Unmarshal failure into the same silent (nil, false)
// that "genuinely not published" would also produce — and every one of
// its five consumers (requiredFieldWarnings, targetConstraintWarnings,
// numericBoundWarnings, uniqueConstraintWarnings,
// backfillNewUniqueConstraints) treats !ok as "nothing to check here" and
// `continue`s without a trace. That was already correct behavior for
// "not published" (which can't actually happen at this call site — see
// publishedDefFor's own doc comment on why every entityType it's called
// with is already known-published); it was silent data loss for a real
// error. #116/#158 fixed the same shape of bug one level down, at each
// warnings function's own Count* call; this is the wider gap those two
// issues' own follow-up named: the shared defFor closure itself.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestPublishedDefFor_GetPublishedErrorIsSurfaced(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	defFor := publishedDefFor(context.Background(), entityDefs, "Demo Organization", "tenant-abc")

	var def *entity.Definition
	var ok bool
	logged := captureLog(t, func() {
		def, ok = defFor("Widget")
	})

	if ok || def != nil {
		t.Errorf("expected (nil, false) on a GetPublished error, got (%v, %v)", def, ok)
	}
	for _, want := range []string{
		"WARNING", "Demo Organization", "tenant-abc", "Widget",
		"could not re-read the just-published definition",
		"database is closed",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

func TestPublishedDefFor_UnmarshalErrorIsSurfaced(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	id, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	// Valid JSON, and valid enough to pass createDraft's own generic
	// map[string]any unmarshal (used only to build the audit diff) — but
	// "fields" as a string instead of an array fails the strict,
	// typed json.Unmarshal into entity.Definition that publishedDefFor
	// performs. This reaches the exact failure mode a corrupt/unexpected
	// stored definition would, through the real repository API rather
	// than a raw SQL row-poke.
	bad := []byte(`{"entity_type":"Widget","version":1,"fields":"not-an-array"}`)
	if _, err := entityDefs.CreateDraft(ctx, "Widget", 1, bad, setupActor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := entityDefs.Approve(ctx, "Widget", 1, setupActor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := entityDefs.Publish(ctx, "Widget", 1, setupActor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	defFor := publishedDefFor(ctx, entityDefs, name, id)
	var def *entity.Definition
	var ok bool
	logged := captureLog(t, func() {
		def, ok = defFor("Widget")
	})

	if ok || def != nil {
		t.Errorf("expected (nil, false) on an unmarshal error, got (%v, %v)", def, ok)
	}
	for _, want := range []string{
		"WARNING", name, id, "Widget",
		"could not be decoded",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

// TestPublishedDefFor_RepeatedFailureForSameEntityTypeWarnsOnce is a
// regression test for independent review's finding on the first version
// of this fix: the same entityType reaches publishedDefFor once per
// caller (up to five times per syncTenant call — requiredFieldWarnings,
// targetConstraintWarnings, numericBoundWarnings, uniqueConstraintWarnings,
// backfillNewUniqueConstraints), and a persistently-failing lookup
// (dead connection, a stored definition that never becomes valid) would
// otherwise log the same multi-line WARNING once per caller — up to five
// near-identical paragraphs for one entity type, burying the signal this
// fix exists to surface. publishedDefFor dedupes by entity type within
// one closure (one syncTenant call) so a persistent failure still costs
// exactly one WARNING line, however many times it's actually called.
func TestPublishedDefFor_RepeatedFailureForSameEntityTypeWarnsOnce(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	defFor := publishedDefFor(context.Background(), entityDefs, "Demo Organization", "tenant-abc")

	logged := captureLog(t, func() {
		// Five calls, matching the real five-caller shape this closure is
		// actually shared across in syncTenant.
		for i := 0; i < 5; i++ {
			if _, ok := defFor("Widget"); ok {
				t.Fatalf("call %d: expected ok=false against a closed DB", i)
			}
		}
	})

	if n := strings.Count(logged, "WARNING"); n != 1 {
		t.Errorf("expected exactly one WARNING for five failing calls against the same entity type, got %d in: %q", n, logged)
	}
	if !strings.Contains(logged, "Widget") {
		t.Errorf("expected the single WARNING to name Widget, got: %q", logged)
	}
}

// TestPublishedDefFor_DifferentEntityTypesEachWarnOnce guards the other
// half of the dedup: it is keyed by entity type, not a single global
// latch — a second, DIFFERENT entity type failing must still be reported,
// not silently absorbed by the first type's dedup entry.
func TestPublishedDefFor_DifferentEntityTypesEachWarnOnce(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	defFor := publishedDefFor(context.Background(), entityDefs, "Demo Organization", "tenant-abc")

	logged := captureLog(t, func() {
		defFor("Widget")
		defFor("Widget") // repeat: must not add a second WARNING
		defFor("Gadget") // different type: must warn independently
	})

	if n := strings.Count(logged, "WARNING"); n != 2 {
		t.Errorf("expected one WARNING per distinct entity type (2 total), got %d in: %q", n, logged)
	}
	for _, want := range []string{"Widget", "Gadget"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected a WARNING naming %q, got: %q", want, logged)
		}
	}
}

func TestPublishedDefFor_SuccessStaysSilent(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	id, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	good := []byte(`{"entity_type":"Widget","version":1,"fields":[{"name":"sku","type":"string"}]}`)
	if _, err := entityDefs.CreateDraft(ctx, "Widget", 1, good, setupActor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := entityDefs.Approve(ctx, "Widget", 1, setupActor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := entityDefs.Publish(ctx, "Widget", 1, setupActor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	defFor := publishedDefFor(ctx, entityDefs, name, id)
	var def *entity.Definition
	var ok bool
	logged := captureLog(t, func() {
		def, ok = defFor("Widget")
	})

	if !ok || def == nil {
		t.Fatalf("expected a successful lookup, got (%v, %v)", def, ok)
	}
	if def.EntityType != "Widget" || len(def.Fields) != 1 || def.Fields[0].Name != "sku" {
		t.Errorf("expected the decoded Widget definition with its sku field, got %+v", def)
	}
	if logged != "" {
		t.Errorf("a successful lookup must stay silent, got log output: %q", logged)
	}
}
