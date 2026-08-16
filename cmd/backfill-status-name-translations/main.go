// Command backfill-status-name-translations adds real tr/fa/ar
// translations onto an already-provisioned tenant's existing Status rows
// (uc-infra#244, phase 2 of uc-infra#214/ADR-0030-0031).
//
// statusgraph.Seed's own getOrCreate is additive-only — an existing
// Status row's "name" is never overwritten on a re-seed — so a tenant
// provisioned before this card shipped keeps whatever content it was
// seeded with (English-only, {"en": ...}) forever, even after the owning
// module's PublishStatuses is re-run: PublishStatuses only creates
// Status rows that don't exist yet, it never mutates existing ones. This
// command is the separate, explicit backfill step ADR-0030's own
// "a row already outside the bound will fail its next edit"-style
// precedent already established for exactly this shape of gap.
//
// Sibling, not successor, of cmd/backfill-status-name-i18n: that tool
// wraps a still-plain-string "name" ({"name": "Draft"}) into the
// i18n_text shape ({"en": "Draft"}) foundation.Status's v2 Version bump
// requires; this one assumes that has already happened (every row it
// touches is already an i18n_text object) and adds any tr/fa/ar keys the
// object is missing. Run the i18n-shape backfill first on any tenant
// that might still have legacy plain-string rows — this command SKIPS a
// row whose "name" isn't already an object, for manual review, rather
// than guessing which one it needs.
//
// Content source: internal/kernel/{purchasing,sales,hr,projects,crm,
// assets}.StatusSpecs() — the identical Spec literals (including
// Translations) each module's own PublishStatuses passes to
// statusgraph.Seed, so a freshly-provisioned tenant and a backfilled
// already-provisioned one end up with byte-identical Status.name
// content, not two independently-maintained copies.
//
// Never overwrites a locale a row already carries (even if it disagrees
// with StatusSpecs' current content — a real conflict there is a content
// question for a human, not something this tool should resolve by
// clobbering silently) — it only ADDS a missing locale key. "en" is
// never touched at all, on the same reasoning.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant), same as cmd/backfill-status-name-i18n
// and cmd/backfill-purchase-order-status — this tool has no
// router/control-plane wiring of its own either.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/recordmigrate"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
)

