// Command sync-tenant-modules brings already-provisioned tenants up to
// the current code's entity/form Definitions and status graphs.
//
// The gap it closes (#70, ADR-0017): every module's Publish/
// PublishForms/PublishStatuses is idempotent and safe to re-run, but
// nothing re-runs them. A tenant provisioned before an entity shipped
// simply does not have it, however new the image serving it is.
// Measured against the live demo tenant on 2026-08-01, that meant
// InventoryItem v2 with no Facility, PurchaseOrder v5, and the whole
// RFQ family missing — while the deployment in front of it was already
// running the newest code.
//
// **Which modules a tenant has is inferred from its own registry**, not
// stored anywhere: entity.Definition carries a `module` key and
// PublishAll stores the whole Definition, so
// `SELECT DISTINCT definition->>'module'` is the answer, and it cannot
// go stale the way a `tenants.modules` column could. See ADR-0017 for
// why that is preferred over adding one.
//
// **It never grants a module.** Only modules a tenant already has are
// re-published. Granting one is a licensing decision with billing
// consequences and stays an explicit cmd/provision-tenant call —
// otherwise a routine sync would silently enable CRM for every tenant
// the moment the code gained a CRM package.
//
// **It reports the data migrations it just made necessary**, which is
// the part most easily left as a footnote. Publishing a version that
// adds a Required field leaves existing records readable but
// un-editable; the live tenant had ten such InventoryItem rows the
// moment v3 published. Those are counted and named, with a pointer to
// run the matching backfill. It does not attempt the migration itself:
// backfills are entity-specific and judgement-laden (see
// cmd/backfill-inventory-facility, which has to invent a facility and
// assert all prior stock was at one location), and a generic tool
// guessing at that would be far worse than a loud pointer.
//
// **One narrow, deliberate exception** (uc-infra#81):
// backfillNewUniqueConstraints DOES write something on a real run — but
// only to record_unique_keys, the Unique-constraint mechanism's own
// bookkeeping table, never to records.data itself. Populating it
// requires no entity-specific judgement (unlike inventing a
// facility_id): it is a deterministic function of data already on the
// record. Without it, a Unique constraint added to an entity type that
// already has data would protect nothing already there — a fresh
// duplicate against an un-keyed pre-existing record would be silently
// accepted — so "report, never write" would leave the constraint it just
// warned about only half-real. Records that already collide with each
// other are still left for a human (see uniqueConstraintWarnings).
//
// **Dry-run limitation, stated so it is not mistaken for a guarantee:**
// the preview compares ENTITY Definitions only. Form Definitions and
// status graphs are published by a real run but do not appear in the
// dry-run diff, because reporting them would mean either publishing
// them to find out or duplicating each module's form list here — and a
// second list to keep in sync is the thing ADR-0017 §4 is about. So a
// dry run reporting "already current" means the entity registry is
// current; a real run may still publish a changed form. It is a drift
// report, not a proof of no-op.
//
// DATABASE_URL must point at the CONTROL-PLANE database — unlike the
// backfill commands, which take a single tenant's own database. Tenant
// databases are reached through internal/tenantdb's router, same as the
// server does.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/modules"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

