package crm

import (
	"context"
	"database/sql"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// The three pipeline entities must each be labellable by the reference
// picker. Employee shipped without that and rendered as a raw UUID
// everywhere, unsearchable past the picker's result cap (#16) — these
// carry a `name` field, which the kernel's default name/title
// convention picks up, so they need no explicit LabelField. This test
// pins that: rename `name` and it fails here rather than in a picker.
func TestPipelineEntitiesAreLabellable(t *testing.T) {
	for _, def := range []*entity.Definition{Campaign(), Lead(), Opportunity()} {
		f, ok := def.FieldByName("name")
		if !ok {
			t.Errorf("%s has no `name` field and declares LabelField=%q; it would render as a raw UUID in every picker",
				def.EntityType, def.LabelField)
			continue
		}
		if f.Type != entity.FieldString || !f.Required {
			t.Errorf("%s.name is %s (required=%v); a label must be a required string", def.EntityType, f.Type, f.Required)
		}
	}
}

// Lead is pre-Party by design (ADR-0014): its contact details are free
// text because creating a Party for every unqualified enquiry is the
// master-data pollution the Party pattern exists to prevent. If someone
// later "tidies" these into references, the conversion story breaks
// silently — converted_party_id becomes meaningless.
func TestLeadContactDetailsAreFreeTextNotReferences(t *testing.T) {
	def := Lead()
	for _, name := range []string{"name", "company_name", "email", "phone"} {
		f, ok := def.FieldByName(name)
		if !ok {
			t.Fatalf("Lead has no %s field", name)
		}
		if f.Type != entity.FieldString {
			t.Errorf("Lead.%s is %s, want string: a lead exists before its Party does (ADR-0014)", name, f.Type)
		}
	}
	if f, ok := def.FieldByName("converted_party_id"); !ok || f.Target != "Party" {
		t.Errorf("Lead.converted_party_id must reference Party; got ok=%v target=%q", ok, f.Target)
	}
}

// Campaign's leads section is a related list, not a composition. Get
// this wrong and deleting a campaign would take its leads with it —
// the R11a distinction the Architect step is required to make
// explicitly.
func TestCampaignLeadsIsARelatedListNotComposition(t *testing.T) {
	var found bool
	for _, r := range Campaign().Relationships {
		if r.Target != "Lead" {
			continue
		}
		found = true
		if r.Kind != entity.RelationRelatedList {
			t.Errorf("Campaign->Lead is %q, want %q: a lead outlives the campaign that sourced it",
				r.Kind, entity.RelationRelatedList)
		}
		if r.ParentField != "campaign_id" {
			t.Errorf("Campaign->Lead ParentField = %q, want campaign_id", r.ParentField)
		}
	}
	if !found {
		t.Error("Campaign declares no relationship to Lead")
	}
}

func publishPipeline(t *testing.T, tenantDB *sql.DB) (context.Context, audit.Actor, *crud.Engine) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"crm", Publish},
		{"crm statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	return ctx, actor, crud.NewEngine(tenantDB)
}

// statusIDs maps code -> Status record id for one status type.
func statusIDs(t *testing.T, ctx context.Context, tenantDB *sql.DB, engine *crud.Engine, statusTypeCode string) map[string]string {
	t.Helper()
	types, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", statusTypeCode)
	if err != nil || len(types) != 1 {
		t.Fatalf("%s StatusType: err=%v n=%d, want exactly 1", statusTypeCode, err, len(types))
	}
	rows, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	if err != nil {
		t.Fatalf("list %s statuses: %v", statusTypeCode, err)
	}
	out := map[string]string{}
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c != "" {
			out[c] = r.ID
		}
	}
	return out
}

func edgesFor(t *testing.T, ctx context.Context, tenantDB *sql.DB, engine *crud.Engine, statusTypeCode string) (map[string]string, func(a, b string) bool) {
	t.Helper()
	byCode := statusIDs(t, ctx, tenantDB, engine, statusTypeCode)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", statusTypeCode)
	trs, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusTransition"), "status_type_id", types[0].ID)
	if err != nil {
		t.Fatalf("list %s transitions: %v", statusTypeCode, err)
	}
	edges := map[string]bool{}
	for _, tr := range trs {
		from, _ := tr.Data["from_status_id"].(string)
		to, _ := tr.Data["to_status_id"].(string)
		edges[from+"->"+to] = true
	}
	return byCode, func(a, b string) bool { return edges[byCode[a]+"->"+byCode[b]] }
}