// allStatusSpecs merges every module's StatusSpecs() into one map keyed
// by StatusTypeCode. Modules are independently licensable with no
// Go-level dependency on each other (statusgraph's own doc comment), but
// this command is a cross-cutting operational tool, not kernel/business
// logic, so importing all six here doesn't cross the boundary CLAUDE.md's
// kernel-boundary rule protects — the same reasoning cmd/seed-demo-data
// and cmd/sync-tenant-modules already rely on.
func allStatusSpecs() map[string][]statusgraph.Spec {
	out := map[string][]statusgraph.Spec{}
	for _, specs := range []map[string][]statusgraph.Spec{
		purchasing.StatusSpecs(),
		sales.StatusSpecs(),
		hr.StatusSpecs(),
		projects.StatusSpecs(),
		crm.StatusSpecs(),
		assets.StatusSpecs(),
	} {
		for code, s := range specs {
			out[code] = s
		}
	}
	return out
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	actorID := flag.String("actor-id", "", "audit actor id for every record this updates (required)")
	// See audit.ResolveCLIActor for why this can't just hard-code
	// ActorHuman (uc-infra#72/#123/#124, uc-infra#167).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	actor, err := audit.ResolveCLIActor(*actorID, *actorType, *modelVersion, os.Args[1:])
	if err != nil {
		log.Fatalf("invalid actor: %v", err)
	}

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	ctx := context.Background()
	entityDefs := data.NewEntityDefinitionRepo(sqlDB)
	engine := crud.NewEngine(sqlDB)

	statusTypeDef, err := publishedDef(ctx, entityDefs, "StatusType")
	if err != nil {
		log.Fatalf("look up StatusType definition: %v", err)
	}
	statusDef, err := publishedDef(ctx, entityDefs, "Status")
	if err != nil {
		log.Fatalf("look up Status definition: %v", err)
	}
	if f, ok := statusDef.FieldByName("name"); !ok || f.Type != entity.FieldI18nText {
		log.Fatal("the published Status definition's \"name\" field is not i18n_text yet — publish foundation v2 for this tenant first (cmd/sync-tenant-modules does this)")
	}

	// statusTypeSpecsByID: for each StatusType row actually seeded in
	// this tenant, its Specs (by code) — built once, up front, so the
	// per-record Transform below is a pure map lookup, not a query.
	statusTypes, err := engine.List(ctx, statusTypeDef)
	if err != nil {
		log.Fatalf("list StatusType records: %v", err)
	}
	statusTypeSpecsByID := map[string]map[string]statusgraph.Spec{}
	knownSpecs := allStatusSpecs()
	for _, st := range statusTypes {
		code, _ := st.Data["code"].(string)
		specs, ok := knownSpecs[code]
		if !ok {
			// A StatusType this command's six modules don't know about —
			// e.g. a test-only fixture, a bundle-installed module
			// (modulebundle's own Translations passthrough handles its
			// own content at Install time, not here), or a future
			// built-in module not yet added to allStatusSpecs. NOT an
			// error and nothing to act on: its Status rows are simply
			// left as-is, same as an unrecognized code below. The
			// per-record note below says so explicitly (recordmigrate's
			// own "skipped, needs manual review" framing is the shared
			// package's generic wording for every Skip reason — the note
			// text is what actually distinguishes "ignore this" from
			// "look at this").
			log.Printf("no known StatusSpecs for StatusType %q (code %q) — left untouched, no action needed", st.ID, code)
			continue
		}
		byCode := make(map[string]statusgraph.Spec, len(specs))
		for _, s := range specs {
			byCode[s.Code] = s
		}
		statusTypeSpecsByID[st.ID] = byCode
	}

	res, err := recordmigrate.Run(ctx, engine, statusDef,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			current, ok := rec.Data["name"].(map[string]any)
			if !ok {
				return nil, recordmigrate.Skip, fmt.Sprintf("name field is %T, not yet an i18n_text object — run cmd/backfill-status-name-i18n first", rec.Data["name"])
			}
			statusTypeID, _ := rec.Data["status_type_id"].(string)
			byCode, ok := statusTypeSpecsByID[statusTypeID]
			if !ok {
				return nil, recordmigrate.Skip, fmt.Sprintf("status_type_id %q has no known StatusSpecs — left untouched, no action needed (not managed by this tool's six known modules)", statusTypeID)
			}
			code, _ := rec.Data["code"].(string)
			spec, ok := byCode[code]
			if !ok {
				// Unlike the unknown-StatusType case above, this one IS
				// worth a human's attention: the StatusType itself is
				// known (one of the six modules' own), but this specific
				// code isn't in that module's StatusSpecs — most likely
				// this row's own module gained a status code that
				// StatusSpecs wasn't updated for, a real drift worth
				// investigating, not an expected "out of scope" case.
				return nil, recordmigrate.Skip, fmt.Sprintf("code %q has no known Spec for its (known) StatusType — possible StatusSpecs drift, worth investigating", code)
			}

			added := map[string]string{}
			for locale, text := range spec.Translations {
				if _, exists := current[locale]; !exists {
					added[locale] = text
				}
			}
			if len(added) == 0 {
				return nil, recordmigrate.AlreadyDone, ""
			}

			merged := make(map[string]any, len(current)+len(added))
			for k, v := range current {
				merged[k] = v
			}
			for locale, text := range added {
				merged[locale] = text
			}
			fields := recordmigrate.CopyExcept(rec.Data, "name")
			fields["name"] = merged
			return fields, recordmigrate.Migrate, fmt.Sprintf("name +%v", added)
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill Status name translations: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d Status record(s); %d already had every known translation; %d skipped for manual review.\n",
		verb, res.Migrated, res.AlreadyDone, res.Skipped)
}

func publishedDef(ctx context.Context, repo *data.EntityDefinitionRepo, entityType string) (*entity.Definition, error) {
	v, err := repo.GetPublished(ctx, entityType)
	if err != nil {
		return nil, fmt.Errorf("get published %s: %w", entityType, err)
	}
	def, err := entity.Unmarshal(v.Definition)
	if err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", entityType, err)
	}
	return def, nil
}
