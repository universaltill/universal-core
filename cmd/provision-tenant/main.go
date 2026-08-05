// Command provision-tenant brings a tenant online: creates it (or reuses
// an existing one via -tenant-id) and publishes the foundation layer's
// entity + form Definitions, plus any -modules requested, all the way
// through the registry's real draft->approve->publish lifecycle.
//
// Exists to close a real gap found while dogfooding the purchasing
// module (see uc-infra/docs/code-reviews/2026-07-20-purchasing-module.md):
// foundation.Publish/purchasing.Publish only ever published entity
// Definitions, never Form Definitions, and no code path called
// foundation.PublishForms/purchasing.PublishForms at all — every Form
// Definition was reachable only from tests via a test helper. A tenant
// provisioned by Publish alone can create/list/import records but every
// GET /forms/{entityType}/... 404s. This binary is that missing
// provisioning path, the same way cmd/migrate is the missing schema-setup
// path cmd/universal-core itself also uses on boot.
//
// Safe to re-run: every Publish/PublishForms call is idempotent (see
// moduleseed.PublishAll's doc comment), so provisioning an already-
// provisioned tenant (e.g. to pick up a newly added module) is a no-op
// for what's already published and only brings the new module online.
//
// DATABASE_URL is the control-plane database (ADR-0003) — the tenants
// registry, not any tenant's own data. A new tenant's own database is
// created and migrated here via internal/tenantdb.Router.Create; an
// existing tenant (-tenant-id) is resolved the same way via Router.Get.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/modules"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

func main() {
	controlDBURL := os.Getenv("DATABASE_URL")
	if controlDBURL == "" {
		log.Fatal("DATABASE_URL is required (the control-plane database — see this file's own doc comment)")
	}

	// One derived list, used by both the help text and the unknown-module
	// error. They were two strings — one derived, one hardcoded — which
	// is the second-list-drifts smell internal/modules exists to remove
	// (independent review).
	available := strings.Join(modules.Keys(), ", ")

	name := flag.String("name", "", "tenant name (required unless -tenant-id reuses an existing tenant)")
	region := flag.String("region", "eu-west", "tenant region, only used when creating a new tenant")
	tenantID := flag.String("tenant-id", "", "reuse an existing tenant id instead of creating a new one")
	actorID := flag.String("actor-id", "", "audit actor id for every Definition this provisions (required)")
	// An unattended pipeline provisioning run is an ai_agent actor, and
	// ADR-0001 §14 makes that distinction first-class — hard-coding
	// ActorHuman would write a falsified actor_type onto every
	// draft/approve/publish row of every tenant this pipeline
	// provisions (uc-infra#72, same shape as cmd/install-module's fix).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	modulesFlag := flag.String("modules", "", "comma-separated modules to publish besides foundation (available: "+available+")")
	flag.Parse()

	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	if *tenantID == "" && *name == "" {
		log.Fatal("-name is required when not reusing an existing tenant via -tenant-id")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// control-plane connection and migrations already ran (same
	// discipline as cmd/install-module).
	actor := audit.Actor{Type: audit.ActorType(*actorType), ID: *actorID, ModelVersion: *modelVersion}
	switch actor.Type {
	case audit.ActorHuman, audit.ActorAgent:
	default:
		log.Fatalf("invalid actor: -actor-type must be %q or %q, got %q", audit.ActorHuman, audit.ActorAgent, *actorType)
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

	// De-duplicated via a set: PublishAll is idempotent regardless (a
	// repeat would just be redundant work, not incorrect), but there's
	// no reason to actually do that work or log a module twice for
	// something as easy to catch as "-modules purchasing,purchasing".
	var selected []string
	if *modulesFlag != "" {
		seen := make(map[string]bool)
		for m := range strings.SplitSeq(*modulesFlag, ",") {
			if _, ok := modules.Publishers[m]; !ok {
				log.Fatalf("unknown module %q (available: %s)", m, available)
			}
			if !seen[m] {
				seen[m] = true
				selected = append(selected, m)
			}
		}
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

	ctx := context.Background()

	id := *tenantID
	var tenantDB *sql.DB
	if id == "" {
		id, err = router.Create(ctx, *name, *region)
		if err != nil {
			log.Fatalf("create tenant: %v", err)
		}
		log.Printf("created tenant %s", id)
		tenantDB, err = router.Get(ctx, id)
	} else {
		tenantDB, err = router.Get(ctx, id)
	}
	if err != nil {
		log.Fatalf("resolve tenant %s database: %v", id, err)
	}

	// Via modules.PublishFoundation rather than the two calls directly,
	// so this and cmd/sync-tenant-modules cannot disagree about what
	// "publish foundation" means if it ever grows a third step.
	if err := modules.PublishFoundation(ctx, tenantDB, actor); err != nil {
		log.Fatalf("publish foundation: %v", err)
	}
	log.Println("foundation layer published (entities + forms)")

	for _, m := range selected {
		p := modules.Publishers[m]
		if err := p.Publish(ctx, tenantDB, actor); err != nil {
			log.Fatalf("publish %s entities: %v", m, err)
		}
		if err := p.PublishForms(ctx, tenantDB, actor); err != nil {
			log.Fatalf("publish %s forms: %v", m, err)
		}
		if p.PublishStatuses != nil {
			if err := p.PublishStatuses(ctx, tenantDB, actor); err != nil {
				log.Fatalf("publish %s statuses: %v", m, err)
			}
		}
		log.Printf("%s module published (entities + forms)", m)
	}

	fmt.Println(id)
}
