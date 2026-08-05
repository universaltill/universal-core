// Command backfill-goods-receipt-facility stamps a facility_id onto
// every GoodsReceipt written before that field existed.
//
// GoodsReceipt v2 (uc-infra#54, executing ADR-0015 §5's deferred
// decision) added a REQUIRED facility_id — what makes
// purchasing.PostGoodsReceiptLineToLedger's InventoryItem-crediting
// wiring possible at all: a receipt has to say WHERE goods arrived, not
// just that they did. Required-ness is the point, same reasoning as
// InventoryItem's own v3 migration (cmd/backfill-inventory-facility) —
// but it means every row written under v1 is invalid until stamped.
// Reads are unaffected (entity.ValidateRecord runs on write), so the
// practical window is between publishing v2 and running this.
//
// **The assumption this migration makes, stated plainly: every
// pre-existing GoodsReceipt arrived at one location.** Safe by
// construction, not by luck — the v1 model had no location dimension to
// disagree with. Every facility-less row lands on one facility,
// get-or-created by -facility-code — same default (MAIN) as
// cmd/backfill-inventory-facility, and deliberately the SAME shared
// purchasing.GetOrCreateFacility helper, so a tenant that runs both
// backfills with default flags ends up with pre-existing stock and
// pre-existing receipts pointing at the identical Facility row rather
// than two coincidentally-named ones.
//
// Idempotent: a record that already has a facility_id is left alone, so
// re-running after a partial failure, or against an already-migrated
// tenant, is safe. Supports -dry-run.
//
// **Deliberately does NOT retroactively credit InventoryItem** for
// receipts that predate the wiring. That is a second, materially
// different decision (would it double-count against stock a tenant
// already corrected by hand in the gap before this landed?) that this
// migration does not make silently — it only makes GoodsReceipt records
// valid again, the same narrow scope cmd/backfill-inventory-facility has
// for InventoryItem itself.
//
// The shared loop is internal/kernel/recordmigrate — see that package
// on why validation runs before the dry-run branch, and why a bad
// record is skipped while a bad database aborts.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant), same as
// cmd/backfill-inventory-facility — neither tool has control-plane
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
	facilityCode := flag.String("facility-code", "MAIN", "code of the Facility to assign pre-existing receipts to; created if absent")
	facilityName := flag.String("facility-name", "Main Warehouse", "name used only when the facility is created")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
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
	actor := audit.Actor{Type: audit.ActorHuman, ID: *actorID}
	entityDefs := data.NewEntityDefinitionRepo(sqlDB)
	engine := crud.NewEngine(sqlDB)

	grDef, err := publishedDef(ctx, entityDefs, "GoodsReceipt")
	if err != nil {
		log.Fatalf("look up GoodsReceipt definition: %v", err)
	}
	if _, ok := grDef.FieldByName("facility_id"); !ok {
		log.Fatal("the published GoodsReceipt definition has no facility_id field — publish v2 for this tenant first (cmd/provision-tenant does this)")
	}
	facilityDef, err := publishedDef(ctx, entityDefs, "Facility")
	if err != nil {
		log.Fatalf("look up Facility definition (publish purchasing v3+ for this tenant first): %v", err)
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

	res, err := recordmigrate.Run(ctx, engine, grDef,
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
		log.Fatalf("backfill GoodsReceipt facility: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d GoodsReceipt record(s); %d already had facility_id; %d skipped for manual review.\n",
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
