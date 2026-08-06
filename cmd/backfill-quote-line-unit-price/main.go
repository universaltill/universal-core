// Command backfill-quote-line-unit-price migrates
// RequestForQuotationQuoteLine.unit_price from its pre-Version-2 shape
// (a FieldNumber major-unit decimal, e.g. 9.5 meaning $9.50) to its
// Version 2 shape (a FieldMoney whole number of minor units, e.g. 950 —
// uc-infra#68, independent review). A row written before that bump
// still carries the old major-unit value; without this backfill it
// fails entity.ValidateRecord on its next edit (FieldMoney rejects any
// fractional value) and, until then, is silently excluded from the RFQ
// vendor-comparison report's grid (internal/data/rfq_reporting.go's
// moneyMinorUnitsPattern guard — a genuinely un-migrated row renders as
// "this vendor never quoted," not a 500).
//
// Written against internal/kernel/recordmigrate, the same shared loop
// cmd/backfill-purchase-order-status and cmd/backfill-inventory-facility
// use — see that package's own doc comment for the decisions shared
// across all three.
//
// # Why whole-number legacy values are SKIPPED by default, not converted
//
// A major-unit legacy amount that happens to be a whole dollar figure
// ($4.00, stored as the JSON number 4) is, after this migration exists,
// indistinguishable from an ALREADY-migrated minor-units value (4 meaning
// $0.04) by looking at the stored value alone — there is no marker field
// this migration adds that a re-run could check, unlike PurchaseOrder's
// v3->v4 bump (a new "status_id" key whose presence IS the "already
// done" signal). Converting every whole number unconditionally would be
// safe exactly once — run immediately after publishing Version 2, before
// any new quote line has been created under the new (correct) minor-units
// semantics — and silently WRONG the moment that ceases to hold (a
// second run, or a run after even one real v2 quote line exists, would
// re-multiply an already-correct minor-units amount by 100).
//
// So by default this tool converts only UNAMBIGUOUS rows — a stored
// value with a genuine fractional component can only be a pre-migration
// major-unit amount, since FieldMoney never lets a fractional value be
// written at all — and SKIPS every whole-number row as "needs manual
// review," the same recordmigrate.Skip outcome an unrecognized status
// code gets in cmd/backfill-purchase-order-status. An operator who has
// confirmed (by deployment timing, or by checking created_at/updated_at
// against the Version-2 publish time) that no real v2 write can exist
// yet may pass -include-whole-numbers to also convert those — a
// deliberately separate, explicitly-named opt-in, not a default,
// because getting it wrong silently corrupts real money amounts 100x.
//
// Idempotent for the default (fractional-only) mode: a row already
// converted has no fractional component left, so a re-run always skips
// it. NOT idempotent when -include-whole-numbers is passed a second
// time against the same tenant — that flag exists for a single,
// deliberate, human-timed run, not a routine one.
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
	// Same ADR-0001 §14 reasoning as every other backfill command: an
	// unattended pipeline run is an ai_agent actor, and hard-coding
	// ActorHuman would falsify actor_type on every migrated record
	// (uc-infra#72).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	includeWholeNumbers := flag.Bool("include-whole-numbers", false,
		"ALSO convert whole-number unit_price values, not just fractional ones — "+
			"only safe when NO real Version-2 quote line has been created yet "+
			"(see this command's own doc comment); getting this wrong silently "+
			"multiplies an already-correct minor-units amount by 100")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	actor := audit.Actor{Type: audit.ActorType(*actorType), ID: *actorID, ModelVersion: *modelVersion}
	switch actor.Type {
	case audit.ActorHuman, audit.ActorAgent:
	default:
		log.Fatalf("invalid actor: -actor-type must be %q or %q, got %q", audit.ActorHuman, audit.ActorAgent, *actorType)
	}
	if actor.Type == audit.ActorHuman && *modelVersion != "" {
		log.Fatalf("invalid actor: -model-version is only meaningful when -actor-type is %q", audit.ActorAgent)
	}
	if err := actor.Validate(); err != nil {
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

	def, err := publishedDef(ctx, entityDefs, "RequestForQuotationQuoteLine")
	if err != nil {
		log.Fatalf("look up RequestForQuotationQuoteLine definition: %v", err)
	}
	if def.Version < 2 {
		log.Fatalf("RequestForQuotationQuoteLine's published Definition is still Version %d — publish Version 2 (purchasing.Publish) before running this backfill", def.Version)
	}

	res, err := recordmigrate.Run(ctx, engine, def,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			raw, present := rec.Data["unit_price"]
			if !present {
				return nil, recordmigrate.Skip, "no unit_price value at all"
			}
			f, ok := raw.(float64)
			if !ok {
				return nil, recordmigrate.Skip, fmt.Sprintf("unit_price is not a number (%T)", raw)
			}
			isWhole := f == math.Trunc(f)
			if isWhole && !*includeWholeNumbers {
				return nil, recordmigrate.Skip, fmt.Sprintf(
					"unit_price %v is already a whole number — ambiguous whether this is an already-migrated minor-units amount or a legacy whole-dollar amount; pass -include-whole-numbers only if you have confirmed no real Version-2 quote line exists yet", f)
			}
			minor := ledger.ToMinorUnits(f)
			fields := recordmigrate.CopyExcept(rec.Data)
			fields["unit_price"] = minor
			return fields, recordmigrate.Migrate, fmt.Sprintf("unit_price %v -> %d minor units", f, minor)
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill RequestForQuotationQuoteLine unit_price: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	// res.AlreadyDone is always 0 here: this Transform never returns
	// recordmigrate.AlreadyDone (see the doc comment above on why
	// "already migrated" can't be told apart from "legacy whole-dollar
	// amount" without -include-whole-numbers) — every row that isn't
	// converted comes back as Skipped, with the reason logged per-row by
	// recordmigrate itself.
	fmt.Printf("%s %d RequestForQuotationQuoteLine record(s); %d skipped for manual review (see warnings above for why — most commonly an ambiguous whole-number value; pass -include-whole-numbers to also convert those, only if safe per this command's own doc comment).\n",
		verb, res.Migrated, res.Skipped)
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
