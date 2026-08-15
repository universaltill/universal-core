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
	// See audit.ResolveCLIActor for why this can't just hard-code
	// ActorHuman (uc-infra#72/#123/#124, uc-infra#167).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	only := flag.String("tenant-id", "", "sync just this tenant instead of every provisioned one")
	dryRun := flag.Bool("dry-run", false, "report what would change without publishing anything")
	backfillOnly := flag.String("backfill-only", "", "comma-separated entity types: skip the normal sync entirely and force a fresh unique-constraint backfill for each, against its CURRENTLY PUBLISHED Definition, regardless of whether this run would otherwise consider it changed. The retry path for a unique-constraint backfill that needs to run again against an unchanged Definition version (uc-infra#237) — requires -tenant-id, and does not support -dry-run. -actor-id/-actor-type/-model-version are still validated in this mode but not otherwise used: record_unique_keys is bookkeeping, not audited data (see backfillNewUniqueConstraints' own doc comment).")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// control-plane connection and every tenant's publish work already
	// ran (same discipline as cmd/provision-tenant).
	actor, err := audit.ResolveCLIActor(*actorID, *actorType, *modelVersion, os.Args[1:])
	if err != nil {
		log.Fatalf("invalid actor: %v", err)
	}

	// Same "before any database work" discipline as the actor check above
	// — a fleet-wide -backfill-only run, one combined with -dry-run, or a
	// stray positional argument (independent review, uc-infra#237: this
	// flag takes NO positional args — "-backfill-only Foo Department"
	// would otherwise silently process only "Foo" and drop "Department"
	// without a word) are all mistakes this flag's own doc string already
	// rules out; an operator should learn that before this process even
	// opens the control-plane connection, not after (and not
	// conditionally on whether any tenant happens to be provisioned yet
	// — see this file's "No tenants provisioned" early-return below,
	// which runs before the -backfill-only branch this check would
	// otherwise depend on). backfillOnlyTypes is resolved here too, not
	// just validated: computing it once, before any database work, means
	// the later branch only ever consumes an already-clean list.
	var backfillOnlyTypes []string
	if *backfillOnly != "" {
		if *only == "" {
			log.Fatal("-backfill-only requires -tenant-id — it is a targeted retry against one tenant, not a fleet-wide operation")
		}
		if *dryRun {
			log.Fatal("-backfill-only always writes (it is a retry of a real backfill, and RecordUniqueKeyRepo.InsertTx is idempotent for records it already backed) — it does not support -dry-run")
		}
		if flag.NArg() > 0 {
			log.Fatalf("-backfill-only takes entity types as its own comma-separated value (-backfill-only=Foo,Bar), not positional arguments — unexpected argument(s): %v", flag.Args())
		}
		backfillOnlyTypes = dedupe(splitCommaList(*backfillOnly))
		if len(backfillOnlyTypes) == 0 {
			log.Fatal("-backfill-only requires at least one entity type")
		}
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

	// -backfill-only is a separate, narrower mode from the rest of main():
	// a targeted retry against one already-provisioned tenant's already-
	// published Definitions, not a sync. Handled and returned here, before
	// any of the normal publish/report loop below, so the two modes can't
	// bleed into each other.
	if *backfillOnly != "" {
		// -tenant-id/-dry-run/entity-type-list already validated and
		// resolved (backfillOnlyTypes) above, before any database work.
		tenantDB, err := router.Get(ctx, *only)
		if err != nil {
			log.Fatalf("open tenant database: %v", err)
		}
		entityDefs := data.NewEntityDefinitionRepo(tenantDB)
		name := names[*only]
		if name == "" {
			name = "(unnamed)"
		}
		var results []backfillOnlyResult
		hadFailure := false
		for _, t := range backfillOnlyTypes {
			res := backfillEntityTypeForced(ctx, tenantDB, name, t, entityDefs)
			results = append(results, res)
			if res.failed {
				hadFailure = true
			}
		}
		fmt.Printf("-backfill-only %s:\n", *backfillOnly)
		for _, res := range results {
			// Attempted == 0 (no Unique/UniqueWhen declared on the
			// currently published Definition) is reported explicitly,
			// not folded into silence — independent review of
			// uc-infra#237: silence here reads exactly like "everything
			// is already protected", which is not the same claim as
			// "there is nothing to protect".
			if res.attempted == 0 && !res.failed {
				fmt.Printf("  %s: %s — no unique/conditional-unique constraint declared on the currently published Definition, nothing to backfill.\n", name, res.entityType)
				continue
			}
			for _, line := range res.lines {
				// WARNING lines go to stderr via log.Print, matching
				// every other WARNING this binary emits (uniqueConstraintWarnings,
				// backfillNewUniqueConstraints, etc. — all log.Print, never
				// fmt.Print); only the success/report lines go to stdout.
				// Independent review of uc-infra#237: routing everything
				// through fmt.Printf meant `2>errors.log` captured nothing
				// even when a type genuinely failed.
				if strings.HasPrefix(line, "WARNING:") {
					log.Print(line)
					continue
				}
				fmt.Printf("  %s\n", line)
			}
		}
		if hadFailure {
			// Independent review of uc-infra#237: the previous version of
			// this branch always returned (implicit exit 0) regardless of
			// whether every named entity type failed — indistinguishable
			// from success to any script or CI step checking the exit
			// code. A "some records already collide with each other"
			// result is NOT a failure of the tool itself (that's real,
			// honestly-reported data, the same as the ordinary sync path's
			// own warnings) and does not reach here — only a genuine
			// could-not-read/decode/backfill error does.
			log.Fatalf("-backfill-only %s: one or more entity types failed — see the WARNING(s) above", *backfillOnly)
		}
		return
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
		warnings = append(warnings, typeChangeWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
		// Built inline, unlike the real-run path below: a dry run has only
		// this one consumer of the lookup (no backfill step), so there is
		// nothing to share it with yet. If a second dry-run consumer is
		// ever added, hoist this to a local and pass the same instance to
		// both, the same way the real-run path already has to.
		warnings = append(warnings, uniqueConstraintWarnings(ctx, tenantDB, name, changes, defFor, oldUniqueNameLookup(ctx, entityDefs, name))...)
		warnings = append(warnings, conditionalUniqueConstraintWarnings(ctx, tenantDB, name, changes, defFor, oldConditionalUniqueNameLookup(ctx, entityDefs, name))...)
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

	// Named coverage gap (independent review of uc-infra#159): no test
	// drives a real syncTenant call through publishedDefFor's own error
	// branches — entityDefs is a concrete *data.EntityDefinitionRepo, not
	// an interface, so nothing can be substituted here to fail GetPublished
	// mid-run after the earlier, successful publishedVersions call above,
	// and forcing that exact race against a real Postgres isn't practical.
	// publishedDefFor's own unit tests (published_deffor_test.go) exercise
	// both error branches directly and are what actually cover this
	// behavior; this line's wiring to that function is otherwise
	// unverified by an automated test.
	defFor := publishedDefFor(ctx, entityDefs, name, id)
	// One shared instance across both calls below (uniqueConstraintWarnings
	// and backfillNewUniqueConstraints run over the exact same `changes` on
	// a real sync) so a corrupt/unreadable old version warns once per
	// syncTenant call, not once per caller — see oldUniqueNameLookup's own
	// doc comment.
	oldNamesFor := oldUniqueNameLookup(ctx, entityDefs, name)
	// Same one-shared-instance reasoning as oldNamesFor above, for
	// UniqueWhen (uc-infra#201, ADR-0028) — kept as its own lookup/closure
	// rather than folded into oldNamesFor's: a corrupt/unreadable old
	// version would otherwise warn twice (once per constraint kind) under
	// a shared closure unless that closure's own warned-dedup grew a
	// second key dimension, which is more invasive than a second,
	// independently-cheap re-read for a command that isn't a hot path.
	oldConditionalNamesFor := oldConditionalUniqueNameLookup(ctx, entityDefs, name)
	warnings := requiredFieldWarnings(ctx, tenantDB, name, changes, defFor)
	warnings = append(warnings, targetConstraintWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
	warnings = append(warnings, numericBoundWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
	warnings = append(warnings, typeChangeWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)...)
	warnings = append(warnings, uniqueConstraintWarnings(ctx, tenantDB, name, changes, defFor, oldNamesFor)...)
	warnings = append(warnings, conditionalUniqueConstraintWarnings(ctx, tenantDB, name, changes, defFor, oldConditionalNamesFor)...)
	// Real run only — a dry run must not write record_unique_keys rows,
	// same "nothing is published" contract this whole branch's dry-run
	// sibling above holds for entity/form definitions.
	backfillNewUniqueConstraints(ctx, tenantDB, name, changes, defFor, oldNamesFor)
	backfillNewConditionalUniqueConstraints(ctx, tenantDB, name, changes, defFor, oldConditionalNamesFor)
	return warnings, result
}

// publishedDefFor returns the real-run defFor closure shared by
// requiredFieldWarnings, targetConstraintWarnings, numericBoundWarnings,
// uniqueConstraintWarnings and backfillNewUniqueConstraints: it re-reads
// each changed entity type's now-published Definition so those five can
// check/backfill against what is actually live, not what this binary's
// code would have published (that's the dry-run defFor's job, above).
//
// Every entityType this is ever called with is already known-published as
// of the moment `after` (main.go's syncTenant, above) was captured:
// `changes` is built from `after`, and `after` came from
// publishedVersions, which only ever adds an entity type to its map once
// GetPublished has already succeeded for it earlier in this same
// syncTenant call. So a !ok result here overwhelmingly means a transient
// failure between that first successful read and this one — a DB error
// (dropped connection, canceled context) or a corrupt/unexpected stored
// definition that fails to unmarshal — rather than "genuinely not
// published." (Not an absolute guarantee: a concurrent operator rolling
// that exact version back in the narrow window between the two reads
// would make GetPublished legitimately return no-rows here too. Harmless
// either way — this logs and the caller skips the check regardless of
// which of the two actually happened — but worth naming so "genuinely
// not published" isn't read as impossible, only vanishingly unlikely and
// not the case this WARNING is written for.)
//
// Every one of the five callers above silently `continue`s past a !ok
// result (correctly — a missing Definition means the check for that
// entity type is simply skipped, same as this file's other Count*-error
// handling), so without a WARNING logged from here, that transient
// failure vanishes with no operator-visible trace at all: the sync
// still reports success, and whichever data-migration check invoked
// defFor this time — required-field, reference-constraint, numeric-bound,
// unique-constraint, or the unique-constraint backfill itself — silently
// does not run (uc-infra#159, a wider version of the single-Count*-call
// gap uc-infra#116/#158 closed). Logging once here, rather than
// duplicating the same WARNING at all five call sites, is what the
// issue's own suggested fix asked for: defFor is shared, so the fix
// belongs in defFor.
//
// The WARNING deliberately says "at least one ... check ... may be
// incomplete", not "every check for this entity type is skipped" — the
// same entityType reaches this closure once per caller (up to five times
// per syncTenant call, one per consumer above), and *sql.DB pools
// reconnect, so a failure on the first call and a recovery on the second
// through fifth is a real, ordinary case: claiming all five failed here
// would be actively wrong for it, not just imprecise. warned dedupes by
// entity type so a *persistent* failure (a genuinely dead connection, a
// stored definition that never becomes valid) still logs once per
// syncTenant call rather than once per caller — up to five near-identical
// multi-line WARNINGs for the same entity type would bury the signal this
// fix exists to surface, working against the operator-visibility this
// whole file's header comment is about. dedup is safe unsynchronized:
// syncTenant and this closure never run this map from more than one
// goroutine.
func publishedDefFor(ctx context.Context, entityDefs *data.EntityDefinitionRepo, tenantName, tenantID string) func(string) (*entity.Definition, bool) {
	warned := make(map[string]bool)
	warnOnce := func(t, msg string) {
		if warned[t] {
			return
		}
		warned[t] = true
		log.Print(msg)
	}
	return func(t string) (*entity.Definition, bool) {
		v, err := entityDefs.GetPublished(ctx, t)
		if err != nil {
			warnOnce(t, fmt.Sprintf(
				"WARNING: %s (%s): %s — could not re-read the just-published definition to check for data-migration warnings this run; at least one required-field/reference/bound/unique-constraint check (or a unique-constraint backfill) for this entity type may be incomplete: %v",
				tenantName, tenantID, t, err))
			return nil, false
		}
		var def entity.Definition
		if err := json.Unmarshal(v.Definition, &def); err != nil {
			warnOnce(t, fmt.Sprintf(
				"WARNING: %s (%s): %s — the just-published definition could not be decoded to check for data-migration warnings this run; at least one required-field/reference/bound/unique-constraint check (or a unique-constraint backfill) for this entity type may be incomplete: %v",
				tenantName, tenantID, t, err))
			return nil, false
		}
		return &def, true
	}
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

// splitCommaList splits a comma-separated flag value, trimming whitespace
// around each entry and dropping empties — so "-backfill-only Foo, ,Bar"
// and "-backfill-only Foo,Bar" behave the same, and a trailing/stray comma
// can't silently produce a blank entity type that every downstream lookup
// would just fail to find.
func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// dedupe drops repeated entries, keeping first-occurrence order — used
// on -backfill-only's entity type list so "-backfill-only=Foo,Foo"
// backfills Foo once, not twice (independent review, uc-infra#237).
func dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
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
			if err != nil {
				log.Printf("WARNING: %s: %s — could not check existing records against the now-required %q, this warning may be incomplete: %v",
					tenantName, c.entityType, f.Name, err)
				continue
			}
			if n == 0 {
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
			if err != nil {
				log.Printf("WARNING: %s: %s v%d — could not re-read the previous published version to tell which reference constraints are actually new this run, so every reference constraint on this entity type is being re-checked as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
					tenantName, c.entityType, c.from, err)
			} else {
				var d entity.Definition
				if err := json.Unmarshal(v.Definition, &d); err != nil {
					log.Printf("WARNING: %s: %s v%d — the previous published version could not be decoded to tell which reference constraints are actually new this run, so every reference constraint on this entity type is being re-checked as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
						tenantName, c.entityType, c.from, err)
				} else {
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
			if err != nil {
				log.Printf("WARNING: %s: %s — could not check existing records against the new reference constraint on %q, this warning may be incomplete: %v",
					tenantName, c.entityType, f.Name, err)
				continue
			}
			if n == 0 {
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

// typeChangeWarnings is requiredFieldWarnings/targetConstraintWarnings/
// numericBoundWarnings' counterpart for a field's declared TYPE changing
// on an existing field NAME (uc-infra#214/ADR-0031's Status.name
// string->i18n_text bump is the first of this class this command has
// ever had to report on). requiredFieldWarnings' own CountMissingField
// cannot catch this: a legacy Status row's "name" is a non-empty plain
// string, so it reads as present under the old-or-new type either way —
// zero warnings, and the operator learns about the now-invalid shape
// only when an edit-and-save 400s, exactly the silent state every other
// warning in this file exists to make loud (ADR-0017 §5). This is why
// this diff's own doc comments elsewhere calling Status v1->v2 "the same
// class of bump as PurchaseOrder v3->v4 / InventoryItem v2->v3" only
// held on the "a Version bump can invalidate existing rows" dimension —
// those two bumps both ADDED a new required field, which
// requiredFieldWarnings already catches; this is the first bump in the
// "same field name, new type" class, and needed its own check.
//
// Same "compare against c.from's old Definition, skip if unchanged,
// treat a from==0 (new to the registry) field as changed" shape as
// targetConstraintWarnings above — see that function's own doc comment
// for why a brand-new-to-the-registry type is deliberately not skipped.
func typeChangeWarnings(
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
			if err != nil {
				log.Printf("WARNING: %s: %s v%d — could not re-read the previous published version to tell which field types actually changed this run, so every field's type is being re-checked as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
					tenantName, c.entityType, c.from, err)
			} else {
				var d entity.Definition
				if err := json.Unmarshal(v.Definition, &d); err != nil {
					log.Printf("WARNING: %s: %s v%d — the previous published version could not be decoded to tell which field types actually changed this run, so every field's type is being re-checked as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
						tenantName, c.entityType, c.from, err)
				} else {
					oldDef = &d
				}
			}
		}
		for _, f := range newDef.Fields {
			shape, known := f.Type.FieldTypeJSONShape()
			if !known {
				continue
			}
			if !fieldTypeChanged(oldDef, f) {
				continue
			}
			n, err := records.CountFieldTypeMismatch(ctx, c.entityType, f.Name, shape)
			if err != nil {
				log.Printf("WARNING: %s: %s — could not check existing records against the new type of %q, this warning may be incomplete: %v",
					tenantName, c.entityType, f.Name, err)
				continue
			}
			if n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) still hold %q in its old shape and will fail validation on next edit; run the matching backfill command for this entity type",
				tenantName, c.entityType, n, f.Name))
		}
	}
	return out
}

// fieldTypeChanged reports whether newField's declared Type is new, or
// differs from, the same-named field on oldDef — targetConstraintChanged's
// counterpart for a field's TYPE rather than its reference constraints.
// oldDef == nil (nothing to compare against — either genuinely new to the
// registry, or its old version couldn't be read/decoded) is treated as
// changed, the same "fail open to checking everything" choice
// targetConstraintChanged and numericBoundChanged both make. A field
// that's new to oldDef (no prior field of that name at all) is
// deliberately NOT reported as a type change — a field appearing for the
// first time isn't a change to its OWN type, it has no prior type to
// have changed from; requiredFieldWarnings already covers "invalid
// because this field didn't exist on those rows" if it's Required.
func fieldTypeChanged(oldDef *entity.Definition, newField entity.Field) bool {
	if oldDef == nil {
		return true
	}
	oldField, existed := oldDef.FieldByName(newField.Name)
	if !existed {
		return false
	}
	return oldField.Type != newField.Type
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
			if err != nil {
				log.Printf("WARNING: %s: %s v%d — could not re-read the previous published version to tell which numeric bounds are actually new this run, so every numeric bound on this entity type is being re-checked as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
					tenantName, c.entityType, c.from, err)
			} else {
				var d entity.Definition
				if err := json.Unmarshal(v.Definition, &d); err != nil {
					log.Printf("WARNING: %s: %s v%d — the previous published version could not be decoded to tell which numeric bounds are actually new this run, so every numeric bound on this entity type is being re-checked as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
						tenantName, c.entityType, c.from, err)
				} else {
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
			if err != nil {
				log.Printf("WARNING: %s: %s — could not check existing records against the new bound on %q, this warning may be incomplete: %v",
					tenantName, c.entityType, f.Name, err)
				continue
			}
			if n == 0 {
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
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
	oldNamesFor func(change) map[string]bool,
) []string {
	records := data.NewRecordRepo(tenantDB)
	var out []string
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, set := range newlyAddedUniqueSets(c, newDef, oldNamesFor) {
			name := entity.UniqueConstraintName(set)
			n, err := crud.CountUniqueConstraintViolations(ctx, records, c.entityType, set)
			if err != nil {
				log.Printf("WARNING: %s: %s — could not check existing records against the new unique constraint %q, this warning may be incomplete: %v",
					tenantName, c.entityType, name, err)
				continue
			}
			if n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) already collide on the new unique constraint %q and will fail validation on next edit",
				tenantName, c.entityType, n, name))
		}
	}
	return out
}

// oldUniqueNameLookup returns a closure that resolves c's OLD published
// version's declared Unique-set names, canonicalized via
// entity.UniqueConstraintName — the piece newlyAddedUniqueSets needs to
// tell "already declared" apart from "actually new".
//
// This is built ONCE per syncTenant call and shared by both of
// newlyAddedUniqueSets' callers (uniqueConstraintWarnings and, on a real
// run, backfillNewUniqueConstraints — both invoked with the exact same
// `changes`): without sharing it, a single corrupt/unreadable old version
// would resolve independently in each caller and log the identical
// WARNING twice per syncTenant call, the same "near-identical warnings
// bury the signal" problem publishedDefFor's own dedup exists to avoid.
// Deduped by entity type, same reasoning and same one-call scope as
// publishedDefFor's warned map — c.from is fixed per entity type within
// one syncTenant call, so keying on entityType alone can't hide a
// different failure for the same type.
func oldUniqueNameLookup(ctx context.Context, entityDefs *data.EntityDefinitionRepo, tenantName string) func(c change) map[string]bool {
	warned := make(map[string]bool)
	warnOnce := func(t, msg string) {
		if warned[t] {
			return
		}
		warned[t] = true
		log.Print(msg)
	}
	return func(c change) map[string]bool {
		if c.from == 0 {
			return nil // brand new to the registry: nothing old to compare against, not an error
		}
		v, err := entityDefs.GetVersion(ctx, c.entityType, c.from)
		if err != nil {
			warnOnce(c.entityType, fmt.Sprintf(
				"WARNING: %s: %s v%d — could not re-read the previous published version to tell which unique constraints are actually new this run, so every unique constraint on this entity type is being re-checked (and re-backfilled) as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
				tenantName, c.entityType, c.from, err))
			return nil
		}
		var d entity.Definition
		if err := json.Unmarshal(v.Definition, &d); err != nil {
			warnOnce(c.entityType, fmt.Sprintf(
				"WARNING: %s: %s v%d — the previous published version could not be decoded to tell which unique constraints are actually new this run, so every unique constraint on this entity type is being re-checked (and re-backfilled) as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
				tenantName, c.entityType, c.from, err))
			return nil
		}
		names := make(map[string]bool, len(d.Unique))
		for _, set := range d.Unique {
			names[entity.UniqueConstraintName(set)] = true
		}
		return names
	}
}

// newlyAddedUniqueSets returns newDef.Unique's sets that were NOT already
// declared on c's old published version (per oldNamesFor, see
// oldUniqueNameLookup above) — the shared "what's actually new" logic
// uniqueConstraintWarnings and backfillNewUniqueConstraints both need,
// extracted so the two can never quietly disagree about which sets are
// new. A brand-new-to-the-registry type (c.from == 0) treats every
// declared set as new, same reasoning requiredFieldWarnings' own doc
// comment gives for not skipping it ("new to the published registry"
// does not mean "no records" — a rolled-back version's records are still
// there); so does a lookup that failed to resolve an old version at all
// (oldNamesFor already warned about that; a nil result here fails open
// to "check everything", never silently to "check nothing").
func newlyAddedUniqueSets(c change, newDef *entity.Definition, oldNamesFor func(change) map[string]bool) [][]string {
	oldNames := oldNamesFor(c)
	var out [][]string
	for _, set := range newDef.Unique {
		if oldNames != nil && oldNames[entity.UniqueConstraintName(set)] {
			continue
		}
		out = append(out, set)
	}
	return out
}

// backfillLogLine and backfillWarnLine format one entity type's backfill
// result — shared by backfillNewUniqueConstraints,
// backfillNewConditionalUniqueConstraints, and backfillEntityTypeForced
// (the -backfill-only retry path) so the three call sites can never
// independently drift on wording the way backfillNewUniqueConstraints'
// and backfillNewConditionalUniqueConstraints' own error line already
// once did (uc-infra#228: one had the correct "no retry path exists"
// wording, the other still overclaimed "until this is retried" — fixed
// by making both read from the same place instead of two copies that
// can silently diverge again).
//
// collisionHint is the parenthetical a nonzero `skipped` count gets —
// callers pass what's actually true for them: the two ordinary-sync
// callers ran uniqueConstraintWarnings/conditionalUniqueConstraintWarnings
// over the same `changes` moments earlier and can truthfully point back
// at that ("see the warning above"); backfillEntityTypeForced runs
// standalone with no such warning pass, so pointing there would send an
// operator looking for output that was never printed (independent review
// of uc-infra#237 caught this — the shared-helper refactor is what
// introduced the false pointer, since the original two copies each wrote
// their own, only-locally-true wording).
func backfillLogLine(tenantName, entityType, kind, name string, backfilled, skipped int, collisionHint string) string {
	suffix := ""
	if skipped > 0 {
		suffix = fmt.Sprintf(" (%d left unprotected due to an existing collision%s)", skipped, collisionHint)
	}
	return fmt.Sprintf("%s: %s — backfilled %s %q for %d existing record(s)%s",
		tenantName, entityType, kind, name, backfilled, suffix)
}

func backfillWarnLine(tenantName, entityType, kind, name string, err error) string {
	return fmt.Sprintf("WARNING: %s: %s — could not backfill %s %q, existing records are unprotected: %v",
		tenantName, entityType, kind, name, err)
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
// what uniqueConstraintWarnings above already reports.
//
// Previously, a re-backfill re-attempting an entity type that was
// already fully backed (the fail-open re-check path below, or
// backfillEntityTypeForced's forced retry) misreported already-protected
// records as "skipped" / "unprotected", because record_unique_keys had
// no per-record uniqueness carve-out and an already-keyed record's own
// re-insert collided with ITSELF. Fixed at the source
// (RecordUniqueKeyRepo.InsertTx is now idempotent for a record
// re-claiming a key it already owns — see that method's own doc
// comment), not here: this function's own backfilled/skipped counting
// needed no change, it was already reading real conflict outcomes.
func backfillNewUniqueConstraints(
	ctx context.Context,
	tenantDB *sql.DB,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
	oldNamesFor func(change) map[string]bool,
) {
	records := data.NewRecordRepo(tenantDB)
	keys := data.NewRecordUniqueKeyRepo(tenantDB)
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, set := range newlyAddedUniqueSets(c, newDef, oldNamesFor) {
			name := entity.UniqueConstraintName(set)
			backfilled, skipped, err := crud.BackfillUniqueConstraintKeys(ctx, tenantDB, records, keys, c.entityType, set)
			if err != nil {
				// A re-run of this command only re-attempts a backfill for
				// an entity type whose Definition version actually moved
				// again (newlyAddedUniqueSets, via diffVersions) — an
				// unchanged version needs -backfill-only instead
				// (backfillEntityTypeForced) to force a retry (uc-infra#237).
				log.Print(backfillWarnLine(tenantName, c.entityType, "unique constraint", name, err))
				continue
			}
			if backfilled == 0 && skipped == 0 {
				continue
			}
			log.Print(backfillLogLine(tenantName, c.entityType, "unique constraint", name, backfilled, skipped, ", see the warning above"))
		}
	}
}

// conditionalUniqueConstraintWarnings is uniqueConstraintWarnings'
// UniqueWhen counterpart (uc-infra#201, ADR-0028): a version bump that
// adds a new ConditionalUnique can find existing records satisfying its
// condition that already collide on it — e.g. a tenant that already
// holds two Currency rows with is_base=true before this version's sync,
// which crud's enforcement stage only ever sees going forward. Same
// "added" test as uniqueConstraintWarnings (set membership on the OLD
// published Definition vs. the new one, this time keyed by
// entity.ConditionalUniqueConstraintName so declaration order and the
// condition itself both have to match for a constraint to count as
// unchanged).
func conditionalUniqueConstraintWarnings(
	ctx context.Context,
	tenantDB *sql.DB,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
	oldNamesFor func(change) map[string]bool,
) []string {
	records := data.NewRecordRepo(tenantDB)
	var out []string
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, cu := range newlyAddedConditionalUniqueSets(c, newDef, oldNamesFor) {
			name := entity.ConditionalUniqueConstraintName(cu)
			n, err := crud.CountConditionalUniqueConstraintViolations(ctx, records, c.entityType, cu)
			if err != nil {
				log.Printf("WARNING: %s: %s — could not check existing records against the new conditional unique constraint %q, this warning may be incomplete: %v",
					tenantName, c.entityType, name, err)
				continue
			}
			if n == 0 {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s: %s — %d record(s) already collide on the new conditional unique constraint %q and will fail validation on next edit",
				tenantName, c.entityType, n, name))
		}
	}
	return out
}

