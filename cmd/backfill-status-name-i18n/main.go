// Command backfill-status-name-i18n converts every Status row's "name"
// field from a plain string to the i18n_text shape ({"en": "<value>"})
// foundation.Status's v2 bump requires (uc-infra#214, ADR-0030).
//
// Status v1's "name" was a plain FieldString; v2 changed its TYPE to
// FieldI18nText — the same class of change PurchaseOrder's v3->v4 bump
// made for "status" -> "status_id" (see that Definition's own doc
// comment), not just a new field added. A Status row written before the
// bump still holds a bare string, which now fails entity.ValidateRecord
// on its next edit. Reads are unaffected (validation only runs on
// write), so the practical window is between publishing v2 and running
// this.
//
// Idempotent: a record whose "name" is already an object (map[string]any)
// is left alone, so re-running after a partial failure, or against an
// already-migrated tenant, is safe. Supports -dry-run.
//
// The shared loop is internal/kernel/recordmigrate — see that package on
// why validation runs before the dry-run branch, and why a bad record is
// skipped while a bad database aborts.
//
// This backfill wraps each row's existing string verbatim as its "en"
// translation — it does not add real per-locale content. Populating
// tr/fa/ar (or any other shipped locale) for each module's actual status
// names is deliberately out of scope here (uc-infra#214's own follow-up
// card): this tool exists to keep every already-provisioned tenant's data
// valid under the new field type, not to translate it.
//
// DATABASE_URL must point directly at the target tenant's own database
// (ADR-0003, database-per-tenant), same as cmd/backfill-purchase-order-
// status and cmd/backfill-inventory-facility — this tool has no
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

	statusDef, err := publishedDef(ctx, entityDefs, "Status")
	if err != nil {
		log.Fatalf("look up Status definition: %v", err)
	}
	if f, ok := statusDef.FieldByName("name"); !ok || f.Type != entity.FieldI18nText {
		log.Fatal("the published Status definition's \"name\" field is not i18n_text yet — publish foundation v2 for this tenant first (cmd/sync-tenant-modules does this)")
	}

	res, err := recordmigrate.Run(ctx, engine, statusDef,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			switch v := rec.Data["name"].(type) {
			case map[string]any:
				return nil, recordmigrate.AlreadyDone, ""
			case string:
				fields := recordmigrate.CopyExcept(rec.Data, "name")
				fields["name"] = map[string]any{"en": v}
				return fields, recordmigrate.Migrate, fmt.Sprintf("name %q -> {\"en\": %q}", v, v)
			default:
				return nil, recordmigrate.Skip, fmt.Sprintf("name field has unexpected type %T (expected string or object)", rec.Data["name"])
			}
		},
		actor,
		recordmigrate.Options{DryRun: *dryRun, Logf: log.Printf},
	)
	if err != nil {
		log.Fatalf("backfill Status name: %v", err)
	}

	verb := "Migrated"
	if *dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d Status record(s); %d already had i18n_text name; %d skipped for manual review.\n",
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
