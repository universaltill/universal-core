// Command backfill-fixedasset-posted-period-count sets
// FixedAsset.posted_period_count (uc-infra#202, assets.go's FixedAsset
// Version 4) on every already-provisioned FixedAsset to the true count
// of its DepreciationSchedule children with a non-empty posted_at — the
// denormalized counter assets.PostDueDepreciationBatch's completion
// sweep (healStuckFullyDepreciatedAssets, via
// data.LifeCompleteGroupOptions.ParentCounterField) now trusts directly
// instead of re-deriving the count from the child rows on every call.
//
// A row created before Version 4 has no posted_period_count key at all,
// which a JSONB read treats as 0 — correct for a never-posted asset,
// silently WRONG (undercounting) for one that already has real posted
// history, which is exactly what this tool corrects once, per tenant.
//
// Until this backfill runs against a tenant's database, the counter
// fast path will not recognize an already-fully-posted-but-not-yet-
// backfilled asset as life-complete (0 >= useful_life_months is false)
// — this fails toward "not yet healed," never toward a premature or
// incorrect transition. See assets.FixedAsset's own doc comment on
// posted_period_count, and
// TestPostDueDepreciation_UnbackfilledCounterDoesNotHealPrematurely
// (internal/kernel/assets/ledger_test.go) for the direct regression
// coverage of that safety property. Not urgent for the same reason
// uc-infra#202 itself was not urgent: the completion sweep's OLD
// (still available — leave ParentCounterField empty) child-counting
// path is always correct, just not O(1); nothing breaks by an operator
// running this a while after the code that reads it ships.
//
// Idempotent: a FixedAsset whose stored posted_period_count already
// matches its true count is left untouched (counted as "already had the
// correct value"), so re-running this after a partial failure, or
// against a tenant that's already been backfilled, is safe and cheap.
// A FixedAsset changed concurrently between this tool's own List and
// its Update for that one asset (most likely the depreciation worker
// posting a row for it mid-run — every posted row bumps FixedAsset's
// own version now, uc-infra#202) is skipped with a warning, not treated
// as fatal — safe to just re-run this tool afterward to pick it up.
//
// NOT run through internal/kernel/recordmigrate's shared Run loop,
// unlike its cmd/backfill-* siblings — recordmigrate.Transform only ever
// sees the one record being migrated, and computing this field needs
// each FixedAsset's own DepreciationSchedule children, a cross-entity
// read recordmigrate has no hook for. The version-checked update,
// skip-if-already-correct, and dry-run behavior below are hand-rolled to
// match recordmigrate's own documented decisions anyway (see that
// package's own comment) rather than diverging from them for no reason.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant), same as every other cmd/backfill-*
// tool.
package main

import (
	"context"
	"database/sql"
	"errors"
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

	assetDef, err := publishedDef(ctx, entityDefs, "FixedAsset")
	if err != nil {
		log.Fatalf("look up FixedAsset definition: %v", err)
	}
	if assetDef.Version < 4 {
		log.Fatalf("FixedAsset's published Definition is still Version %d — publish Version 4 (assets.Publish) before running this backfill", assetDef.Version)
	}
	scheduleDef, err := publishedDef(ctx, entityDefs, "DepreciationSchedule")
	if err != nil {
		log.Fatalf("look up DepreciationSchedule definition: %v", err)
	}

	assetRecs, err := engine.List(ctx, assetDef)
	if err != nil {
		log.Fatalf("list FixedAsset records: %v", err)
	}

	var migrated, alreadyDone, skipped int
	for _, rec := range assetRecs {
		children, err := engine.ListByField(ctx, scheduleDef, "fixed_asset_id", rec.ID)
		if err != nil {
			log.Fatalf("list DepreciationSchedule children for FixedAsset %s: %v", rec.ID, err)
		}
		var trueCount float64
		for _, child := range children {
			if postedAt, _ := child.Data["posted_at"].(string); postedAt != "" {
				trueCount++
			}
		}
		current, _ := rec.Data["posted_period_count"].(float64)
		if current == trueCount {
			alreadyDone++
			continue
		}

		fields := recordmigrate.CopyExcept(rec.Data)
		fields["posted_period_count"] = trueCount
		if err := entity.ValidateRecord(assetDef, fields); err != nil {
			log.Printf("WARNING: FixedAsset %s would fail validation after migration (%v) — skipped, needs manual review", rec.ID, err)
			skipped++
			continue
		}

		if *dryRun {
			log.Printf("DRY RUN: would migrate FixedAsset %s: posted_period_count %v -> %v", rec.ID, current, trueCount)
			migrated++
			continue
		}

		expectedVersion := rec.Version
		if _, err := engine.Update(ctx, assetDef, rec.ID, fields, &expectedVersion, actor); err != nil {
			if errors.Is(err, data.ErrVersionConflict) {
				// A live tenant's worker can post a depreciation row for
				// this exact FixedAsset between our List above and this
				// Update — a real, no-longer-rare race now that every
				// posted row bumps FixedAsset's own version (uc-infra#202
				// independent review finding 6), not the "bad database"
				// class recordmigrate's own doc comment reserves for
				// aborting the whole run. Skip and let a re-run (this
				// tool is idempotent — see its own doc comment) pick this
				// one up once the contention has passed, rather than
				// losing every other asset's already-computed migration
				// because this one raced.
				log.Printf("WARNING: FixedAsset %s changed concurrently (likely the depreciation worker) — skipped, re-run this tool to pick it up", rec.ID)
				skipped++
				continue
			}
			log.Fatalf("update FixedAsset %s: %v", rec.ID, err)
		}
		migrated++
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d FixedAsset record(s); %d already had the correct posted_period_count; %d skipped for manual review.\n",
		verb, migrated, alreadyDone, skipped)
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