// oldConditionalUniqueNameLookup is oldUniqueNameLookup's UniqueWhen
// counterpart — resolves c's OLD published version's declared UniqueWhen
// constraint names (via entity.ConditionalUniqueConstraintName), the
// piece newlyAddedConditionalUniqueSets needs to tell "already declared"
// apart from "actually new". Kept as its own closure/lookup rather than
// widening oldUniqueNameLookup's return type — see this function's call
// site in syncTenant for why a second, independent re-read was chosen
// over threading a second key dimension through the shared one.
func oldConditionalUniqueNameLookup(ctx context.Context, entityDefs *data.EntityDefinitionRepo, tenantName string) func(c change) map[string]bool {
	warned := make(map[string]bool)
	warnOnce := func(t, msg string) {
		if warned[t] {
			return
		}
		warned[t] = true
		log.Print(msg)
	}
	return func(c change) map[string]bool {
		if c.from == 0 {
			return nil // brand new to the registry: nothing old to compare against, not an error
		}
		v, err := entityDefs.GetVersion(ctx, c.entityType, c.from)
		if err != nil {
			warnOnce(c.entityType, fmt.Sprintf(
				"WARNING: %s: %s v%d — could not re-read the previous published version to tell which conditional unique constraints are actually new this run, so every conditional unique constraint on this entity type is being re-checked (and re-backfilled) as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
				tenantName, c.entityType, c.from, err))
			return nil
		}
		var d entity.Definition
		if err := json.Unmarshal(v.Definition, &d); err != nil {
			warnOnce(c.entityType, fmt.Sprintf(
				"WARNING: %s: %s v%d — the previous published version could not be decoded to tell which conditional unique constraints are actually new this run, so every conditional unique constraint on this entity type is being re-checked (and re-backfilled) as if newly added; the record-migration warning(s) below may repeat one already reported for an earlier version: %v",
				tenantName, c.entityType, c.from, err))
			return nil
		}
		names := make(map[string]bool, len(d.UniqueWhen))
		for _, cu := range d.UniqueWhen {
			names[entity.ConditionalUniqueConstraintName(cu)] = true
		}
		return names
	}
}