func main() {
	controlDBURL := os.Getenv("DATABASE_URL")
	if controlDBURL == "" {
		log.Fatal("DATABASE_URL is required (the control-plane database — see this file's own doc comment)")
	}
	actorID := flag.String("actor-id", "", "audit actor id for every Definition this publishes (required)")
	// An unattended pipeline sync run is an ai_agent actor, and
	// ADR-0001 §14 makes that distinction first-class — hard-coding
	// ActorHuman would write a falsified actor_type onto every
	// Definition this command re-publishes (uc-infra#72/#123, same
	// shape as cmd/provision-tenant's fix).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	only := flag.String("tenant-id", "", "sync just this tenant instead of every provisioned one")
	dryRun := flag.Bool("dry-run", false, "report what would change without publishing anything")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// control-plane connection and every tenant's publish work already
	// ran (same discipline as cmd/provision-tenant).
	actor := audit.Actor{Type: audit.ActorType(*actorType), ID: *actorID, ModelVersion: *modelVersion}
	switch actor.Type {
	case audit.ActorHuman, audit.ActorAgent:
	default:
		log.Fatalf("invalid actor: -actor-type must be %q or %q, got %q", audit.ActorHuman, audit.ActorAgent, *actorType)
	}
	// A human actor carrying a model version is the same class of
	// falsified audit metadata this fix exists to prevent the other
	// way around (uc-infra#72 independent review) — Validate() alone
	// only rejects an EMPTY ModelVersion on an agent, never a populated
	// one on a human, so that half of the mistake needs its own check.
	if actor.Type == audit.ActorHuman && *modelVersion != "" {
		log.Fatalf("invalid actor: -model-version is only meaningful when -actor-type is %q", audit.ActorAgent)
	}
	if err := actor.Validate(); err != nil {
		log.Fatalf("invalid actor: %v", err)
	}

	controlDB, err := sql.Open("pgx", controlDBURL)
	if err != nil {
		log.Fatalf("open control-plane database: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.Ping(); err != nil {
		log.Fatalf("ping control-plane database: %v", err)
	}

	ctx := context.Background()
	tenants := data.NewTenantRepo(controlDB)

	ids := []string{*only}
	if *only == "" {
		ids, err = tenants.ListIDs(ctx)
		if err != nil {
			log.Fatalf("list tenants: %v", err)
		}
	}
	if len(ids) == 0 {
		fmt.Println("No tenants provisioned; nothing to sync.")
		return
	}

	router, err := tenantdb.NewRouter(controlDB, controlDBURL)
	if err != nil {
		log.Fatalf("build tenant router: %v", err)
	}
	defer router.Close()

	names, err := tenants.NamesByIDs(ctx, ids)
	if err != nil {
		log.Fatalf("look up tenant names: %v", err)
	}

	var synced, partial, skipped int
	var warnings []string
	for _, id := range ids {
		name := names[id]
		if name == "" {
			name = "(unnamed)"
		}
		w, outcome := syncTenant(ctx, router, id, name, actor, *dryRun)
		switch outcome {
		case outcomeSkipped:
			skipped++
			continue
		case outcomePartial:
			partial++
		default:
			synced++
		}
		warnings = append(warnings, w...)
	}

	verb := "Synced"
	if *dryRun {
		verb = "Would sync"
	}
	fmt.Printf("%s %d tenant(s); %d partially synced; %d skipped.\n", verb, synced, partial, skipped)

	// A mistyped -tenant-id is otherwise indistinguishable from a
	// successful run: router.Get fails, the tenant is logged as skipped,
	// and the process exits 0 saying "Synced 0 tenant(s); 1 skipped."
	// When the operator named exactly one tenant, skipping it is a
	// failure of the thing they asked for, not a partial success worth
	// a zero exit.
	//
	// Across ALL tenants the opposite is right: one unreachable database
	// says nothing about the others (ADR-0003, database-per-tenant), and
	// failing the whole run would make a fleet-wide sync hostage to a
	// single sick tenant. There the warnings and the skipped count are
	// the report.
	if *only != "" && synced == 0 && partial == 0 {
		log.Fatalf("tenant %s was not synced — see the warning above (a mistyped -tenant-id looks exactly like this)", *only)
	}

	// Printed last, and to stdout, because it is the part a human has
	// to act on — buried among per-tenant progress lines it would be
	// missed, which is exactly how a tenant ends up quietly broken on
	// next edit.
	if len(warnings) > 0 {
		fmt.Printf("\n%d data migration(s) now needed:\n", len(warnings))
		for _, w := range warnings {
			fmt.Printf("  %s\n", w)
		}
		fmt.Println("\nRun the matching backfill command for each before considering these tenants done.")
	}
}

// syncTenant publishes one tenant's existing modules at current code.
// Returns any data-migration warnings, and false if the tenant was
// skipped.
//
// A failure on one tenant is logged and skipped rather than aborting
// the run: with database-per-tenant (ADR-0003) one unreachable database
// says nothing about the others, and stopping would leave the fleet
// half-synced with no record of how far it got.
func syncTenant(
	ctx context.Context,
	router *tenantdb.Router,
	id, name string,
	actor audit.Actor,
	dryRun bool,
) ([]string, outcome) {
	tenantDB, err := router.Get(ctx, id)
	if err != nil {
		log.Printf("WARNING: %s (%s): cannot open tenant database, skipped: %v", name, id, err)
		return nil, outcomeSkipped
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	present, err := entityDefs.PublishedModules(ctx)
	if err != nil {
		log.Printf("WARNING: %s (%s): cannot read published modules, skipped: %v", name, id, err)
		return nil, outcomeSkipped
	}
	if len(present) == 0 {
		log.Printf("WARNING: %s (%s): no published definitions carrying a module key — not a provisioned tenant, skipped", name, id)
		return nil, outcomeSkipped
	}
	// ADR-0017 §2's headline property is that syncing never grants a
	// module, and §2 rested that on "foundation is always present by
	// construction" — an assumption the code did not check. Independent
	// review built the counter-example: a tenant holding only a
	// bundle-installed definition gained 22 foundation entity types from
	// a sync, reported as "[foundation]" as though they had been there
	// all along. Checking turns the assumption into an invariant.
	if !contains(present, modules.FoundationKey) {
		log.Printf("WARNING: %s (%s): no foundation definitions published (found module(s): %s) — syncing would GRANT foundation rather than update it, skipped",
			name, id, strings.Join(present, ", "))
		return nil, outcomeSkipped
	}

	// foundation is always present and always synced (ADR-0001 §8); the
	// optional modules are whichever this tenant already has. Anything
	// in its registry that is not a built-in — a module-bundle install
	// (ADR-0012) — is deliberately left alone: this command only knows
	// how to publish code that ships in this binary.
	var optional, external []string
	for _, m := range present {
		if m == modules.FoundationKey {
			continue
		}
		if _, ok := modules.Publishers[m]; ok {
			optional = append(optional, m)
		} else {
			external = append(external, m)
		}
	}

	before, err := publishedVersions(ctx, entityDefs)
	if err != nil {
		log.Printf("WARNING: %s (%s): cannot read current versions, skipped: %v", name, id, err)
		return nil, outcomeSkipped
	}

	if dryRun {
		// Nothing is published, so the "after" state is what the code
		// WOULD publish rather than what the registry holds — and the
		// same substitution makes the invalidated-record warning
		// predictable without writing anything. An earlier version
		// returned here before computing it, which made -dry-run report
		// a version bump while staying silent about the backfill that
		// bump implies: precisely the obligation ADR-0017 §5 exists to
		// surface, missing from the mode most likely to be run first.
		changes, blocked := plannedChanges(ctx, entityDefs, before, optional)
		report(name, id, optional, external, changes, true)
		reportBlocked(name, id, blocked)
		want := codeDefinitions(optional)
		defFor := func(t string) (*entity.Definition, bool) { d, ok := want[t]; return d, ok }
		warnings := requiredFieldWarnings(ctx, tenantDB, name, changes, defFor)
		warnings = append(warnings, targetConstraintWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
		warnings = append(warnings, numericBoundWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
		warnings = append(warnings, uniqueConstraintWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
		return warnings, outcomeSynced
	}

	// A failure part-way through leaves this tenant PARTIALLY published,
	// which is a third outcome — not "synced" and emphatically not
	// "skipped", a word every other use of in this command means
	// untouched. Independent review found the old code returning the
	// skipped path here, which both mis-described the tenant and threw
	// away the data-migration warnings that a partial publish can
	// itself create. So the error is recorded and the function falls
	// through to the same reporting the success path uses.
	var failure error
	if err := modules.PublishFoundation(ctx, tenantDB, actor); err != nil {
		failure = fmt.Errorf("foundation: %w", err)
	}
	for _, m := range optional {
		if failure != nil {
			break
		}
		p := modules.Publishers[m]
		if err := p.Publish(ctx, tenantDB, actor); err != nil {
			failure = fmt.Errorf("%s entities: %w", m, err)
			break
		}
		if err := p.PublishForms(ctx, tenantDB, actor); err != nil {
			failure = fmt.Errorf("%s forms: %w", m, err)
			break
		}
		if p.PublishStatuses != nil {
			if err := p.PublishStatuses(ctx, tenantDB, actor); err != nil {
				failure = fmt.Errorf("%s status graph: %w", m, err)
			}
		}
	}

	after, err := publishedVersions(ctx, entityDefs)
	if err != nil {
		log.Printf("WARNING: %s (%s): published, but cannot re-read versions to report: %v", name, id, err)
		return nil, outcomeSynced
	}
	result := outcomeSynced
	if failure != nil {
		result = outcomePartial
	}
	changes := diffVersions(before, after)
	report(name, id, optional, external, changes, false)
	reportBlocked(name, id, blockedAfterPublish(ctx, entityDefs, after, optional))
	if failure != nil {
		log.Printf("WARNING: %s (%s): PARTIALLY SYNCED — %v. The definitions listed above did land; the rest did not. Re-run once the cause is fixed (publishing is idempotent).",
			name, id, failure)
	}

	defFor := func(t string) (*entity.Definition, bool) {
		v, err := entityDefs.GetPublished(ctx, t)
		if err != nil {
			return nil, false
		}
		var def entity.Definition
		if err := json.Unmarshal(v.Definition, &def); err != nil {
			return nil, false
		}
		return &def, true
	}
	warnings := requiredFieldWarnings(ctx, tenantDB, name, changes, defFor)
	warnings = append(warnings, targetConstraintWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
	warnings = append(warnings, numericBoundWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
	warnings = append(warnings, uniqueConstraintWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
	// Real run only — a dry run must not write record_unique_keys rows,
	// same "nothing is published" contract this whole branch's dry-run
	// sibling above holds for entity/form definitions.
	backfillNewUniqueConstraints(ctx, tenantDB, entityDefs, name, changes, defFor)
	return warnings, result
}

// outcome is how one tenant fared. Partial is deliberately distinct
// from both: a partially-published tenant is neither done nor
// untouched, and calling it "skipped" — as this command first did —
// tells an operator the opposite of the truth.
type outcome int

const (
	outcomeSynced outcome = iota
	outcomePartial
	outcomeSkipped
)

// change is one entity type whose published version moved (or appeared).
type change struct {
	entityType string
	from, to   int // from == 0 means newly published
}

func (c change) String() string {
	if c.from == 0 {
		return fmt.Sprintf("%s v%d (new)", c.entityType, c.to)
	}
	return fmt.Sprintf("%s v%d->v%d", c.entityType, c.from, c.to)
}

func publishedVersions(ctx context.Context, repo *data.EntityDefinitionRepo) (map[string]int, error) {
	types, err := repo.ListPublishedEntityTypes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(types))
	for _, t := range types {
		v, err := repo.GetPublished(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("get published %s: %w", t, err)
		}
		out[t] = v.Version
	}
	return out, nil
}

func diffVersions(before, after map[string]int) []change {
	var out []change
	for t, av := range after {
		if bv, ok := before[t]; !ok {
			out = append(out, change{entityType: t, to: av})
		} else if av != bv {
			out = append(out, change{entityType: t, from: bv, to: av})
		}
	}
	sortChanges(out)
	return out
}

// plannedChanges is diffVersions for a dry run: it compares the
// registry against what the in-binary Definitions would publish,
// without writing anything.
// blocked lists entity types the sync CANNOT advance because the target
// version is rolled back in this tenant's registry, and the versions
// involved.
type blockedChange struct {
	entityType string
	version    int
}

// plannedChanges is diffVersions for a dry run, and it must not promise
// anything a real run will refuse.
//
// The refusal that matters is moduleseed.publishOne's: it leaves a
// version whose row is `rolled_back` alone, deliberately and with its
// own tests. So a target version that is rolled back here will never be
// published, and reporting it as a pending change makes the dry run
// invent work — including, before this was fixed, a backfill obligation
// for a migration that could never happen (independent review).
//
// Those types come back separately rather than being silently dropped:
// a tenant that can never be advanced is exactly the state this command
// exists to make visible, and hiding it would leave it looking healthy.
func plannedChanges(
	ctx context.Context,
	entityDefs *data.EntityDefinitionRepo,
	before map[string]int,
	optional []string,
) (changes []change, blocked []blockedChange) {
	for t, wv := range wantedVersions(optional) {
		bv, ok := before[t]
		if ok && wv <= bv {
			continue
		}
		if isRolledBack(ctx, entityDefs, t, wv) {
			blocked = append(blocked, blockedChange{entityType: t, version: wv})
			continue
		}
		if !ok {
			changes = append(changes, change{entityType: t, to: wv})
		} else {
			changes = append(changes, change{entityType: t, from: bv, to: wv})
		}
	}
	sortChanges(changes)
	sortBlocked(blocked)
	return changes, blocked
}

// wantedVersions is what this binary would publish, by entity type.
func wantedVersions(optional []string) map[string]int {
	want := map[string]int{}
	for _, def := range modules.FoundationDefinitions() {
		want[def.EntityType] = def.Version
	}
	for _, m := range optional {
		for _, def := range modules.Publishers[m].Definitions() {
			want[def.EntityType] = def.Version
		}
	}
	return want
}

// isRolledBack reports whether this exact version is present-and-
// rolled-back. A version that simply does not exist yet is not blocked
// — that is the ordinary case for a new entity.
func isRolledBack(ctx context.Context, entityDefs *data.EntityDefinitionRepo, entityType string, version int) bool {
	v, err := entityDefs.GetVersion(ctx, entityType, version)
	if err != nil {
		return false
	}
	return v.Status == data.StatusRolledBack
}

// blockedAfterPublish finds the same condition on the real path: types
// the code wants to advance that the registry still refuses. Computed
// AFTER publishing, so it reports what actually did not move.
func blockedAfterPublish(
	ctx context.Context,
	entityDefs *data.EntityDefinitionRepo,
	after map[string]int,
	optional []string,
) []blockedChange {
	var blocked []blockedChange
	for t, wv := range wantedVersions(optional) {
		if av, ok := after[t]; ok && av >= wv {
			continue
		}
		if isRolledBack(ctx, entityDefs, t, wv) {
			blocked = append(blocked, blockedChange{entityType: t, version: wv})
		}
	}
	sortBlocked(blocked)
	return blocked
}

func sortBlocked(b []blockedChange) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].entityType < b[j-1].entityType; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}

func reportBlocked(name, id string, blocked []blockedChange) {
	for _, b := range blocked {
		log.Printf("WARNING: %s (%s): %s v%d is rolled_back in this tenant's registry — the sync cannot advance it, and it will stay behind the code until someone re-publishes that version by hand",
			name, id, b.entityType, b.version)
	}
}

// codeDefinitions is what this binary would publish, by entity type —
// the dry run's stand-in for a registry read it deliberately cannot do,
// since it has published nothing.
func codeDefinitions(optional []string) map[string]*entity.Definition {
	out := map[string]*entity.Definition{}
	for _, def := range modules.FoundationDefinitions() {
		out[def.EntityType] = def
	}
	for _, m := range optional {
		for _, def := range modules.Publishers[m].Definitions() {
			out[def.EntityType] = def
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func sortChanges(c []change) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].entityType < c[j-1].entityType; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}

func report(name, id string, optional, external []string, changes []change, dryRun bool) {
	mods := strings.Join(append([]string{modules.FoundationKey}, optional...), ", ")
	prefix := ""
	if dryRun {
		prefix = "DRY RUN: "
	}
	if len(changes) == 0 {
		log.Printf("%s%s (%s): already current [%s]", prefix, name, id, mods)
	} else {
		strs := make([]string, 0, len(changes))
		for _, c := range changes {
			strs = append(strs, c.String())
		}
		log.Printf("%s%s (%s): %s [%s]", prefix, name, id, strings.Join(strs, ", "), mods)
	}
	if len(external) > 0 {
		log.Printf("  note: %s also has non-built-in module(s) %s (module-bundle installs) — left untouched",
			name, strings.Join(external, ", "))
	}
}

// requiredFieldWarnings finds records that a just-published Definition
// has made invalid: rows with no value for a field that is now
// Required. See this command's doc comment on why it reports rather
// than repairs.
// defFor resolves the Definition a change lands on: the freshly
// published one for a real run, the in-binary one for a dry run — which
// is what lets the preview predict the same warnings.
func requiredFieldWarnings(
	ctx context.Context,
	tenantDB *sql.DB,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
) []string {
	records := data.NewRecordRepo(tenantDB)
	var out []string
	for _, c := range changes {
		// Every change is checked, including a type that is new to the
		// REGISTRY. "New means no records" is an assumption `before`
		// cannot support: it is built from *published* definitions, so a
		// type whose only version was rolled back reads as new while its
		// records sit there. Independent review built exactly that and
		// watched one record get stranded in silence. For a genuinely
		// new type every count returns 0, which costs a few indexed
		// count(*)s on a cold path — against a hole in the one thing
		// ADR-0017 §5 says must not be a footnote.
		def, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, f := range def.Fields {
			if !f.Required {
				continue
			}
			n, err := records.CountMissingField(ctx, c.entityType, f.Name)
			if err != nil || n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) lack the now-required %q and will fail validation on next edit",
				tenantName, c.entityType, n, f.Name))
		}
	}
	return out
}

// targetConstraintWarnings is requiredFieldWarnings' counterpart for a
// FieldReference's declared TargetFilter/MustMatchParentField
// (uc-infra#78 follow-up, same class of gap as uc-infra#73): a version
// bump that adds or tightens one of these constraints can invalidate
// existing records the exact same way a newly-Required field does, and
// this command's whole reason for reporting anything (see the package
// doc comment) is so an operator sees that BEFORE the next edit-and-save
// of one of those records 400s with no warning it was coming.
//
// "Added or tightened" is judged by comparing the field's declaration on
// the OLD published Definition (c.from, looked up via entityDefs) against
// defFor's result, field by field (targetConstraintChanged). A field
// whose TargetFilter/MustMatchParentField is byte-for-byte unchanged from
// the previous version needs no new check — whatever warning surfaced
// when it was FIRST added already covered it, and re-warning on every
// later unrelated version bump would train an operator to ignore this
// list. A brand-new-to-the-registry type (c.from == 0, oldDef left nil)
// is deliberately NOT skipped, the same reasoning requiredFieldWarnings'
// own doc comment gives: "new to the published registry" does not mean
// "no records" (a rolled-back version's records are still there) — every
// constrained field is treated as newly-added and checked; the check
// itself is cheap (a handful of indexed page reads) when there really
// are zero records.
func targetConstraintWarnings(
	ctx context.Context,
	tenantDB *sql.DB,
	entityDefs *data.EntityDefinitionRepo,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
) []string {
	records := data.NewRecordRepo(tenantDB)
	var out []string
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		var oldDef *entity.Definition
		if c.from > 0 {
			v, err := entityDefs.GetVersion(ctx, c.entityType, c.from)
			if err == nil {
				var d entity.Definition
				if json.Unmarshal(v.Definition, &d) == nil {
					oldDef = &d
				}
			}
		}
		for _, f := range newDef.Fields {
			if f.Type != entity.FieldReference || (len(f.TargetFilter) == 0 && f.MustMatchParentField == "") {
				continue
			}
			if oldDef != nil && !targetConstraintChanged(oldDef, f) {
				continue
			}
			n, err := crud.CountTargetConstraintViolations(ctx, tenantDB, records, newDef, f.Name)
			if err != nil || n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) already violate the new reference constraint on %q and will fail validation on next edit",
				tenantName, c.entityType, n, f.Name))
		}
	}
	return out
}

