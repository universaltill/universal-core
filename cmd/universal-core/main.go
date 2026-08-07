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
	"github.com/universaltill/universal-core/internal/kernel/aiassist"
	"github.com/universaltill/universal-core/internal/kernel/blobstore"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/kernel/secretcrypt"
	"github.com/universaltill/universal-core/internal/kernel/speechassist"
	"github.com/universaltill/universal-core/internal/svcauth"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/webauth"
	"github.com/universaltill/universal-core/internal/worker"
	"github.com/universaltill/universal-core/internal/zitadelmgmt"
)

// defaultOllamaModel matches the model already pulled by the platform's
// reference self-hosted Ollama deployment — small enough to run on
// modest hardware. A deployment with a different model available
// should set OLLAMA_MODEL explicitly.
const defaultOllamaModel = "llama3.2:3b"

// aiClientFromEnv builds an aiassist.Client from OLLAMA_* environment
// variables, returning the resolved model alongside it purely so the
// caller can log what was actually chosen (env unset vs. the default).
// OLLAMA_URL empty is the expected, safe default — no AI assistance
// anywhere in the app, same "every caller already treats a disabled
// client as unavailable, never an error" contract webauthConfigFromEnv's
// own doc comment describes for auth.
func aiClientFromEnv() (client *aiassist.Client, url, model string) {
	url = os.Getenv("OLLAMA_URL")
	model = os.Getenv("OLLAMA_MODEL")
	if model == "" {
		model = defaultOllamaModel
	}
	return aiassist.NewClient(url, model), url, model
}

// speechClientFromEnv builds a speechassist.Client from WHISPER_URL —
// same "empty is the expected, safe default" contract as
// aiClientFromEnv, matching the platform's reference self-hosted
// Whisper ASR deployment this was verified end-to-end against.
func speechClientFromEnv() (client *speechassist.Client, url string) {
	url = os.Getenv("WHISPER_URL")
	return speechassist.NewClient(url), url
}

// defaultBlobStorageRoot is used when BLOB_STORAGE_ROOT is unset — a
// relative path under the process's own working directory, so a
// deployment gets a working attachment store with zero configuration
// (ADR-0024's "self-hosted, no external credential" posture) rather than
// the feature being silently off until someone sets an env var. A real
// deployment mounting persistent storage should set BLOB_STORAGE_ROOT
// explicitly to that mount, the same way it already must for Postgres
// itself.
const defaultBlobStorageRoot = "./data/attachments"

// blobstoreFromEnv builds the filesystem-backed blob store from
// BLOB_STORAGE_ROOT (ADR-0024). Unlike aiClientFromEnv/
// speechClientFromEnv, there is no legitimate "disabled" state here —
// blob storage needs no external credential — so this returns an error
// (fatal at boot, same as a bad DATABASE_URL) rather than a nil-safe
// client, and the resolved root is returned alongside purely for the
// startup log line.
func blobstoreFromEnv() (store *blobstore.FSStore, root string, err error) {
	root = os.Getenv("BLOB_STORAGE_ROOT")
	if root == "" {
		root = defaultBlobStorageRoot
	}
	store, err = blobstore.NewFSStore(root)
	return store, root, err
}

