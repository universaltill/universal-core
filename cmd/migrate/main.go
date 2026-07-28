// Command migrate applies internal/db's embedded migrations against
// DATABASE_URL and exits. Exists so CI (and any operator) can bring a
// database's schema up to date through the same tracked path
// cmd/universal-core itself uses on boot — schema_migrations bookkeeping
// included — rather than a second, untracked mechanism (e.g. applying
// the .sql files directly with psql) that would drift from it. Running
// this and then running cmd/universal-core against the same database is
// a no-op the second time, by design.
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
	target := flag.String("target", "legacy", "which migration set to apply: legacy, control, or tenant")
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
