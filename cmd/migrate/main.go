// Command migrate applies internal/db's embedded migrations against
// DATABASE_URL and exits. Exists so CI (and any operator) can bring a
// database's schema up to date through the same tracked path
// cmd/universal-core itself uses on boot — schema_migrations bookkeeping
// included — rather than a second, untracked mechanism (e.g. applying
// the .sql files directly with psql) that would drift from it. Running
// this and then running cmd/universal-core against the same database is
// a no-op the second time, by design.
package main

import (
	"context"
	"database/sql"
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

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	if err := db.Apply(context.Background(), sqlDB); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}
	log.Println("migrations applied")
}