// secretCryptorFromEnv builds a secretcrypt.Cryptor from
// SECRET_ENCRYPTION_KEY — a base64-encoded 32-byte AES-256 key. Empty is
// the expected, safe default on a deployment that hasn't opted into the
// AI-provider BYOK settings page (internal/api/aiprovidersettings.go):
// that page itself refuses to store a tenant's own API key at all while
// this is disabled, rather than ever falling back to storing one
// unencrypted. A non-empty but malformed value is a real configuration
// error and fails startup loudly, same reasoning webauthConfigFromEnv's
// own doc comment gives for not silently ignoring a bad OIDC_* value.
func secretCryptorFromEnv() *secretcrypt.Cryptor {
	cryptor, err := secretcrypt.NewCryptor(os.Getenv("SECRET_ENCRYPTION_KEY"))
	if err != nil {
		log.Fatalf("configure secret encryption: %v", err)
	}
	return cryptor
}

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
	if raw := os.Getenv("WORKFLOW_DEPRECIATION_POST_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			cfg.DepreciationPostInterval = d
		} else {
			log.Printf("WORKFLOW_DEPRECIATION_POST_INTERVAL=%q is not a valid duration, using default", raw)
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

// svcauthConfigFromEnv builds a svcauth.Config for machine-to-machine
// API auth (connector plugins — see INTEGRATIONS.md and
// internal/svcauth's own doc comment). Reuses OIDC_ISSUER_URL — the
// same Zitadel instance webauth authenticates humans against also
// introspects a connector's own Bearer token — plus two new
// credentials for the introspection call itself
// (uc-infra/infra/terraform/zitadel's zitadel_application_api
// "Universal Core Introspection"). Every field empty is the expected,
// safe default (Enabled() is false) until that Terraform is actually
// applied.
func svcauthConfigFromEnv() svcauth.Config {
	return svcauth.Config{
		IssuerURL:    os.Getenv("OIDC_ISSUER_URL"),
		ClientID:     os.Getenv("SVC_INTROSPECTION_CLIENT_ID"),
		ClientSecret: os.Getenv("SVC_INTROSPECTION_CLIENT_SECRET"),
	}
}

func main() {
	// DATABASE_URL is the control-plane database (ADR-0003) — the
	// tenants registry, not any tenant's own data. Every tenant's own
	// database lives on the same Postgres server (only dbname differs;
	// host/user/password/sslmode carry over unchanged — see
	// internal/tenantdb.Router), resolved per request/job through
	// router below, never held as a single shared *sql.DB the way this
	// process's own database connection used to be.
	controlDBURL := os.Getenv("DATABASE_URL")
	if controlDBURL == "" {
		log.Fatal("DATABASE_URL is required (the control-plane database — see this file's own doc comment)")
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := controlDB.PingContext(r.Context()); err != nil {
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
	auth, err := webauth.New(context.Background(), webauthCfg, data.NewTenantRepo(controlDB))
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
		log.Printf("no auth backend configured — /api and /forms routes will 401 every request")
	}
	svcCfg := svcauthConfigFromEnv()
	svc, err := svcauth.New(context.Background(), svcCfg, data.NewTenantRepo(controlDB))
	if err != nil {
		log.Fatalf("configure svcauth: %v", err)
	}
	if svc.Enabled() {
		log.Printf("svcauth: machine-to-machine API auth enabled (issuer=%s) — a connector's Bearer access token authenticates via token introspection, see INTEGRATIONS.md", svcCfg.IssuerURL)
	}
	catalog, err := i18n.Load("en")
	if err != nil {
		log.Fatalf("load i18n catalog: %v", err)
	}
	// webauth's own pages (the tenant picker, #25) localize through the
	// same catalog; nil-safe setter so test/e2e wiring stays untouched.
	auth.SetI18n(catalog)
	ai, ollamaURL, ollamaModel := aiClientFromEnv()
	if ai.Enabled() {
		log.Printf("AI assistance enabled (OLLAMA_URL=%s, model=%s) — import mapping suggestions", ollamaURL, ollamaModel)
	}
	speech, whisperURL := speechClientFromEnv()
	if speech.Enabled() {
		log.Printf("Voice transcription enabled (WHISPER_URL=%s) — issue logger voice notes", whisperURL)
	}
	secretCryptor := secretCryptorFromEnv()
	if secretCryptor.Enabled() {
		log.Printf("Secret encryption enabled — tenants may configure their own AI provider API keys")
	} else {
		log.Printf("SECRET_ENCRYPTION_KEY not set — the AI-provider settings page will refuse to store a tenant API key")
	}
	handler := api.New(router, catalog, auth, svc, ai, speech, secretCryptor)
	blobStore, blobRoot, err := blobstoreFromEnv()
	if err != nil {
		log.Fatalf("initialize blob storage (BLOB_STORAGE_ROOT=%q): %v", blobRoot, err)
	}
	handler.SetBlobstore(blobStore)
	log.Printf("attachment storage: filesystem-backed at %s (ADR-0024)", blobRoot)
	// Runtime member management (universal-core#3, ADR-0010): the
	// /settings/members page's outbound Zitadel client. Issuer reuses
	// OIDC_ISSUER_URL — one identity instance for login and management;
	// PAT and project id arrive from the deployment's secret store. Any
	// field empty ⇒ nil client ⇒ the page renders its "unavailable"
	// state, nothing else is affected.
	memberMgmt := zitadelmgmt.NewClient(zitadelmgmt.Config{
		PAT:       os.Getenv("ZITADEL_MGMT_PAT"),
		Issuer:    os.Getenv("OIDC_ISSUER_URL"),
		ProjectID: os.Getenv("ZITADEL_PROJECT_ID"),
	})
	handler.SetMemberMgmt(memberMgmt, data.NewTenantRepo(controlDB))
	// Also the slug directory for /t/{slug} path-prefix tenancy (#25,
	// ADR-0011) — same repo SetMemberMgmt wired; the explicit setter
	// keeps prefix routing independent of member management being on.
	handler.SetTenantRepo(data.NewTenantRepo(controlDB))
	if memberMgmt.Enabled() {
		log.Printf("member management enabled — /settings/members manages Zitadel users/grants at runtime (ADR-0010)")
	} else {
		log.Printf("ZITADEL_MGMT_PAT/ZITADEL_PROJECT_ID not set — /settings/members will render its unavailable state")
	}
	// The real composition root for lifecycle hooks (internal/kernel/
	// crud.Hook) — internal/api itself never imports purchasing/sales
	// directly, keeping that package entity-agnostic (its own doc
	// comment); this is the one place concrete kernel modules are named
	// to wire a hook in, the same role cmd/provision-tenant's own
	// modulePublishers map already plays for module publishing.
	handler.RegisterHook("GoodsReceiptLine", purchasing.PostGoodsReceiptLineToLedger)
	handler.RegisterHook("CustomerInvoice", sales.PostCustomerInvoiceToLedger)
	handler.RegisterHook("VendorInvoice", purchasing.MatchVendorInvoiceOnUpdate)
	handler.RegisterHook("StockTransfer", purchasing.ValidateStockTransfer)
	handler.Routes(mux)

	// The durable workflow job queue (internal/kernel/workflow.Queue) has
	// existed since the definition-registry increment, but nothing ever
	// actually ran it — RegistryDefinitionLookup's own doc comment called
	// this out as "a worker process, not built yet." Wire it in: it runs
	// for the lifetime of the process, alongside the HTTP server, with no
	// graceful-shutdown handling yet (consistent with ListenAndServe below,
	// which doesn't have any either — ctx here is process lifetime, not a
	// signal-driven one, on purpose, so this doesn't silently change how
	// the process responds to SIGINT/SIGTERM while that's still unhandled
	// everywhere else in this binary). Fans out across every tenant's own
	// database each tick (ADR-0003) — see internal/worker's own doc
	// comment.
	workerRunner := worker.New(router, controlDB, nil, workerConfigFromEnv())
	workerRunner.RunConcurrent(context.Background())
	log.Printf("workflow worker started")

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	log.Printf("universal-core kernel listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
