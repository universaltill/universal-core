// install-module installs an erp_module bundle (ADR-0012's format v1 —
// Entity + Form Definitions plus status graphs as data, ADR-0002's
// module artifact) into one tenant's database, driving the same
// registry draft→approve→publish lifecycle and status seeding the
// built-in Go modules use. The marketplace-side path (download +
// entitlement, ADR-0002 steps 3/6) doesn't exist yet — this is the
// locally-supplied-bundle first step its rollout plan names.
//
//	DATABASE_URL=postgres://… install-module -tenant-id <id> -actor-id <id> -bundle library.json
//	install-module -validate -bundle library.json      # parse+validate only, no database touched
//
// Same operational conventions as cmd/provision-tenant: DATABASE_URL is
// the control-plane database, the tenant's own database is resolved
// through internal/tenantdb's Router (ADR-0003), and every registry
// write carries the -actor-id audit actor. Requires foundation already
// published in the target tenant (provision-tenant always does that
// first).
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/modulebundle"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

func main() {
	bundlePath := flag.String("bundle", "", "path to the erp_module bundle JSON (required)")
	tenantID := flag.String("tenant-id", "", "tenant to install into (required unless -validate)")
	actorID := flag.String("actor-id", "", "audit actor id for every registry write (required unless -validate)")
	// See audit.ResolveCLIActor for why this can't just hard-code
	// ActorHuman (uc-infra#72/#123/#124, uc-infra#167).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	validate := flag.Bool("validate", false, "parse and validate the bundle, then exit without touching any database")
	flag.Parse()

	if *bundlePath == "" {
		log.Fatal("-bundle is required")
	}
	raw, err := os.ReadFile(*bundlePath)
	if err != nil {
		log.Fatalf("read bundle: %v", err)
	}
	b, err := modulebundle.Load(raw)
	if err != nil {
		log.Fatalf("bundle is invalid: %v", err)
	}
	log.Printf("bundle %q (%s) is valid: %d entities, %d forms, %d status graphs",
		b.Module, b.Name, len(b.Entities), len(b.Forms), len(b.StatusGraphs))
	if *validate {
		return
	}

	if *tenantID == "" {
		log.Fatal("-tenant-id is required")
	}
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// tenant lookup.
	actor, err := audit.ResolveCLIActor(*actorID, *actorType, *modelVersion, os.Args[1:])
	if err != nil {
		log.Fatalf("invalid actor: %v", err)
	}

	controlDBURL := os.Getenv("DATABASE_URL")
	if controlDBURL == "" {
		log.Fatal("DATABASE_URL is required (the control-plane database)")
	}

	controlDB, err := sql.Open("pgx", controlDBURL)
	if err != nil {
		log.Fatalf("open control database: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.Ping(); err != nil {
		log.Fatalf("ping control database: %v", err)
	}
	if err := db.ApplyControl(context.Background(), controlDB); err != nil {
		log.Fatalf("apply control-plane migrations: %v", err)
	}
	router, err := tenantdb.NewRouter(controlDB, controlDBURL)
	if err != nil {
		log.Fatalf("build tenant router: %v", err)
	}
	defer router.Close()

	ctx := context.Background()
	tenantDB, err := router.Get(ctx, *tenantID)
	if err != nil {
		log.Fatalf("resolve tenant %s database: %v", *tenantID, err)
	}

	blocked, err := modulebundle.Install(ctx, tenantDB, b, actor)
	if err != nil {
		// Install is non-atomic but resumable (see its doc comment):
		// the honest recovery instruction is "run this again", not
		// "clean something up first".
		log.Fatalf("install: %v\n(the install is resumable — re-running the same command completes a partial install)", err)
	}
	// A nil error is not the same as everything landing (uc-infra#73):
	// an item whose version is rolled_back in this tenant's registry is
	// deliberately left alone rather than erroring, so it has to be
	// reported here instead of folded into the ordinary success line —
	// the same shape of warning cmd/sync-tenant-modules already prints
	// for the built-in-module re-publish path.
	for _, item := range blocked {
		log.Printf("WARNING: %s %s v%d is rolled_back in this tenant's registry — the install left it behind; it will stay unpublished until someone re-publishes that version by hand",
			item.Kind, item.EntityType, item.Version)
	}
	if len(blocked) > 0 {
		// Deliberately does NOT contain the success line's "installed
		// into tenant" wording (independent review) — an operator
		// grepping logs for that phrase must not get a false positive
		// on a run that left items behind.
		log.Fatalf("module %q was NOT fully installed into tenant %s — %d item(s) left behind (see warnings above)", b.Module, *tenantID, len(blocked))
	}
	log.Printf("module %q installed into tenant %s", b.Module, *tenantID)
}