// newlyAddedConditionalUniqueSets is newlyAddedUniqueSets' UniqueWhen
// counterpart — returns newDef.UniqueWhen's entries not already declared
// on c's old published version (per oldNamesFor, see
// oldConditionalUniqueNameLookup above). Same brand-new-type and
// failed-lookup fail-open behavior as newlyAddedUniqueSets, same
// reasoning.
func newlyAddedConditionalUniqueSets(c change, newDef *entity.Definition, oldNamesFor func(change) map[string]bool) []entity.ConditionalUnique {
	oldNames := oldNamesFor(c)
	var out []entity.ConditionalUnique
	for _, cu := range newDef.UniqueWhen {
		if oldNames != nil && oldNames[entity.ConditionalUniqueConstraintName(cu)] {
			continue
		}
		out = append(out, cu)
	}
	return out
}

// backfillNewConditionalUniqueConstraints is backfillNewUniqueConstraints'
// UniqueWhen counterpart (uc-infra#201, ADR-0028) — populates
// record_unique_keys for every EXISTING live record satisfying a
// newly-declared ConditionalUnique's condition, same reasoning as
// backfillNewUniqueConstraints' own doc comment (without this, the
// constraint protects nothing that predates it). Called only on a real
// (non-dry-run) sync, after publish.
func backfillNewConditionalUniqueConstraints(
	ctx context.Context,
	tenantDB *sql.DB,
	tenantName string,
	changes []change,
	defFor func(entityType string) (*entity.Definition, bool),
	oldNamesFor func(change) map[string]bool,
) {
	records := data.NewRecordRepo(tenantDB)
	keys := data.NewRecordUniqueKeyRepo(tenantDB)
	for _, c := range changes {
		newDef, ok := defFor(c.entityType)
		if !ok {
			continue
		}
		for _, cu := range newlyAddedConditionalUniqueSets(c, newDef, oldNamesFor) {
			name := entity.ConditionalUniqueConstraintName(cu)
			backfilled, skipped, err := crud.BackfillConditionalUniqueConstraintKeys(ctx, tenantDB, records, keys, c.entityType, cu)
			if err != nil {
				// See backfillNewUniqueConstraints' identical branch above
				// (backfillWarnLine) for why an unchanged version needs
				// -backfill-only instead.
				log.Print(backfillWarnLine(tenantName, c.entityType, "conditional unique constraint", name, err))
				continue
			}
			if backfilled == 0 && skipped == 0 {
				continue
			}
			log.Print(backfillLogLine(tenantName, c.entityType, "conditional unique constraint", name, backfilled, skipped, ", see the warning above"))
		}
	}
}