// targetConstraintChanged reports whether newField's declared
// TargetFilter/MustMatchParentField is new, or differs from, the
// same-named field on oldDef — deliberately comparing just those two
// constraint mechanisms, not the whole Field (a relabel or an unrelated
// Required flip is not this warning's concern).
func targetConstraintChanged(oldDef *entity.Definition, newField entity.Field) bool {
	oldField, ok := oldDef.FieldByName(newField.Name)
	if !ok {
		return true // the field itself is new
	}
	if oldField.MustMatchParentField != newField.MustMatchParentField {
		return true
	}
	return !reflect.DeepEqual(oldField.TargetFilter, newField.TargetFilter)
}

// numericBoundWarnings is requiredFieldWarnings/targetConstraintWarnings'
// counterpart for a FieldNumber's declared entity.Field.Min/Max
// (uc-infra#80, ADR-0018 §4 and §Consequences): "a row already outside
// the bound will fail its next edit, so the sync's data-migration
// warning is the right place to notice" — this is that warning. Same
// shape as targetConstraintWarnings: only a newly-added-or-tightened
// bound is checked (numericBoundChanged), a brand-new-to-the-registry
// type is NOT skipped for the same "rolled-back records are still
// there" reason, and the count lives in data.RecordRepo because raw SQL
// stays in internal/data (CLAUDE.md).
func numericBoundWarnings(
	ctx context.Context,
	tenantDB *sql.DB,
	entityDefs *data.EntityDefinitionRepo,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
) []string {
	records := data.NewRecordRepo(tenantDB)
	var out []string
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		var oldDef *entity.Definition
		if c.from > 0 {
			v, err := entityDefs.GetVersion(ctx, c.entityType, c.from)
			if err == nil {
				var d entity.Definition
				if json.Unmarshal(v.Definition, &d) == nil {
					oldDef = &d
				}
			}
		}
		for _, f := range newDef.Fields {
			if f.Type != entity.FieldNumber || (f.Min == nil && f.Max == nil) {
				continue
			}
			if oldDef != nil && !numericBoundChanged(oldDef, f) {
				continue
			}
			n, err := records.CountOutOfRangeField(ctx, c.entityType, f.Name, f.Min, f.Max)
			if err != nil || n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) are outside the new bound on %q and will fail validation on next edit",
				tenantName, c.entityType, n, f.Name))
		}
	}
	return out
}