// All three new graphs seed, and — the part that has actually broken in
// this codebase before (purchasing/sales, 2026-07-29) — status codes
// are scoped by status_type_id. `new` means a fresh Case and a fresh
// Lead, and those must be two different Status rows.
func TestPublishStatuses_PipelineGraphsAreScopedAndComplete(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx, actor, engine := publishPipeline(t, tenantDB)

	wantSizes := map[string]int{"lead_status": 5, "opportunity_stage": 6, "campaign_status": 4, "case_status": 6}
	for code, want := range wantSizes {
		if got := len(statusIDs(t, ctx, tenantDB, engine, code)); got != want {
			t.Errorf("%s has %d statuses, want %d", code, got, want)
		}
	}

	caseNew := statusIDs(t, ctx, tenantDB, engine, "case_status")["new"]
	leadNew := statusIDs(t, ctx, tenantDB, engine, "lead_status")["new"]
	if caseNew == "" || leadNew == "" {
		t.Fatal("both case_status and lead_status must define a `new` status")
	}
	if caseNew == leadNew {
		t.Error("case_status.new and lead_status.new resolved to the same Status row — status codes must be scoped by status_type_id")
	}

	// Idempotent: re-seeding must not duplicate any of the three.
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("re-PublishStatuses: %v", err)
	}
	for code, want := range wantSizes {
		if got := len(statusIDs(t, ctx, tenantDB, engine, code)); got != want {
			t.Errorf("re-seed changed %s to %d statuses, want %d", code, got, want)
		}
	}
}

// The pipeline's shape, asserted as edges rather than as a walk — the
// walk is the next test. Both matter: this catches a missing edge even
// where no test happens to traverse it.
func TestPublishStatuses_PipelineEdges(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx, _, engine := publishPipeline(t, tenantDB)

	_, opp := edgesFor(t, ctx, tenantDB, engine, "opportunity_stage")
	if !opp("negotiation", "proposal") {
		t.Error("a customer asking for a revised quote must put the deal back in proposal, not kill it")
	}
	if !opp("proposal", "won") {
		t.Error("a customer can accept a proposal outright; won must be reachable without negotiation")
	}
	for _, live := range []string{"prospecting", "qualification", "proposal", "negotiation"} {
		if !opp(live, "lost") {
			t.Errorf("a deal must be losable from %s — a pipeline that cannot regress reports optimistically", live)
		}
	}
	if opp("won", "lost") || opp("lost", "won") {
		t.Error("won and lost are both terminal")
	}

	_, lead := edgesFor(t, ctx, tenantDB, engine, "lead_status")
	if !lead("qualified", "converted") {
		t.Error("a qualified lead must be convertible")
	}
	for _, live := range []string{"new", "contacted", "qualified"} {
		if !lead(live, "disqualified") {
			t.Errorf("a lead must be disqualifiable from %s", live)
		}
	}
	if lead("disqualified", "contacted") || lead("converted", "contacted") {
		t.Error("both lead exits are terminal; a returning prospect is a new lead, not a reopened one (see PublishStatuses)")
	}

	_, camp := edgesFor(t, ctx, tenantDB, engine, "campaign_status")
	if !camp("planned", "cancelled") || !camp("active", "cancelled") {
		t.Error("a campaign must be cancellable before and during its run — the fact its dates cannot express")
	}
	if camp("completed", "active") {
		t.Error("completed is terminal")
	}
}

// Drive a real Opportunity through the guarded engine, including the
// backward edge. Asserting which StatusTransition rows exist proves the
// seed; only this proves enforcement.
func TestOpportunity_PipelineThroughTheGuardedEngine(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx, actor, engine := publishPipeline(t, tenantDB)
	oppDef := publishedDef(t, tenantDB, "Opportunity")
	status := statusIDs(t, ctx, tenantDB, engine, "opportunity_stage")

	customer, err := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Demo Customer", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create customer Party: %v", err)
	}

	fields := func(code string) map[string]any {
		return map[string]any{
			"name": "Demo Deal", "customer_id": customer.ID, "amount": 1000.0,
			"probability": 50.0, "expected_close_date": "2026-12-01", "status_id": status[code],
		}
	}
	rec, err := engine.Create(ctx, oppDef, fields("prospecting"), actor)
	if err != nil {
		t.Fatalf("create Opportunity: %v", err)
	}
	version := rec.Version
	// Forward to negotiation, back to proposal when the customer asks
	// for a revised quote, forward again, and won.
	for _, to := range []string{"qualification", "proposal", "negotiation", "proposal", "negotiation", "won"} {
		if err := engine.ValidateStatusTransition(ctx, oppDef, rec.ID, fields(to), false, &version); err != nil {
			t.Fatalf("transition to %s should be legal: %v", to, err)
		}
		v, err := engine.Update(ctx, oppDef, rec.ID, fields(to), &version, actor)
		if err != nil {
			t.Fatalf("update to %s: %v", to, err)
		}
		version = v
	}
	for _, to := range []string{"negotiation", "lost"} {
		if err := engine.ValidateStatusTransition(ctx, oppDef, rec.ID, fields(to), false, &version); err == nil {
			t.Errorf("won -> %s must be rejected; won is terminal", to)
		}
	}

	// And a deal that dies in prospecting, before anyone qualified it.
	dead, err := engine.Create(ctx, oppDef, fields("prospecting"), actor)
	if err != nil {
		t.Fatalf("create second Opportunity: %v", err)
	}
	deadVersion := dead.Version
	if err := engine.ValidateStatusTransition(ctx, oppDef, dead.ID, fields("lost"), false, &deadVersion); err != nil {
		t.Errorf("prospecting -> lost must be legal: %v", err)
	}
}

