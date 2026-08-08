// Command backfill-purchase-order-total migrates PurchaseOrder.total
// from its pre-Version-9 shape (a FieldNumber major-unit decimal, e.g.
// 95.00 meaning $95.00) to its Version 9 shape (a FieldMoney whole
// number of minor units, e.g. 9500 — uc-infra#136). A row written before
// that bump still carries the old major-unit value; without this
// backfill it fails entity.ValidateRecord on its next edit (FieldMoney
// rejects any fractional value) and, until then, internal/data/
// reporting.go's moneyMinorUnitsPattern guard (mirroring
// rfq_reporting.go's own established one) excludes it from the
// purchasing report's status-breakdown/vendor-spend sums rather than
// erroring the whole page.
//
// Written against internal/kernel/recordmigrate, the same shared loop
// cmd/backfill-quote-line-unit-price/cmd/backfill-purchase-order-status/
// cmd/backfill-inventory-facility use — see that package's own doc
// comment for the decisions shared across all four.
//
// # Why whole-number legacy values are SKIPPED by default, not converted
//
// Same reasoning as cmd/backfill-quote-line-unit-price's own doc
// comment (uc-infra#68): a major-unit legacy amount that happens to be a
// whole dollar figure ($40.00, stored as the JSON number 40) is, after
// this migration exists, indistinguishable from an ALREADY-migrated
// minor-units value (40 meaning $0.40) by looking at the stored value
// alone. So by default this tool converts only UNAMBIGUOUS rows — a
// stored value with a genuine fractional component can only be a
// pre-migration major-unit amount, since FieldMoney never lets a
// fractional value be written at all — and SKIPS every whole-number row
// as "needs manual review". An operator who has confirmed no real v9
// write can exist yet may pass -include-whole-numbers to also convert
// those — a deliberately separate, explicitly-named opt-in, not a
// default, because getting it wrong silently corrupts real money
// amounts 100x.
//
// Idempotent for the default (fractional-only) mode: a row already
// converted has no fractional component left, so a re-run always skips
// it. NOT idempotent when -include-whole-numbers is passed a second time
// against the same tenant — that flag exists for a single, deliberate,
// human-timed run, not a routine one.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant), same as every other cmd/backfill-*
// tool.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
	"github.com/universaltill/universal-core/internal/kernel/recordmigrate"
)

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
	includeWholeNumbers := flag.Bool("include-whole-numbers", false,
		"ALSO convert whole-number total values, not just fractional ones — "+
			"only safe when NO real Version-9 PurchaseOrder has been created yet "+
			"(see this command's own doc comment); getting this wrong silently "+
			"multiplies an already-correct minor-units amount by 100")
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

	def, err := publishedDef(ctx, entityDefs, "PurchaseOrder")
	if err != nil {
		log.Fatalf("look up PurchaseOrder definition: %v", err)
	}
	if def.Version < 9 {
		log.Fatalf("PurchaseOrder's published Definition is still Version %d — publish Version 9 (purchasing.Publish) before running this backfill", def.Version)
	}

	res, err := recordmigrate.Run(ctx, engine, def,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			raw, present := rec.Data["total"]
			if !present {
				// Absent is unambiguous, not a blocker (independent review
				// of uc-infra#136's sibling cmd/backfill-poline-money,
				// applied here too): total is not Required (Default is
				// never applied at write time — see that command's own
				// evaluateMoneyField doc comment), so a PurchaseOrder with
				// no POLines yet and no total set is an ordinary,
				// unremarkable state, not something needing manual review.
				return nil, recordmigrate.AlreadyDone, ""
			}
			f, ok := raw.(float64)
			if !ok {
				return nil, recordmigrate.Skip, fmt.Sprintf("total is not a number (%T)", raw)
			}
			if f == 0 {
				// 0 major units and 0 minor units are the identical value
				// — nothing to convert, and never ambiguous the way a
				// nonzero whole number is.
				return nil, recordmigrate.AlreadyDone, ""
			}
			isWhole := f == math.Trunc(f)
			if isWhole && !*includeWholeNumbers {
				return nil, recordmigrate.Skip, fmt.Sprintf(
					"total %v is already a whole number — ambiguous whether this is an already-migrated minor-units amount or a legacy whole-dollar amount; pass -include-whole-numbers only if you have confirmed no real Version-9 PurchaseOrder exists yet", f)
			}
			minor := ledger.ToMinorUnits(f)
			fields := recordmigrate.CopyExcept(rec.Data)
			fields["total"] = minor
			return fields, recordmigrate.Migrate, fmt.Sprintf("total %v -> %d minor units", f, minor)
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill PurchaseOrder total: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	// res.AlreadyDone counts rows with nothing to convert at all (absent
	// or already-zero total, both unambiguous) — distinct from Skipped,
	// which is reserved for a genuinely ambiguous or malformed value a
	// human should look at.
	fmt.Printf("%s %d PurchaseOrder record(s); %d already had nothing to convert; %d skipped for manual review (see warnings above for why — most commonly an ambiguous whole-number value; pass -include-whole-numbers to also convert those, only if safe per this command's own doc comment).\n",
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