// numericBoundChanged reports whether newField's declared Min/Max is
// new, or tighter, than the same-named field on oldDef. "Tighter" — not
// merely "different" — matters here in a way it didn't for
// targetConstraintChanged: a Min that moved DOWN (or a Max that moved
// UP) can only ever admit more values, never strand a record that
// satisfied the old bound, so re-warning on a loosened bound would be
// noise, not signal.
func numericBoundChanged(oldDef *entity.Definition, newField entity.Field) bool {
	oldField, ok := oldDef.FieldByName(newField.Name)
	if !ok {
		return true // the field itself is new
	}
	if newField.Min != nil && (oldField.Min == nil || *newField.Min > *oldField.Min) {
		return true
	}
	if newField.Max != nil && (oldField.Max == nil || *newField.Max < *oldField.Max) {
		return true
	}
	return false
}

// uniqueConstraintWarnings is targetConstraintWarnings' counterpart for
// Definition.Unique (uc-infra#81): a version bump that adds a new field
// set to Unique can find existing records that already collide on it —
// crud's enforcement stage only ever sees writes going forward, so
// without this warning an operator sees "0 data migrations needed" and
// the first edit-and-save of one of those already-colliding records then
// fails with no warning it was coming, the exact same gap
// targetConstraintWarnings closed for #78.
//
// "Added" is judged by set membership on the OLD published Definition
// (c.from, looked up via entityDefs) against newDef.Unique, canonicalized
// via entity.UniqueConstraintName so declaration order never matters — a
// set unchanged from the previous version needs no new check, same
// reasoning targetConstraintChanged already documents. A brand-new-to-
// the-registry type (c.from == 0) is not skipped, for the same
// "new to the published registry does not mean no records" reason
// requiredFieldWarnings' own doc comment gives.
func uniqueConstraintWarnings(
	ctx context.Context,
	tenantDB *sql.DB,
	entityDefs *data.EntityDefinitionRepo,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
) []string {
	records := data.NewRecordRepo(tenantDB)
	var out []string
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, set := range newlyAddedUniqueSets(ctx, entityDefs, c, newDef) {
			name := entity.UniqueConstraintName(set)
			n, err := crud.CountUniqueConstraintViolations(ctx, records, c.entityType, set)
			if err != nil || n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) already collide on the new unique constraint %q and will fail validation on next edit",
				tenantName, c.entityType, n, name))
		}
	}
	return out
}