// A lead walks to converted, and converted is the end of the road.
func TestLead_ConversionIsTerminal(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx, actor, engine := publishPipeline(t, tenantDB)
	leadDef := publishedDef(t, tenantDB, "Lead")
	status := statusIDs(t, ctx, tenantDB, engine, "lead_status")

	fields := func(code string) map[string]any {
		return map[string]any{"name": "Demo Prospect", "source": "web", "status_id": status[code]}
	}
	rec, err := engine.Create(ctx, leadDef, fields("new"), actor)
	if err != nil {
		t.Fatalf("create Lead: %v", err)
	}
	version := rec.Version
	for _, to := range []string{"contacted", "qualified", "converted"} {
		if err := engine.ValidateStatusTransition(ctx, leadDef, rec.ID, fields(to), false, &version); err != nil {
			t.Fatalf("transition to %s should be legal: %v", to, err)
		}
		v, err := engine.Update(ctx, leadDef, rec.ID, fields(to), &version, actor)
		if err != nil {
			t.Fatalf("update to %s: %v", to, err)
		}
		version = v
	}
	for _, to := range []string{"contacted", "qualified", "disqualified"} {
		if err := engine.ValidateStatusTransition(ctx, leadDef, rec.ID, fields(to), false, &version); err == nil {
			t.Errorf("converted -> %s must be rejected; a returning prospect is a new lead", to)
		}
	}
}

// The one cross-field rule Campaign can express, enforced on update as
// well as create — the asymmetry #15's review had to check by hand.
func TestCampaign_EndDateMustNotPrecedeStart(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx, actor, engine := publishPipeline(t, tenantDB)
	campDef := publishedDef(t, tenantDB, "Campaign")
	status := statusIDs(t, ctx, tenantDB, engine, "campaign_status")

	base := func(end string) map[string]any {
		return map[string]any{
			"name": "Demo Campaign", "channel": "email", "budget": 100.0,
			"start_date": "2026-09-01", "end_date": end, "status_id": status["planned"],
		}
	}
	if _, err := engine.Create(ctx, campDef, base("2026-08-31"), actor); err == nil {
		t.Error("a campaign ending before it starts must be rejected on create")
	}
	// The boundary is inclusive: a one-day campaign is legal.
	sameDay, err := engine.Create(ctx, campDef, base("2026-09-01"), actor)
	if err != nil {
		t.Fatalf("a single-day campaign must be accepted: %v", err)
	}
	version := sameDay.Version
	if _, err := engine.Update(ctx, campDef, sameDay.ID, base("2026-08-01"), &version, actor); err == nil {
		t.Error("end_date must be enforced on update too, not only on create")
	}
}

// Every enum this card adds rejects a value outside its declared set —
// the cheapest guarantee the kernel makes, and the one a typo in a
// Definition silently removes.
func TestPipelineEnumsRejectUndeclaredValues(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx, actor, engine := publishPipeline(t, tenantDB)
	campStatus := statusIDs(t, ctx, tenantDB, engine, "campaign_status")
	leadStatus := statusIDs(t, ctx, tenantDB, engine, "lead_status")

	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "Campaign"), map[string]any{
		"name": "Bad Channel", "channel": "carrier_pigeon",
		"start_date": "2026-09-01", "status_id": campStatus["planned"],
	}, actor); err == nil {
		t.Error("Campaign.channel must reject a value outside its enum")
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "Lead"), map[string]any{
		"name": "Bad Source", "source": "telepathy", "status_id": leadStatus["new"],
	}, actor); err == nil {
		t.Error("Lead.source must reject a value outside its enum")
	}
}

