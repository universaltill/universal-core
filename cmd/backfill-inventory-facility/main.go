// Command backfill-inventory-facility stamps a facility_id onto every
// InventoryItem written before that field existed.
//
// InventoryItem v3 (#12, ADR-0015) added a REQUIRED facility_id, giving
// the entity the (item, facility) key reference-data-model.md §3 always
// described. Required-ness is the point of the change — an optional
// facility would make "stock at no location" a permanently legal state
// — but it means every row written under v2 is invalid until stamped.
// Reads are unaffected (entity.ValidateRecord runs on write), so the
// practical window is between publishing v3 and running this.
//
// **The assumption this migration makes, stated plainly: all
// pre-existing stock was at one location.** That is safe by
// construction rather than by luck — the v2 model had no location
// dimension, so there is no other answer it could have had. Every
// facility-less row lands on one facility, get-or-created by
// -facility-code.
//
// Idempotent: a record that already has a facility_id is left alone, so
// re-running after a partial failure, or against an already-migrated
// tenant, is safe. Supports -dry-run.
//
// The shared loop is internal/kernel/recordmigrate — see that package
// on why validation runs before the dry-run branch, and why a bad
// record is skipped while a bad database aborts.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant), same as
// cmd/backfill-purchase-order-status — neither tool has control-plane
// routing of its own.
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
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
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
	facilityCode := flag.String("facility-code", "MAIN", "code of the Facility to assign pre-existing stock to; created if absent")
	facilityName := flag.String("facility-name", "Main Warehouse", "name used only when the facility is created")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// connection already ran (same discipline as cmd/provision-tenant).
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

	invDef, err := publishedDef(ctx, entityDefs, "InventoryItem")
	if err != nil {
		log.Fatalf("look up InventoryItem definition: %v", err)
	}
	if _, ok := invDef.FieldByName("facility_id"); !ok {
		log.Fatal("the published InventoryItem definition has no facility_id field — publish v3 for this tenant first (cmd/provision-tenant does this)")
	}
	facilityDef, err := publishedDef(ctx, entityDefs, "Facility")
	if err != nil {
		log.Fatalf("look up Facility definition (publish purchasing v3 for this tenant first): %v", err)
	}

	facilityID, created, err := purchasing.GetOrCreateFacility(ctx, engine, facilityDef, *facilityCode, *facilityName, *dryRun, actor)
	if err != nil {
		log.Fatalf("resolve facility %q: %v", *facilityCode, err)
	}
	switch {
	case created && *dryRun:
		log.Printf("DRY RUN: would create Facility %q (%s)", *facilityCode, *facilityName)
	case created:
		log.Printf("created Facility %q (%s): %s", *facilityCode, *facilityName, facilityID)
	default:
		log.Printf("using existing Facility %q: %s", *facilityCode, facilityID)
	}

	res, err := recordmigrate.Run(ctx, engine, invDef,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			if fid, ok := rec.Data["facility_id"].(string); ok && fid != "" {
				return nil, recordmigrate.AlreadyDone, ""
			}
			// Additive migration: nothing is dropped, so no key is
			// omitted — but the whole map is still copied forward,
			// because Update replaces the blob wholesale.
			fields := recordmigrate.CopyExcept(rec.Data)
			fields["facility_id"] = facilityID
			return fields, recordmigrate.Migrate, ""
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill InventoryItem facility: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d InventoryItem record(s); %d already had facility_id; %d skipped for manual review.\n",
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
