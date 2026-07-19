// Command universal-core runs the kernel: applies migrations and serves
// the (currently minimal) HTTP API. This is the kernel spike from
// ADR-0017's rollout §1 — a runnable starting point, not the finished
// product.
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := sqlDB.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"data":null,"error":"database unreachable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"status":"ok"},"error":null}`))
	})

	// /api and /forms are always registered; httpx.DevAuth (wrapped
	// around every one of them in api.Routes) is what actually gates
	// access — it fails closed (401) unless INSECURE_DEV_AUTH=true, so
	// there's no need to hide the routes themselves here too. Always
	// registering means a client gets a consistent JSON 401 either way,
	// not a plain-text 404 when auth happens to be off.
	if httpx.DevAuthEnabled() {
		// INSECURE_DEV_AUTH: see internal/httpx/devauth.go's doc comment.
		// Loud on purpose — this must never be silently true in a real
		// deployment.
		log.Printf("WARNING: INSECURE_DEV_AUTH=true — /api and /forms routes trust X-Tenant-ID/X-Actor-ID headers with ZERO verification. Do not set this on a publicly reachable deployment.")
	} else {
		log.Printf("INSECURE_DEV_AUTH not set — /api and /forms routes will 401 every request (no auth backend configured yet, see QUEUE.md)")
	}
	catalog, err := i18n.Load("en")
	if err != nil {
		log.Fatalf("load i18n catalog: %v", err)
	}
	api.New(sqlDB, catalog).Routes(mux)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	log.Printf("universal-core kernel listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