// ADR-0014's shape, driven end to end through the guarded engine: a
// person Party, the `contact` PartyRole, and a contact_for
// PartyRelationship running person -> organization. The direction is
// the assertion that matters — the kernel cannot enforce it, so a
// reversed row is indistinguishable from a correct one by inspection.
func TestContactForShapeAndDirection(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	partyDef := publishedDef(t, tenantDB, "Party")

	org, err := engine.Create(ctx, partyDef, map[string]any{
		"party_type": "organization", "name": "Demo Organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	person, err := engine.Create(ctx, partyDef, map[string]any{
		"party_type": "person", "name": "Demo Contact", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "PartyRole"), map[string]any{
		"party_id": person.ID, "role_type": "contact",
	}, actor); err != nil {
		t.Fatalf("the `contact` PartyRole must be accepted: %v", err)
	}

	relDef := publishedDef(t, tenantDB, "PartyRelationship")
	rel, err := engine.Create(ctx, relDef, map[string]any{
		"party_id_from": person.ID, "party_id_to": org.ID, "relationship_type": "contact_for",
	}, actor)
	if err != nil {
		t.Fatalf("contact_for must be a legal relationship_type (PartyRelationship v3, ADR-0014): %v", err)
	}
	if got, _ := rel.Data["party_id_from"].(string); got != person.ID {
		t.Errorf("contact_for runs person -> organization; party_id_from = %q, want the person %q", got, person.ID)
	}
	if got, _ := rel.Data["party_id_to"].(string); got != org.ID {
		t.Errorf("contact_for runs person -> organization; party_id_to = %q, want the organization %q", got, org.ID)
	}

	if _, err := engine.Create(ctx, relDef, map[string]any{
		"party_id_from": person.ID, "party_id_to": org.ID, "relationship_type": "acquainted_with",
	}, actor); err == nil {
		t.Error("relationship_type must still reject values outside its enum")
	}

	// v3 is what gets published, and the older values survive the bump —
	// this is an additive enum change, not a replacement.
	def := publishedDef(t, tenantDB, "PartyRelationship")
	if def.Version != 3 {
		t.Errorf("published PartyRelationship is v%d, want v3", def.Version)
	}
	f, _ := def.FieldByName("relationship_type")
	for _, want := range []string{"employs", "supplies", "parent_of", "contact_for"} {
		if !containsString(f.EnumValues, want) {
			t.Errorf("relationship_type lost %q at v3; the bump must be additive", want)
		}
	}
	// The direction convention is a documentation obligation ADR-0014
	// creates. Keep the doc comment honest about every value it lists.
	if n := len(f.EnumValues); n != 4 {
		t.Errorf("relationship_type has %d values; PartyRelationship's doc comment documents the direction of 4 — update both together", n)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// The i18n surface of everything this card touched: every field and
// every enum value needs a catalog key. internal/i18n's
// TestLocales_HaveIdenticalKeySets proves the four locales agree with
// each other; this proves they agree with the Definitions, which is the
// half that silently rots when a field is added later.
//
// Two things this test learned the hard way, both from the independent
// review:
//
// It checks the fallback locale ONLY, deliberately. An earlier version
// looped over catalog.Available() and reported per-locale, which reads
// well and is inert: Catalog.T falls back through the base language to
// `en`, so a key present in English never reports missing for ar/tr/fa.
// Deleting a Farsi key left it passing. Parity between locales is the
// other test's job; this one's job is Definitions-vs-catalog, and `en`
// is the honest place to check that.
//
// It covers the FOUNDATION definitions this card changed, not just the
// crm ones. Scoping it to crm is what let PartyRelationship's new
// `contact_for` value ship with no key in any locale — an untranslated
// latin code sitting in a dropdown between three translated labels,
// invisible to both this test and the parity test, on the single field
// ADR-0014 exists to add.
func TestPipelineI18nKeysExistForEveryFieldAndEnumValue(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	want := func(key string) {
		t.Helper()
		if catalog.T(catalog.Fallback(), key) == key {
			t.Errorf("missing i18n key %s", key)
		}
	}
	defs := []*entity.Definition{Campaign(), Lead(), Opportunity()}
	// The foundation entity this card bumped to v3. Its enum values are
	// rendered by the same TOrDefault path as any other, and its form is
	// a reachable screen.
	defs = append(defs, foundation.PartyRelationship())
	for _, def := range defs {
		want("entity." + def.EntityType + ".name")
		for _, f := range def.Fields {
			key := "field." + def.EntityType + "." + f.Name
			want(key)
			for _, v := range f.EnumValues {
				want(key + "." + v)
			}
		}
	}
	// Status codes are tenant data, not enum values on the Definition,
	// so they are keyed off status_id and listed explicitly.
	for entityType, codes := range map[string][]string{
		"Campaign":    {"planned", "active", "completed", "cancelled"},
		"Lead":        {"new", "contacted", "qualified", "converted", "disqualified"},
		"Opportunity": {"prospecting", "qualification", "proposal", "negotiation", "won", "lost"},
	} {
		for _, c := range codes {
			want("field." + entityType + ".status_id." + c)
		}
	}
}