// newlyAddedUniqueSets returns newDef.Unique's sets that were NOT already
// declared on c's old published version — the shared "what's actually
// new" logic uniqueConstraintWarnings and backfillNewUniqueConstraints
// both need, extracted so the two can never quietly disagree about which
// sets are new. A brand-new-to-the-registry type (c.from == 0) treats
// every declared set as new, same reasoning requiredFieldWarnings' own
// doc comment gives for not skipping it ("new to the published registry"
// does not mean "no records" — a rolled-back version's records are still
// there).
func newlyAddedUniqueSets(ctx context.Context, entityDefs *data.EntityDefinitionRepo, c change, newDef *entity.Definition) [][]string {
	var oldNames map[string]bool
	if c.from > 0 {
		v, err := entityDefs.GetVersion(ctx, c.entityType, c.from)
		if err == nil {
			var d entity.Definition
			if json.Unmarshal(v.Definition, &d) == nil {
				oldNames = make(map[string]bool, len(d.Unique))
				for _, set := range d.Unique {
					oldNames[entity.UniqueConstraintName(set)] = true
				}
			}
		}
	}
	var out [][]string
	for _, set := range newDef.Unique {
		if oldNames != nil && oldNames[entity.UniqueConstraintName(set)] {
			continue
		}
		out = append(out, set)
	}
	return out
}

