// Command backfill-purchase-order-status is the fix for the gap
// purchasing.PurchaseOrder's own doc comment documents: this kernel has
// no record-data migration mechanism at all (internal/db/migrations only
// ever touches schema, never JSONB record contents), and PurchaseOrder's
// Version 3->4 bump (branch purchasing-order-status) was the first bump
// to replace an existing Required-bearing field — the plain "status"
// FieldEnum — rather than only add one. A PurchaseOrder row written
// before that change still carries its old "status" string with no
// "status_id"; crud.Engine.ValidateStatusTransition treats it as
// predating the lifecycle pattern and only lets its next edit move it to
// an is_initial status (draft), and internal/data/reporting.go's status
// breakdown drops it entirely (status_id NULL, excluded by the join
// guard).
//
// This is deliberately a targeted, one-off tool for this one migration,
// not a generic backfill framework — there is exactly one case that
// needs one today; a speculative generic mechanism for hypothetical
// future Version bumps would be exactly the premature abstraction
// CLAUDE.md warns against. If a second entity ever needs the same kind
// of fix, generalize then, against two real examples instead of one
// imagined one.
//
// Idempotent: a record that already has status_id is left untouched, so
// running this against an already-migrated tenant (or re-running after a
// partial failure) is safe.
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
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	tenantID := flag.String("tenant-id", "", "tenant whose PurchaseOrder records to backfill (required)")
	actorID := flag.String("actor-id", "", "audit actor id for every record this updates (required)")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	flag.Parse()
	if *tenantID == "" {
		log.Fatal("-tenant-id is required")
	}
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

	poDef, err := publishedDef(ctx, entityDefs, *tenantID, "PurchaseOrder")
	if err != nil {
		log.Fatalf("look up PurchaseOrder definition: %v", err)
	}
	statusDef, err := publishedDef(ctx, entityDefs, *tenantID, "Status")
	if err != nil {
		log.Fatalf("look up Status definition: %v", err)
	}

	// statusIDByCode resolves purchase_order_status's Status records —
	// requires purchasing.PublishStatuses to have already been run for
	// this tenant (cmd/provision-tenant does this automatically for any
	// tenant provisioned after the purchasing-order-status increment;
	// an older tenant needs it run once by hand first). Not scoped by
	// status_type_id: same reasoning as cmd/seed-demo-data's own
	// statusID lookup — only one StatusType exists today, so a plain
	// code lookup is unambiguous.
	statusIDByCode := map[string]string{}
	for _, code := range []string{"draft", "submitted", "approved", "received", "cancelled"} {
		recs, err := engine.ListByField(ctx, statusDef, *tenantID, "code", code)
		if err != nil {
			log.Fatalf("list Status by code %q: %v", code, err)
		}
		if len(recs) == 0 {
			log.Fatalf("no Status record for code %q — run purchasing.PublishStatuses for this tenant first (cmd/provision-tenant does this for any tenant provisioned after the purchasing-order-status increment)", code)
		}
		statusIDByCode[code] = recs[0].ID
	}

	orders, err := engine.List(ctx, poDef, *tenantID)
	if err != nil {
		log.Fatalf("list PurchaseOrder records: %v", err)
	}

	var migrated, alreadyDone, skipped int
	for _, po := range orders {
		if sid, ok := po.Data["status_id"].(string); ok && sid != "" {
			alreadyDone++
			continue
		}
		oldStatus, ok := po.Data["status"].(string)
		if !ok || oldStatus == "" {
			log.Printf("WARNING: PurchaseOrder %s has neither status_id nor a recognizable old status value (%v) — skipped, needs manual review", po.ID, po.Data["status"])
			skipped++
			continue
		}
		newStatusID, ok := statusIDByCode[oldStatus]
		if !ok {
			log.Printf("WARNING: PurchaseOrder %s has unrecognized old status %q — skipped, needs manual review", po.ID, oldStatus)
			skipped++
			continue
		}

		// Full field replacement, same discipline as every other
		// crud.Engine.Update call in this kernel (cmd/seed-demo-data's
		// own PurchaseOrder update is the closest precedent) — copy
		// every existing field forward unchanged except swapping
		// status for status_id; the old "status" key is dropped simply
		// by not including it in the new map (Update's SET data = $1
		// replaces the whole blob, see data.RecordRepo.UpdateTx).
		fields := make(map[string]any, len(po.Data))
		for k, v := range po.Data {
			if k == "status" {
				continue
			}
			fields[k] = v
		}
		fields["status_id"] = newStatusID

		// Validated explicitly first — same reasoning
		// internal/api/handlers.go's createRecord/updateRecord already
		// give for the same pattern, and checked before the *dryRun
		// branch so a dry run's preview matches what a real run would
		// actually do: a legacy row missing some other Required field
		// (this migration only ever supplies status_id, it doesn't
		// invent values for anything else that's missing) is a bad-data
		// problem with *that one record*, not a reason to abort a batch
		// migration that's otherwise fine — skip it for manual review
		// like an unrecognized status, don't Fatal the whole run over
		// one row. An error past this point (DB/tenant/version-
		// conflict) is a real infrastructure problem, not a bad record,
		// and still aborts the run.
		if err := entity.ValidateRecord(poDef, fields); err != nil {
			log.Printf("WARNING: PurchaseOrder %s would fail validation after adding status_id (%v) — skipped, needs manual review", po.ID, err)
			skipped++
			continue
		}

		if *dryRun {
			log.Printf("DRY RUN: would migrate PurchaseOrder %s: status %q -> status_id %s (%s)", po.ID, oldStatus, newStatusID, oldStatus)
			migrated++
			continue
		}

		expectedVersion := po.Version
		if _, err := engine.Update(ctx, poDef, *tenantID, po.ID, fields, &expectedVersion, actor); err != nil {
			log.Fatalf("update PurchaseOrder %s: %v", po.ID, err)
		}
		migrated++
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d PurchaseOrder record(s); %d already had status_id; %d skipped for manual review.\n", verb, migrated, alreadyDone, skipped)
}

func publishedDef(ctx context.Context, repo *data.EntityDefinitionRepo, tenantID, entityType string) (*entity.Definition, error) {
	v, err := repo.GetPublished(ctx, tenantID, entityType)
	if err != nil {
		return nil, fmt.Errorf("get published %s: %w", entityType, err)
	}
	def, err := entity.Unmarshal(v.Definition)
	if err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", entityType, err)
	}
	return def, nil
}
