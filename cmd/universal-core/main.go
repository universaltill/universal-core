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
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/webauth"
	"github.com/universaltill/universal-core/internal/worker"
)

// workerConfigFromEnv builds a worker.Config from WORKFLOW_* environment
// variables, falling back to worker.Config's own defaults (loud on a
// malformed value, same pattern as webauthConfigFromEnv — a silently
// ignored typo in a duration/count here would just look like "workflows
// are slow" or "jobs never run" with no clue why).
func workerConfigFromEnv() worker.Config {
	var cfg worker.Config
	if raw := os.Getenv("WORKFLOW_POLL_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			cfg.PollInterval = d
		} else {
			log.Printf("WORKFLOW_POLL_INTERVAL=%q is not a valid duration, using default", raw)
		}
	}
	if raw := os.Getenv("WORKFLOW_LEASE_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			cfg.LeaseTimeout = d
		} else {
			log.Printf("WORKFLOW_LEASE_TIMEOUT=%q is not a valid duration, using default", raw)
		}
	}
	if raw := os.Getenv("WORKFLOW_WORKER_CONCURRENCY"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			cfg.Concurrency = n
		} else {
			log.Printf("WORKFLOW_WORKER_CONCURRENCY=%q is not a valid integer, using default", raw)
		}
	}
	return cfg
}

// webauthConfigFromEnv builds a webauth.Config from OIDC_* environment
// variables. Every field empty is the expected, safe default (Enabled()
// is false, api.New wires Routes exactly as if webauth didn't exist) —
// there's nothing to configure until a real Zitadel org/app exists for
// this deployment (uc-infra's zitadel.tf).
func webauthConfigFromEnv() webauth.Config {
	ttl := 12 * time.Hour
	if raw := os.Getenv("OIDC_SESSION_TTL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			ttl = d
		} else {
			log.Printf("OIDC_SESSION_TTL=%q is not a valid duration, using default %s", raw, ttl)
		}
	}
	var scopes []string
	if raw := os.Getenv("OIDC_SCOPES"); raw != "" {
		scopes = strings.Split(raw, ",")
	}
	return webauth.Config{
		IssuerURL:     os.Getenv("OIDC_ISSUER_URL"),
		ClientID:      os.Getenv("OIDC_CLIENT_ID"),
		RedirectURL:   os.Getenv("OIDC_REDIRECT_URL"),
		PostLogoutURL: os.Getenv("OIDC_POST_LOGOUT_URL"),
		CookieKeyB64:  os.Getenv("OIDC_COOKIE_KEY"),
		Scopes:        scopes,
		SessionTTL:    ttl,
	}
}

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

	// /api and /forms are always registered; webauth.Guard wrapped around
	// httpx.DevAuth (see api.Routes) is what actually gates access — real
	// login when configured, DevAuth's own fail-closed 401 default
	// otherwise. Always registering the routes means a client gets a
	// consistent response either way, not a plain-text 404 when auth
	// happens to be off.
	webauthCfg := webauthConfigFromEnv()
	auth, err := webauth.New(context.Background(), webauthCfg, data.NewTenantRepo(sqlDB))
	if err != nil {
		log.Fatalf("configure webauth: %v", err)
	}
	if auth.Enabled() {
		log.Printf("webauth: real login enabled (issuer=%s client_id=%s) — /api and /forms redirect unauthenticated browsers to /ui/login", webauthCfg.IssuerURL, webauthCfg.ClientID)
	} else if httpx.DevAuthEnabled() {
		// INSECURE_DEV_AUTH: see internal/httpx/devauth.go's doc comment.
		// Loud on purpose — this must never be silently true in a real
		// deployment. Only reachable at all when webauth itself isn't
		// configured (DevAuth is a no-op fallback behind Guard once it is).
		log.Printf("WARNING: INSECURE_DEV_AUTH=true — /api and /forms routes trust X-Tenant-ID/X-Actor-ID headers with ZERO verification. Do not set this on a publicly reachable deployment.")
	} else {
		log.Printf("no auth backend configured — /api and /forms routes will 401 every request (see QUEUE.md)")
	}
	catalog, err := i18n.Load("en")
	if err != nil {
		log.Fatalf("load i18n catalog: %v", err)
	}
	api.New(sqlDB, catalog, auth).Routes(mux)

	// The durable workflow job queue (internal/kernel/workflow.Queue) has
	// existed since the definition-registry increment, but nothing ever
	// actually ran it — RegistryDefinitionLookup's own doc comment called
	// this out as "a worker process, not built yet." Wire it in: it runs
	// for the lifetime of the process, alongside the HTTP server, with no
	// graceful-shutdown handling yet (consistent with ListenAndServe below,
	// which doesn't have any either — ctx here is process lifetime, not a
	// signal-driven one, on purpose, so this doesn't silently change how
	// the process responds to SIGINT/SIGTERM while that's still unhandled
	// everywhere else in this binary).
	workerRunner, err := worker.New(sqlDB, nil, workerConfigFromEnv())
	if err != nil {
		log.Fatalf("configure workflow worker: %v", err)
	}
	workerRunner.RunConcurrent(context.Background())
	log.Printf("workflow worker started")

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	log.Printf("universal-core kernel listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