// backfillNewUniqueConstraints populates record_unique_keys for every
// EXISTING live record against each newly-declared Unique set — without
// this, the constraint protects nothing that predates it: a pre-existing
// record has no key row, so a BRAND-NEW record can silently duplicate
// its combination even though the old record's own data was perfectly
// valid (independent review, uc-infra#81 follow-up — a real gap distinct
// from uniqueConstraintWarnings' report above, which only covers records
// that already collide with EACH OTHER, not "old record has no key at
// all"). Called only on a real (non-dry-run) sync, after publish, so
// newDef reflects what's actually live.
//
// This is a deliberate, narrow exception to this command's own stated
// "does not attempt the migration itself" stance (this file's own header
// comment) — that stance is about records.data backfills, which are
// entity-specific and judgement-laden (cmd/backfill-inventory-facility
// has to INVENT a facility_id and assert where prior stock was). This
// backfill invents nothing and never touches records.data: it
// deterministically populates the constraint mechanism's OWN bookkeeping
// table (record_unique_keys) from data already there, generically over
// any Definition's Unique sets — no entity-specific knowledge needed,
// the same "generic, no entity knowledge required" bar publishing itself
// already clears. Colliding records are left unbacked (oldest wins,
// crud.BackfillUniqueConstraintKeys' own doc comment) and are exactly
// what uniqueConstraintWarnings above already reports — the two never
// disagree about which records need attention.
func backfillNewUniqueConstraints(
	ctx context.Context,
	tenantDB *sql.DB,
	entityDefs *data.EntityDefinitionRepo,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
) {
	records := data.NewRecordRepo(tenantDB)
	keys := data.NewRecordUniqueKeyRepo(tenantDB)
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, set := range newlyAddedUniqueSets(ctx, entityDefs, c, newDef) {
			name := entity.UniqueConstraintName(set)
			backfilled, skipped, err := crud.BackfillUniqueConstraintKeys(ctx, tenantDB, records, keys, c.entityType, set)
			if err != nil {
				log.Printf("WARNING: %s: %s — could not backfill unique constraint %q, existing records are unprotected until this is retried: %v",
					tenantName, c.entityType, name, err)
				continue
			}
			if backfilled == 0 && skipped == 0 {
				continue
			}
			log.Printf("%s: %s — backfilled unique constraint %q for %d existing record(s)%s",
				tenantName, c.entityType, name, backfilled,
				func() string {
					if skipped == 0 {
						return ""
					}
					return fmt.Sprintf(" (%d left unprotected due to an existing collision, see the warning above)", skipped)
				}())
		}
	}
}
