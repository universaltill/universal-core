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
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// modulePublishers maps a -modules name to its Publish/PublishForms(/
// PublishStatuses) set. Foundation is not in this map — it's always
// published, unconditionally, per ADR-0001 §8's "always present"
// requirement; it's not something an operator opts into per tenant.
//
// publishStatuses is nilable: it exists because purchasing.PurchaseOrder
// is the first entity to opt into foundation.go's Status/StatusType
// pattern (see purchasing.PublishStatuses's doc comment for why that's
// required tenant data, not optional like cmd/seed-demo-data's sample
// business data) — a future module with no status-managed entity simply
// has nothing to seed here.
var modulePublishers = map[string]struct {
	publish         func(ctx context.Context, db *sql.DB, actor audit.Actor) error
	publishForms    func(ctx context.Context, db *sql.DB, actor audit.Actor) error
	publishStatuses func(ctx context.Context, db *sql.DB, actor audit.Actor) error
}{
	"purchasing": {purchasing.Publish, purchasing.PublishForms, purchasing.PublishStatuses},
	"sales":      {sales.Publish, sales.PublishForms, sales.PublishStatuses},
}

func main() {
	controlDBURL := os.Getenv("DATABASE_URL")
	if controlDBURL == "" {
		log.Fatal("DATABASE_URL is required (the control-plane database — see this file's own doc comment)")
	}

	name := flag.String("name", "", "tenant name (required unless -tenant-id reuses an existing tenant)")
	region := flag.String("region", "eu-west", "tenant region, only used when creating a new tenant")
	tenantID := flag.String("tenant-id", "", "reuse an existing tenant id instead of creating a new one")
	actorID := flag.String("actor-id", "", "audit actor id for every Definition this provisions (required)")
	modulesFlag := flag.String("modules", "", "comma-separated modules to publish besides foundation (available: purchasing, sales)")
	flag.Parse()

	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	if *tenantID == "" && *name == "" {
		log.Fatal("-name is required when not reusing an existing tenant via -tenant-id")
	}

	// De-duplicated via a set: PublishAll is idempotent regardless (a
	// repeat would just be redundant work, not incorrect), but there's
	// no reason to actually do that work or log a module twice for
	// something as easy to catch as "-modules purchasing,purchasing".
	var modules []string
	if *modulesFlag != "" {
		seen := make(map[string]bool)
		for m := range strings.SplitSeq(*modulesFlag, ",") {
			if _, ok := modulePublishers[m]; !ok {
				log.Fatalf("unknown module %q (available: purchasing, sales)", m)
			}
			if !seen[m] {
				seen[m] = true
				modules = append(modules, m)
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
	actor := audit.Actor{Type: audit.ActorHuman, ID: *actorID}

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

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		log.Fatalf("publish foundation entities: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		log.Fatalf("publish foundation forms: %v", err)
	}
	log.Println("foundation layer published (entities + forms)")

	for _, m := range modules {
		p := modulePublishers[m]
		if err := p.publish(ctx, tenantDB, actor); err != nil {
			log.Fatalf("publish %s entities: %v", m, err)
		}
		if err := p.publishForms(ctx, tenantDB, actor); err != nil {
			log.Fatalf("publish %s forms: %v", m, err)
		}
		if p.publishStatuses != nil {
			if err := p.publishStatuses(ctx, tenantDB, actor); err != nil {
				log.Fatalf("publish %s statuses: %v", m, err)
			}
		}
		log.Printf("%s module published (entities + forms)", m)
	}

	fmt.Println(id)
}
