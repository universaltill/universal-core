// Command migrate applies internal/db's embedded migrations against
// DATABASE_URL and exits. Exists so CI (and any operator) can bring a
// database's schema up to date through the same tracked path
// cmd/universal-core itself uses on boot — schema_migrations bookkeeping
// included — rather than a second, untracked mechanism (e.g. applying
// the .sql files directly with psql) that would drift from it. Running
// this with -target=control and then running cmd/universal-core against
// the same database is a no-op the second time, by design: both apply
// the same control-plane set, and schema_migrations bookkeeping skips
// what's already recorded. (This is NOT true of the default
// -target=legacy — see below.)
//
// -target selects which migration set to apply (ADR-0003): "legacy"
// (default) is the original shared-database set — still the live
// production shape until the real database-per-tenant data migration
// runs, so this stays the default rather than silently changing
// behavior underneath anyone still using it; "control" is the
// control-plane set (tenants registry only); "tenant" is the per-tenant
// set (everything else, tenant_id-free) — mainly useful for manually
// bringing an existing tenant database's schema up to date after a new
// tenant migration file lands (internal/tenantdb.Router.Create already
// runs this automatically when provisioning a brand-new tenant).
//
// This command is NOT a prerequisite for cmd/provision-tenant,
// cmd/install-module, or cmd/universal-core: all three apply their own
// control-plane migrations (-target=control's set, via db.ApplyControl)
// internally and need nothing run before them. Running this command's
// default (-target=legacy) against what's actually meant to be a
// control-plane database, then running any of those three against that
// same database, applies two different, incompatible migration sets to
// one database and fails (see universaltill/uc-infra#84) — each
// -target/internal caller applies a genuinely different schema for a
// genuinely different kind of database, not a step in one shared setup
// sequence.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	target := flag.String("target", "legacy", "which migration set to apply: legacy (original shared-database set — NOT a prerequisite for cmd/provision-tenant, which applies its own control-plane migrations internally), control (control-plane tenants registry), or tenant (a single tenant's own database)")
	flag.Parse()

	apply, err := applyFuncFor(*target)
	if err != nil {
		log.Fatal(err)
	}

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	if err := apply(context.Background(), sqlDB); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}
	log.Printf("%s migrations applied", *target)
}

func applyFuncFor(target string) (func(context.Context, *sql.DB) error, error) {
	switch target {
	case "legacy":
		return db.Apply, nil
	case "control":
		return db.ApplyControl, nil
	case "tenant":
		return db.ApplyTenant, nil
	default:
		return nil, fmt.Errorf("unknown -target %q (must be legacy, control, or tenant)", target)
	}
}