// backfillOnlyResult is backfillEntityTypeForced's report for one entity
// type — richer than a plain []string so main() can tell apart the three
// outcomes a standalone tool needs to report honestly (independent
// review of uc-infra#237, findings 2/3): a real failure (Failed) that
// must make the process exit non-zero, "nothing was even declared to
// backfill" (Attempted == 0), and ordinary successful output
// (Attempted > 0, Failed false) — collapsing any of these into the same
// generic "nothing to report" message is exactly what let a mistyped
// entity type, or one with no declared constraints at all, look
// indistinguishable from "everything is protected".
type backfillOnlyResult struct {
	entityType string
	lines      []string
	attempted  int  // Unique + UniqueWhen sets actually walked
	failed     bool // GetPublished/decode error, or a crud backfill call itself erroring
}

// backfillEntityTypeForced re-runs the unique-constraint backfill for
// entityType's CURRENTLY PUBLISHED Definition, over every declared
// Unique and UniqueWhen set — not just the ones newlyAddedUniqueSets /
// newlyAddedConditionalUniqueSets would call "new this run". This is the
// -backfill-only retry path (uc-infra#237 gap 2): the normal real-run
// path (backfillNewUniqueConstraints / backfillNewConditionalUniqueConstraints,
// called from syncTenant) only ever re-attempts a constraint whose
// Definition version actually moved this run (via `changes`), so a
// backfill that failed partway (a transient DB error, say) against a
// version that then stops moving had no other operator-facing way to be
// retried — the previous state of this file's own comments said as much
// ("there is currently no operator-facing path to force a retry").
//
// Safe to force unconditionally, including over constraints that are
// already fully backed, because RecordUniqueKeyRepo.InsertTx now
// reconciles rather than duplicates a record's existing row (uc-infra#237
// gap 3, fixed in this same change) — a record backed by an earlier run
// is no longer misreported as "skipped due to collision" the second time
// around, and a record whose row was left stale by a removed-then-re-added
// Unique set gets that row repointed rather than duplicated.
//
// Unlike backfillNewUniqueConstraints/backfillNewConditionalUniqueConstraints
// (which stay silent for a set with backfilled==0 && skipped==0 — fine
// there, since that's one line among many in a broader sync report), this
// function reports EVERY declared set explicitly, including a zero
// result: -backfill-only is invoked standalone specifically to confirm a
// retry ran, so silence here would be indistinguishable from "nothing was
// attempted".
func backfillEntityTypeForced(ctx context.Context, tenantDB *sql.DB, tenantName, entityType string, entityDefs *data.EntityDefinitionRepo) backfillOnlyResult {
	res := backfillOnlyResult{entityType: entityType}
	v, err := entityDefs.GetPublished(ctx, entityType)
	if err != nil {
		res.failed = true
		res.lines = []string{fmt.Sprintf("WARNING: %s: %s — could not read the currently published definition, -backfill-only skipped this entity type: %v", tenantName, entityType, err)}
		return res
	}
	var def entity.Definition
	if err := json.Unmarshal(v.Definition, &def); err != nil {
		res.failed = true
		res.lines = []string{fmt.Sprintf("WARNING: %s: %s — the currently published definition could not be decoded, -backfill-only skipped this entity type: %v", tenantName, entityType, err)}
		return res
	}

	records := data.NewRecordRepo(tenantDB)
	keys := data.NewRecordUniqueKeyRepo(tenantDB)
	for _, set := range def.Unique {
		res.attempted++
		name := entity.UniqueConstraintName(set)
		backfilled, skipped, err := crud.BackfillUniqueConstraintKeys(ctx, tenantDB, records, keys, entityType, set)
		if err != nil {
			res.failed = true
			res.lines = append(res.lines, backfillWarnLine(tenantName, entityType, "unique constraint", name, err))
			continue
		}
		// No ", see the warning above" hint (backfillLogLine's collisionHint
		// param): -backfill-only runs standalone, with no preceding
		// uniqueConstraintWarnings pass to point back at.
		res.lines = append(res.lines, backfillLogLine(tenantName, entityType, "unique constraint", name, backfilled, skipped, ""))
	}
	for _, cu := range def.UniqueWhen {
		res.attempted++
		name := entity.ConditionalUniqueConstraintName(cu)
		backfilled, skipped, err := crud.BackfillConditionalUniqueConstraintKeys(ctx, tenantDB, records, keys, entityType, cu)
		if err != nil {
			res.failed = true
			res.lines = append(res.lines, backfillWarnLine(tenantName, entityType, "conditional unique constraint", name, err))
			continue
		}
		res.lines = append(res.lines, backfillLogLine(tenantName, entityType, "conditional unique constraint", name, backfilled, skipped, ""))
	}
	return res
}
