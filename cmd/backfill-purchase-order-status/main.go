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
// This was deliberately a targeted, one-off tool when it was written:
// there was exactly one case that needed one, and a speculative generic
// mechanism for hypothetical future Version bumps would have been the
// premature abstraction CLAUDE.md warns against. The comment here ended
// with a standing instruction — "if a second entity ever needs the same
// kind of fix, generalize then, against two real examples instead of
// one imagined one."
//
// **That happened.** InventoryItem v2->v3 (#12, ADR-0015) added a
// required facility_id and needed the same loop, so the shared parts now
// live in internal/kernel/recordmigrate and this command is written
// against it — as is cmd/backfill-inventory-facility. What stayed here
// is the PurchaseOrder-specific half: resolving purchase_order_status
// codes to Status ids, and the transform that swaps the field. Anyone
// facing a third migration should reuse recordmigrate rather than write
// a fourth loop.
//
// Idempotent: a record that already has status_id is left untouched, so
// running this against an already-migrated tenant (or re-running after a
// partial failure) is safe.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant) — this tool has no router/control-plane
// wiring of its own yet, so it operates on whichever database the caller
// connects it to, not a -tenant-id selector.
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
	"github.com/universaltill/universal-core/internal/kernel/recordmigrate"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	actorID := flag.String("actor-id", "", "audit actor id for every record this updates (required)")
	// An unattended pipeline backfill run is an ai_agent actor, and
	// ADR-0001 §14 makes that distinction first-class — hard-coding
	// ActorHuman would write a falsified actor_type onto every record
	// this command migrates (uc-infra#72, same shape as
	// cmd/install-module's fix).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	flag.Parse()
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// connection already ran (same discipline as cmd/install-module).
	actor := audit.Actor{Type: audit.ActorType(*actorType), ID: *actorID, ModelVersion: *modelVersion}
	switch actor.Type {
	case audit.ActorHuman, audit.ActorAgent:
	default:
		log.Fatalf("invalid actor: -actor-type must be %q or %q, got %q", audit.ActorHuman, audit.ActorAgent, *actorType)
	}
	// An ai_agent actor has no natural-language prompt of its own here —
	// the resolved invocation is the closest analogue, and ADR-0001 §14
	// requires input_hash for every ai_agent audit row, not just
	// model_version (uc-infra#124).
	if actor.Type == audit.ActorAgent {
		actor.Input = audit.CLIInvocationInput(os.Args[1:])
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

	poDef, err := publishedDef(ctx, entityDefs, "PurchaseOrder")
	if err != nil {
		log.Fatalf("look up PurchaseOrder definition: %v", err)
	}
	statusDef, err := publishedDef(ctx, entityDefs, "Status")
	if err != nil {
		log.Fatalf("look up Status definition: %v", err)
	}
	statusTypeDef, err := publishedDef(ctx, entityDefs, "StatusType")
	if err != nil {
		log.Fatalf("look up StatusType definition: %v", err)
	}

	// statusIDByCode resolves purchase_order_status's Status records —
	// requires purchasing.PublishStatuses to have already been run for
	// this tenant (cmd/provision-tenant does this automatically for any
	// tenant provisioned after the purchasing-order-status increment;
	// an older tenant needs it run once by hand first). Scoped by
	// status_type_id, not just code: the sales module
	// (internal/kernel/sales) added sales_order_status and
	// customer_invoice_status, which declare their own colliding
	// "draft"/"cancelled" codes — a plain code-only lookup here would be
	// ambiguous on any tenant that also has Sales published (this
	// comment used to claim "only one StatusType exists today," which
	// stopped being true the moment that module shipped).
	statusTypes, err := engine.ListByField(ctx, statusTypeDef, "code", "purchase_order_status")
	if err != nil {
		log.Fatalf("list StatusType by code: %v", err)
	}
	if len(statusTypes) == 0 {
		log.Fatalf("no purchase_order_status StatusType — run purchasing.PublishStatuses for this tenant first")
	}
	existingStatuses, err := engine.ListByField(ctx, statusDef, "status_type_id", statusTypes[0].ID)
	if err != nil {
		log.Fatalf("list Status by status_type_id: %v", err)
	}
	statusIDByCode := map[string]string{}
	for _, r := range existingStatuses {
		if c, _ := r.Data["code"].(string); c != "" {
			statusIDByCode[c] = r.ID
		}
	}
	for _, code := range []string{"draft", "submitted", "approved", "received", "cancelled"} {
		if _, ok := statusIDByCode[code]; !ok {
			log.Fatalf("no purchase_order_status Status record for code %q — run purchasing.PublishStatuses for this tenant first (cmd/provision-tenant does this for any tenant provisioned after the purchasing-order-status increment)", code)
		}
	}

	res, err := recordmigrate.Run(ctx, engine, poDef,
		func(po data.Record) (map[string]any, recordmigrate.Action, string) {
			if sid, ok := po.Data["status_id"].(string); ok && sid != "" {
				return nil, recordmigrate.AlreadyDone, ""
			}
			oldStatus, ok := po.Data["status"].(string)
			if !ok || oldStatus == "" {
				return nil, recordmigrate.Skip,
					fmt.Sprintf("neither status_id nor a recognizable old status value (%v)", po.Data["status"])
			}
			newStatusID, ok := statusIDByCode[oldStatus]
			if !ok {
				return nil, recordmigrate.Skip, fmt.Sprintf("unrecognized old status %q", oldStatus)
			}
			// The old "status" key is dropped by omitting it — Update
			// replaces the whole blob (see recordmigrate's own comment
			// on full field replacement).
			fields := recordmigrate.CopyExcept(po.Data, "status")
			fields["status_id"] = newStatusID
			// The mapping is the whole reason to dry-run a STATUS
			// migration — a bare record id tells an operator nothing the
			// tally didn't.
			return fields, recordmigrate.Migrate,
				fmt.Sprintf("status %q -> status_id %s", oldStatus, newStatusID)
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill PurchaseOrder status: %v", err)
	}
	migrated, alreadyDone, skipped := res.Migrated, res.AlreadyDone, res.Skipped

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d PurchaseOrder record(s); %d already had status_id; %d skipped for manual review.\n", verb, migrated, alreadyDone, skipped)
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
