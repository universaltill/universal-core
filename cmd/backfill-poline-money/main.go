// Command backfill-poline-money migrates POLine.unit_price and
// POLine.line_total from their pre-Version-4 shape (FieldNumber
// major-unit decimals, e.g. 9.50/95.00) to their Version 4 shape
// (FieldMoney whole numbers of minor units, e.g. 950/9500 — uc-infra
// #136). A row written before that bump still carries old major-unit
// values for both fields; without this backfill it fails
// entity.ValidateRecord on its next edit (FieldMoney rejects any
// fractional value), and purchasing.PostGoodsReceiptLineToLedger/
// receivedValueForPurchaseOrder (ledger.go) would silently post/compare
// against whatever raw value is still there, 100x too small, until this
// backfill runs against it.
//
// Written against internal/kernel/recordmigrate, the same shared loop
// cmd/backfill-quote-line-unit-price/cmd/backfill-purchase-order-total/
// cmd/backfill-purchase-order-status/cmd/backfill-inventory-facility use
// — see that package's own doc comment for the decisions shared across
// all five.
//
// # Two fields, one Transform, one all-or-nothing decision per row
//
// Unlike every prior backfill-* command (which migrates exactly one
// field), this record carries TWO money fields that must migrate
// together — they were bumped in the same Version (3->4) precisely
// because PurchaseOrderForm's own RollUp wires line_total into
// PurchaseOrder.total, so a row with one field converted and the other
// still major-unit would silently corrupt whatever reads either. Each
// field is evaluated independently for the same
// fractional-is-unambiguous / whole-is-ambiguous rule
// cmd/backfill-quote-line-unit-price established, but the ROW-level
// decision is all-or-nothing: if EITHER field is a whole number not
// covered by -include-whole-numbers, the WHOLE row is skipped for
// manual review, naming which field(s) were ambiguous — converting only
// the unambiguous field and leaving the other untouched would write a
// row mixing minor- and major-unit values under two fields with
// identical FieldMoney semantics, which is a strictly worse state than
// leaving both alone.
//
// Idempotent for the default (fractional-only) mode, same reasoning as
// every other backfill-* command here. NOT idempotent when
// -include-whole-numbers is passed a second time against the same
// tenant.
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
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
	"github.com/universaltill/universal-core/internal/kernel/recordmigrate"
)

// moneyFieldDecision is one field's evaluation within a POLine row —
// whether it needs a real conversion, is ambiguous (a whole number,
// needs -include-whole-numbers), or isn't a usable number at all.
//
// hasChange is false — not a blocker — for both an ABSENT field and a
// stored ZERO (independent review of uc-infra#136's first pass, which
// treated both as ambiguous and let either one block the WHOLE row,
// including its unambiguous sibling field, with no flag able to fix an
// absent value since -include-whole-numbers only ever affects a
// WHOLE-NUMBER decision). Absent has nothing to misinterpret: line_total
// is optional (not Required, and Default is never applied at write
// time — form.FormField's Default handling only ever consults FieldEnum,
// render.go's buildFields), so a POLine with unit_price but no
// line_total is an ordinary, reachable shape (this repo's own
// internal/e2e/field_permission_test.go and purchasing_report_test.go
// fixtures create exactly that), not a rare pre-#82-style edge case. A
// stored zero is unambiguous for a different reason: 0 major units and
// 0 minor units are the identical value, so there is nothing to convert
// and nothing worth gating behind the dangerous whole-number flag.
type moneyFieldDecision struct {
	hasChange   bool   // true only when this field genuinely needs (and gets) a new minor-units value written
	minor       int64  // the value to write when hasChange is true
	skipReason  string // set when this field alone would block the row
	previewNote string // set together with hasChange
}

func evaluateMoneyField(fields map[string]any, field string, includeWholeNumbers bool) moneyFieldDecision {
	raw, present := fields[field]
	if !present {
		return moneyFieldDecision{}
	}
	f, ok := raw.(float64)
	if !ok {
		return moneyFieldDecision{skipReason: fmt.Sprintf("%s is not a number (%T)", field, raw)}
	}
	if f == 0 {
		return moneyFieldDecision{}
	}
	if isWhole := f == math.Trunc(f); isWhole && !includeWholeNumbers {
		return moneyFieldDecision{skipReason: fmt.Sprintf(
			"%s %v is already a whole number — ambiguous whether this is an already-migrated minor-units amount or a legacy whole-dollar amount", field, f)}
	}
	minor := ledger.ToMinorUnits(f)
	return moneyFieldDecision{hasChange: true, minor: minor, previewNote: fmt.Sprintf("%s %v -> %d minor units", field, f, minor)}
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	actorID := flag.String("actor-id", "", "audit actor id for every record this updates (required)")
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	includeWholeNumbers := flag.Bool("include-whole-numbers", false,
		"ALSO convert whole-number unit_price/line_total values, not just fractional ones — "+
			"only safe when NO real Version-4 POLine has been created yet "+
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
	if actor.Type == audit.ActorAgent {
		actor.Input = audit.CLIInvocationInput(os.Args[1:])
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

	def, err := publishedDef(ctx, entityDefs, "POLine")
	if err != nil {
		log.Fatalf("look up POLine definition: %v", err)
	}
	if def.Version < 4 {
		log.Fatalf("POLine's published Definition is still Version %d — publish Version 4 (purchasing.Publish) before running this backfill", def.Version)
	}

	res, err := recordmigrate.Run(ctx, engine, def,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			priceDecision := evaluateMoneyField(rec.Data, "unit_price", *includeWholeNumbers)
			totalDecision := evaluateMoneyField(rec.Data, "line_total", *includeWholeNumbers)

			var blockers []string
			if priceDecision.skipReason != "" {
				blockers = append(blockers, priceDecision.skipReason)
			}
			if totalDecision.skipReason != "" {
				blockers = append(blockers, totalDecision.skipReason)
			}
			if len(blockers) > 0 {
				// All-or-nothing ONLY across fields that actually need
				// converting: see this command's own doc comment on why a
				// partial conversion (one field migrated, the other left
				// major-unit) is worse than converting neither. An absent
				// or zero-valued sibling never reaches this branch at all
				// (evaluateMoneyField's own doc comment) — it isn't
				// "converted", but it was never ambiguous either, so it
				// doesn't block a genuinely ambiguous sibling from being
				// reported here.
				return nil, recordmigrate.Skip, strings.Join(blockers, "; ")
			}
			if !priceDecision.hasChange && !totalDecision.hasChange {
				// Neither field needs a write — absent and/or already
				// zero on both. Not Migrate (nothing changes) and not a
				// row needing manual review (nothing is ambiguous either).
				return nil, recordmigrate.AlreadyDone, ""
			}

			fields := recordmigrate.CopyExcept(rec.Data)
			var notes []string
			if priceDecision.hasChange {
				fields["unit_price"] = priceDecision.minor
				notes = append(notes, priceDecision.previewNote)
			}
			if totalDecision.hasChange {
				fields["line_total"] = totalDecision.minor
				notes = append(notes, totalDecision.previewNote)
			}
			return fields, recordmigrate.Migrate, strings.Join(notes, ", ")
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill POLine unit_price/line_total: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d POLine record(s); %d already had nothing to convert (both fields absent and/or zero); %d skipped for manual review (see warnings above for why — most commonly an ambiguous whole-number value on unit_price and/or line_total; pass -include-whole-numbers to also convert those, only if safe per this command's own doc comment).\n",
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
