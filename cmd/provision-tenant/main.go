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

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// modulePublishers maps a -modules name to its Publish/PublishForms
// pair. Foundation is not in this map — it's always published,
// unconditionally, per ADR-0001 §8's "always present" requirement; it's
// not something an operator opts into per tenant.
var modulePublishers = map[string]struct {
	publish      func(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error
	publishForms func(ctx context.Context, db *sql.DB, tenantID string, actor audit.Actor) error
}{
	"purchasing": {purchasing.Publish, purchasing.PublishForms},
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	name := flag.String("name", "", "tenant name (required unless -tenant-id reuses an existing tenant)")
	region := flag.String("region", "eu-west", "tenant region, only used when creating a new tenant")
	tenantID := flag.String("tenant-id", "", "reuse an existing tenant id instead of creating a new one")
	actorID := flag.String("actor-id", "", "audit actor id for every Definition this provisions (required)")
	modulesFlag := flag.String("modules", "", "comma-separated modules to publish besides foundation (available: purchasing)")
	flag.Parse()

	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	if *tenantID == "" && *name == "" {
		log.Fatal("-name is required when not reusing an existing tenant via -tenant-id")
	}

	var modules []string
	if *modulesFlag != "" {
		modules = strings.Split(*modulesFlag, ",")
		for _, m := range modules {
			if _, ok := modulePublishers[m]; !ok {
				log.Fatalf("unknown module %q (available: purchasing)", m)
			}
		}
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

	id := *tenantID
	if id == "" {
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO tenants (name, region) VALUES ($1, $2) RETURNING id`,
			*name, *region,
		).Scan(&id); err != nil {
			log.Fatalf("create tenant: %v", err)
		}
		log.Printf("created tenant %s", id)
	}

	if err := foundation.Publish(ctx, sqlDB, id, actor); err != nil {
		log.Fatalf("publish foundation entities: %v", err)
	}
	if err := foundation.PublishForms(ctx, sqlDB, id, actor); err != nil {
		log.Fatalf("publish foundation forms: %v", err)
	}
	log.Println("foundation layer published (entities + forms)")

	for _, m := range modules {
		p := modulePublishers[m]
		if err := p.publish(ctx, sqlDB, id, actor); err != nil {
			log.Fatalf("publish %s entities: %v", m, err)
		}
		if err := p.publishForms(ctx, sqlDB, id, actor); err != nil {
			log.Fatalf("publish %s forms: %v", m, err)
		}
		log.Printf("%s module published (entities + forms)", m)
	}

	fmt.Println(id)
}
