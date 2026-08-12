// Smoke tests for the real compiled cmd/universal-core binary — starts
// it as a real subprocess against a real Postgres control database and
// drives real HTTP requests at it, the way an operator or a load
// balancer health check actually would. Everything else in this repo
// tests api.Routes via httptest.Server wired directly to the handler
// (internal/api, internal/e2e); this is the one layer that proves the
// binary itself — flag/env parsing, migrations-on-boot, the real
// net/http.ListenAndServe — actually works end to end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "universal-core")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// freePort asks the OS for an unused TCP port on localhost. There is a
// small window between closing this listener and the subprocess binding
// the same port, same tradeoff every "find a free port for a test
// server" helper in the Go ecosystem accepts.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startServer launches the real compiled binary against controlDSN,
// waits for /healthz to respond (up to 10s — schema migrations run on
// boot, so this isn't instant), and registers a cleanup that terminates
// it. Extra env vars (e.g. INSECURE_DEV_AUTH=true) are appended to the
// process's own DATABASE_URL/LISTEN_ADDR.
func startServer(t *testing.T, controlDSN string, extraEnv ...string) (baseURL string) {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	env := append([]string{"DATABASE_URL=" + controlDSN, "LISTEN_ADDR=" + addr}, extraEnv...)

	cmd := exec.Command(binPath)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", binPath, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	baseURL = "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL
			}
			lastErr = fmt.Errorf("healthz status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server never became healthy: %v\nstderr:\n%s", lastErr, stderr.String())
	return ""
}

func TestUniversalCore_MissingDatabaseURL_FailsFast(t *testing.T) {
	cmd := exec.Command(binPath)
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit with DATABASE_URL unset")
	}
	if !strings.Contains(string(out), "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got output: %q", out)
	}
}

func TestUniversalCore_Healthz_RespondsOK(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN)

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("unexpected /healthz body: %s", body)
	}

	// The binary applies control-plane migrations on boot (ADR-0003) —
	// confirm the tenants table actually exists, not just that the
	// process happened to answer HTTP.
	control := testexec.Open(t, controlDSN)
	var exists bool
	if err := control.QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'tenants')`,
	).Scan(&exists); err != nil {
		t.Fatalf("check tenants table: %v", err)
	}
	if !exists {
		t.Fatal("expected control-plane migrations to have run on boot")
	}
}

func TestUniversalCore_NoAuthBackend_RecordsAPI401s(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN)

	resp, err := http.Get(baseURL + "/api/records/Party")
	if err != nil {
		t.Fatalf("GET /api/records/Party: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth backend configured, got %d", resp.StatusCode)
	}
}

func TestUniversalCore_DevAuthEnabled_RequiresHeaders(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	// Fails closed (DevAuth's own doc comment) even though the stopgap
	// is enabled, because no X-Tenant-ID/X-Actor-ID headers were sent.
	resp, err := http.Get(baseURL + "/api/records/Party")
	if err != nil {
		t.Fatalf("GET /api/records/Party: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with INSECURE_DEV_AUTH=true but no headers, got %d", resp.StatusCode)
	}
}

func TestUniversalCore_DevAuthEnabled_ValidHeaders_ServesRecordsAPI(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "Server Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	// A dedicated connection, not `control` (which this test closes
	// manually below) — DropTenantDatabase's cleanup runs at the very
	// end of the test and needs its own still-open connection then.
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	if err := foundation.Publish(ctx, tenantDB, audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	router.Close()
	control.Close()

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/records/Party", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "smoke-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/records/Party: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with valid dev-auth headers against a provisioned tenant, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"data"`) {
		t.Fatalf("expected the standard {data,error} envelope, got: %s", body)
	}

	// Department is one of the two org-chart entities added alongside
	// Party in foundation.All() — this confirms the real compiled binary
	// actually serves it too, not just Party (the one entity type every
	// other smoke assertion in this file already covered).
	deptReq, err := http.NewRequest(http.MethodGet, baseURL+"/api/records/Department", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	deptReq.Header.Set("X-Tenant-ID", tenantID)
	deptReq.Header.Set("X-Actor-ID", "smoke-test")
	deptResp, err := http.DefaultClient.Do(deptReq)
	if err != nil {
		t.Fatalf("GET /api/records/Department: %v", err)
	}
	defer deptResp.Body.Close()
	deptBody, _ := io.ReadAll(deptResp.Body)
	if deptResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /api/records/Department, got %d: %s", deptResp.StatusCode, deptBody)
	}
	if !strings.Contains(string(deptBody), `"data"`) {
		t.Fatalf("expected the standard {data,error} envelope for Department, got: %s", deptBody)
	}

	// Delegation (uc-infra#8) is the newest foundation.All() entity —
	// same "confirm the real compiled binary actually serves this one
	// too" check Department got when it landed.
	delReq, err := http.NewRequest(http.MethodGet, baseURL+"/api/records/Delegation", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	delReq.Header.Set("X-Tenant-ID", tenantID)
	delReq.Header.Set("X-Actor-ID", "smoke-test")
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("GET /api/records/Delegation: %v", err)
	}
	defer delResp.Body.Close()
	delBody, _ := io.ReadAll(delResp.Body)
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /api/records/Delegation, got %d: %s", delResp.StatusCode, delBody)
	}
	if !strings.Contains(string(delBody), `"data"`) {
		t.Fatalf("expected the standard {data,error} envelope for Delegation, got: %s", delBody)
	}
}

// TestUniversalCore_MembersRoutes_RegisteredAndGated is the smoke layer
// for universal-core#3: the real compiled binary registers the
// /settings/members routes (a 401 from the auth wrapper, not a bare
// 404) even when the member-management client is unconfigured — the
// route always exists, the page itself degrades to its "unavailable"
// state behind auth (see internal/api/members.go; the page-level
// behavior is covered by internal/api and internal/e2e, this asserts
// the wiring in main.go actually registers it on the production mux).
func TestUniversalCore_MembersRoutes_RegisteredAndGated(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	for _, route := range []struct{ method, path string }{
		{"GET", "/settings/members"},
		{"POST", "/settings/members/invite"},
		{"POST", "/settings/members/remove"},
		{"POST", "/settings/members/password-link"},
		{"POST", "/settings/members/password-email"},
		{"POST", "/settings/members/roles/assign"},
		{"POST", "/settings/members/roles/revoke"},
	} {
		req, err := http.NewRequest(route.method, baseURL+route.path, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", route.method, route.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", route.method, route.path, err)
		}
		resp.Body.Close()
		// No dev-auth headers sent: DevAuth fails closed with 401 — the
		// route being registered is exactly what distinguishes this from
		// the mux's own 404.
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 (registered, auth-gated), got %d", route.method, route.path, resp.StatusCode)
		}
	}
}

// TestUniversalCore_TenantPrefixRoute_RegisteredAndGated: the /t/{slug}/
// subtree (#25, ADR-0011) is registered on the production mux and sits
// behind the same fail-closed auth as everything else — 401 without
// credentials, not a 404 (route missing) and not a 200 (auth bypass).
func TestUniversalCore_TenantPrefixRoute_RegisteredAndGated(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	resp, err := http.Get(baseURL + "/t/some-tenant/records/Item")
	if err != nil {
		t.Fatalf("GET /t/…: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 (registered, auth-gated), got %d", resp.StatusCode)
	}
}

// TestUniversalCore_ReportRoutes_RegisteredAndGated is the smoke layer
// for the report pages — the RFQ vendor-comparison report (#9) and the
// purchasing report it shares requireReportRead with: the real compiled
// binary registers both on the production mux (401 from the auth
// wrapper, not the mux's own 404) rather than only internal/api's
// httptest mux knowing about them. The RFQ route is path-parameterised
// (/reports/rfq/{id}), so a bare registration bug there would surface as
// a 404 for every id — worth pinning at the binary level, since nothing
// in the UI links to it yet for a human to notice.
func TestUniversalCore_ReportRoutes_RegisteredAndGated(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	for _, path := range []string{
		"/reports/purchasing",
		"/reports/rfq/00000000-0000-0000-0000-000000000000",
	} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s: expected 401 (registered, auth-gated), got %d", path, resp.StatusCode)
		}
	}
}

// TestUniversalCore_HelpRoutes_RegisteredAndGated is the smoke layer for
// the in-product help manual viewer (ADR-0023, uc-infra#144): the real
// compiled binary registers /help, /help/search, and the
// /help/{topicID...} wildcard on the production mux (a 401 from the
// auth wrapper, not the mux's own 404) — not just internal/api's
// httptest mux. /help/search is asserted separately from a real topic
// id specifically to catch a registration-order/specificity regression
// that could route the literal "search" segment into the
// {topicID...} wildcard instead of its own literal handler.
func TestUniversalCore_HelpRoutes_RegisteredAndGated(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	for _, path := range []string{
		"/help",
		"/help/search",
		"/help/entity/Item",
	} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		// No dev-auth headers sent: DevAuth fails closed with 401 — the
		// route being registered (and reachable, not swallowed by the
		// mux's default 404) is exactly what this proves.
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s: expected 401 (registered, auth-gated), got %d", path, resp.StatusCode)
		}
	}
}

// TestUniversalCore_HelpAssetRoute_ServedUnauthenticated is the smoke
// layer for the help-screenshot static asset route (uc-infra#145,
// independent review finding): unlike every path in
// TestUniversalCore_HelpRoutes_RegisteredAndGated above, GET
// /help/assets/{path...} must succeed on the real compiled binary WITH
// NO dev-auth headers at all — that's the whole point of registering it
// unauthenticated (internal/api/helpassets.go's own doc comment). The
// route-precedence claim ("assets" outranks the /help/{topicID...}
// wildcard) was previously only verified against an in-process
// http.NewServeMux built inside internal/api's own tests, never against
// the production mux this binary actually serves — this closes that gap
// at the smoke layer CLAUDE.md requires ("a real compiled binary/server
// actually starts, responds, and serves the routes it claims to").
func TestUniversalCore_HelpAssetRoute_ServedUnauthenticated(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	// No dev-auth headers sent, deliberately — a 401 here would mean this
	// route fell through to authPage(renderHelpTopic) instead of landing
	// on serveHelpAsset.
	resp, err := http.Get(baseURL + "/help/assets/list/en.jpg")
	if err != nil {
		t.Fatalf("GET /help/assets/list/en.jpg: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /help/assets/list/en.jpg (no auth headers) = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want %q (would be text/html from the page shell if this fell through to renderHelpTopic)", ct, "image/jpeg")
	}
}

// TestUniversalCore_HelpRoute_ServedByRealBinary is the "authenticated,
// actually renders" counterpart to
// TestUniversalCore_HelpRoutes_RegisteredAndGated above (which only
// proves the route exists and is auth-gated, not that a real logged-in
// request gets a real page back) — the same "RegisteredAndGated" vs.
// "ServedByRealBinary" split this file already uses for SAF-T/UBL/stock
// transfer. No real help content ships yet (internal/help/content/ is
// still just its own README.md, uc-infra#147-152's scope), so this
// confirms the honest content-free state renders correctly against a
// real running process — the two-pane shell (topic tree container,
// search box, detail pane) present with a 200, not just a 401 boundary
// check or an in-process httptest.Server the way internal/api/help_test.go
// already covers this.
func TestUniversalCore_HelpRoute_ServedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "Help Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	if err := foundation.Publish(ctx, tenantDB, audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	router.Close()
	control.Close()

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	req, err := http.NewRequest(http.MethodGet, baseURL+"/help", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "smoke-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /help: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /help against the real running binary, got %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`id="uc-help-topics"`, `id="uc-help-search"`, `id="uc-help-detail"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("expected %s in the real binary's /help response, got: %.500s", want, body)
		}
	}
}

// TestUniversalCore_SAFTExportRoutes_ServedByRealBinary is the smoke
// layer for the SAF-T export (universaltill/uc-infra#28): the real
// compiled binary registers GET /export/saft (an actual XML audit file
// for a provisioned tenant with foundation + finance published) and its
// /export/saft/form page — not just the httptest-level wiring
// internal/api's own tests cover.
func TestUniversalCore_SAFTExportRoutes_ServedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "Server Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}
	router.Close()
	control.Close()

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := get("/export/saft?from=2026-01-01&to=2026-12-31")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /export/saft, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("expected application/xml from /export/saft, got %q", ct)
	}
	if !strings.HasPrefix(body, "<?xml") || !strings.Contains(body, "<AuditFile") {
		t.Fatalf("expected a SAF-T XML document, got: %.200s", body)
	}

	formResp, formBody := get("/export/saft/form")
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /export/saft/form, got %d: %s", formResp.StatusCode, formBody)
	}
	if !strings.Contains(formBody, `action="/export/saft"`) {
		t.Fatalf("expected the export form on the page, got: %.300s", formBody)
	}
}

// The smoke layer for the UBL export (universaltill/uc-infra#27): the
// real compiled binary serves GET /export/{entityType}/{id}/ubl with a
// real UBL Order document for a real PurchaseOrder — and the handler
// (not the mux) answers for unsupported entity types.
func TestUniversalCore_UBLExportRoute_ServedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "UBL Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	defs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := defs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	create := func(entityType string, fields map[string]any) string {
		t.Helper()
		rec, err := engine.Create(ctx, def(entityType), fields, actor)
		if err != nil {
			t.Fatalf("create %s: %v", entityType, err)
		}
		return rec.ID
	}
	vendorID := create("Party", map[string]any{"party_type": "organization", "name": "Smoke Vendor", "status": "active"})
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	create("PartyRole", map[string]any{"party_id": vendorID, "role_type": "vendor"})
	currencyID := create("Currency", map[string]any{"code": "USD", "name": "US Dollar", "minor_unit": 2.0})
	itemID := create("Item", map[string]any{"sku": "SMOKE-1", "name": "Smoke Widget", "item_type": "stock"})
	statusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "purchase_order_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list purchase_order_status StatusType: %v (n=%d)", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", statusTypes[0].ID)
	if err != nil || len(statuses) == 0 {
		t.Fatalf("list Status: %v (n=%d)", err, len(statuses))
	}
	// total/unit_price/line_total are FieldMoney now (uc-infra#136): 1000
	// minor units = $10.00.
	poID := create("PurchaseOrder", map[string]any{
		"po_number": "PO-SMOKE-1", "vendor_id": vendorID, "order_date": "2026-07-01",
		"currency_id": currencyID, "status_id": statuses[0].ID, "total": 1000,
	})
	create("POLine", map[string]any{
		"purchase_order_id": poID, "item_id": itemID, "qty": 1.0, "unit_price": 1000, "line_total": 1000,
	})
	router.Close()
	control.Close()

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := get("/export/PurchaseOrder/" + poID + "/ubl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from UBL export, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("expected application/xml, got %q", ct)
	}
	if !strings.HasPrefix(body, "<?xml") || !strings.Contains(body, "<Order") || !strings.Contains(body, "PO-SMOKE-1") {
		t.Fatalf("expected a UBL Order document, got: %.300s", body)
	}

	// An unsupported entity type is answered by the handler's own 404
	// message — proof the route pattern reached exportUBL, not the mux's
	// generic not-found.
	unsupResp, unsupBody := get("/export/Party/" + vendorID + "/ubl")
	if unsupResp.StatusCode != http.StatusNotFound || !strings.Contains(unsupBody, "UBL export is not available") {
		t.Fatalf("expected handler 404 for unsupported type, got %d: %s", unsupResp.StatusCode, unsupBody)
	}
}

// TestUniversalCore_StockTransferRoutes_ServedByRealBinary is the smoke
// layer for uc-infra#13: the real compiled binary (not just internal/api's
// own httptest-router unit tests) actually serves /api/records/StockTransfer
// and /forms/StockTransfer/new, and the StockTransfer hook registered in
// this file's own main() (handler.RegisterHook("StockTransfer",
// purchasing.ValidateStockTransfer)) is really wired on the
// production mux — a same-facility create is rejected as a 400 through
// the real running server, not just through internal/api's own
// TestAPI_CreateRecord_HookRejectionIs400 (a generic proof of the
// mapping mechanism with a throwaway hook, not proof this specific hook
// is actually registered in main()).
func TestUniversalCore_StockTransferRoutes_ServedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "StockTransfer Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	defs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := defs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	create := func(entityType string, fields map[string]any) string {
		t.Helper()
		rec, err := engine.Create(ctx, def(entityType), fields, actor)
		if err != nil {
			t.Fatalf("create %s: %v", entityType, err)
		}
		return rec.ID
	}
	itemID := create("Item", map[string]any{"sku": "SMOKE-ST-1", "name": "Smoke Widget", "item_type": "stock"})
	facilityAID := create("Facility", map[string]any{"code": "WH-SMOKE-A", "name": "Smoke Warehouse A", "facility_type": "warehouse"})
	facilityBID := create("Facility", map[string]any{"code": "WH-SMOKE-B", "name": "Smoke Warehouse B", "facility_type": "warehouse"})
	statusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "stock_transfer_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list stock_transfer_status StatusType: %v (n=%d)", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", statusTypes[0].ID)
	if err != nil || len(statuses) == 0 {
		t.Fatalf("list Status: %v (n=%d)", err, len(statuses))
	}
	var draftStatusID string
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code == "draft" {
			draftStatusID = s.ID
		}
	}
	if draftStatusID == "" {
		t.Fatal("no draft Status found for stock_transfer_status")
	}
	router.Close()
	control.Close()

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	post := func(path, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp, string(respBody)
	}

	listResp, listBody := get("/api/records/StockTransfer")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/records/StockTransfer, got %d: %s", listResp.StatusCode, listBody)
	}
	if !strings.Contains(listBody, `"data"`) {
		t.Fatalf("expected the standard {data,error} envelope, got: %s", listBody)
	}

	formResp, formBody := get("/forms/StockTransfer/new")
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /forms/StockTransfer/new, got %d: %s", formResp.StatusCode, formBody)
	}
	if !strings.Contains(formBody, `hx-post="/api/records/StockTransfer"`) {
		t.Fatalf("expected the new-record form to post to /api/records/StockTransfer, got: %.300s", formBody)
	}

	// The hook wired in main() actually fires on the real running
	// server: a same-facility transfer is rejected as a 400, not a raw
	// 500 (which is exactly what would happen if crud.ErrHookRejected
	// weren't checked in writeCrudError, or the hook weren't registered
	// at all).
	rejectBody := fmt.Sprintf(`{"item_id":%q,"from_facility_id":%q,"to_facility_id":%q,"qty":5,"transfer_date":"2026-08-01","status_id":%q}`,
		itemID, facilityAID, facilityAID, draftStatusID)
	rejectResp, rejectRespBody := post("/api/records/StockTransfer", rejectBody)
	if rejectResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a same-facility StockTransfer, got %d: %s", rejectResp.StatusCode, rejectRespBody)
	}

	// A valid create actually persists through the real server + hook.
	validBody := fmt.Sprintf(`{"item_id":%q,"from_facility_id":%q,"to_facility_id":%q,"qty":5,"transfer_date":"2026-08-01","status_id":%q}`,
		itemID, facilityAID, facilityBID, draftStatusID)
	validResp, validRespBody := post("/api/records/StockTransfer", validBody)
	if validResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for a valid StockTransfer, got %d: %s", validResp.StatusCode, validRespBody)
	}
}

// TestUniversalCore_Account_SavedByRealBinarySyncsGLAccounts is the smoke
// layer for uc-infra#204: the specific handler.RegisterHook("Account",
// finance.SyncGLAccountOnWrite) call this file's own main() adds is
// really wired on the production mux, not just proven generically by
// internal/api's own hook-mapping tests or by another entity's hook here
// (TestUniversalCore_StockTransferRoutes_ServedByRealBinary's own doc
// comment names this exact gap: "not proof this specific hook is
// actually registered in main()" — independent review flagged that this
// card had no such test for its own hook).
//
// gl_accounts (the ledger core's own typed table, ADR-0004) has no
// generic /api/records surface to read it back through — unlike every
// other smoke test in this file, which talks only to the running server
// from the point setup finishes (see
// TestUniversalCore_AttendanceRecordUniqueConstraint_EnforcedByRealBinary's
// own comment on why), this one keeps its setup tenantDB connection open
// deliberately, as the one honest way to observe a deterministic-core
// table's state — same reasoning ADR-0004 already gives for why
// journal_entries only gets a "dedicated, hand-built read-only report
// page," not a generic list. The real HTTP create is still what's under
// test; the DB read is only the assertion.
func TestUniversalCore_Account_SavedByRealBinarySyncsGLAccounts(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "Account Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	post := func(path, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp, string(respBody)
	}

	createBody := `{"code":"1000","name":"Assets","type":"asset","is_active":true}`
	createResp, createRespBody := post("/api/records/Account", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating Account via the real running server, got %d: %s", createResp.StatusCode, createRespBody)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, isActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("expected a gl_accounts row for code 1000 right after the real server's create — RegisterHook(\"Account\", ...) may not be wired in main(): %v", err)
	}
	if !isActive {
		t.Fatal("expected the synced gl_account to be active")
	}
	if id == "" {
		t.Fatal("expected a real gl_accounts id")
	}
}

// TestUniversalCore_AttendanceRecordUniqueConstraint_EnforcedByRealBinary
// is the smoke layer for uc-infra#81: the real compiled binary, talking
// to a real Postgres tenant database, actually rejects a second
// AttendanceRecord for the same (employee_id, entry_date) — not just the
// in-process httptest coverage internal/api and internal/kernel/crud
// already have. This is the "double-submit" case CLAUDE.md's testing
// rule asks for: a real end user double-clicking Save, or a retried
// request, landing on a real running server.
func TestUniversalCore_AttendanceRecordUniqueConstraint_EnforcedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "AttendanceRecord Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := hr.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("hr.Publish: %v", err)
	}
	router.Close()
	control.Close()

	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true")

	post := func(path, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp, string(respBody)
	}

	// employee_id is a dangling reference (no real Employee record) —
	// tolerated (ADR-0007, same as every other FieldReference with no
	// TargetFilter/MustMatchParentField), and irrelevant to what this
	// test is actually proving.
	body := `{"employee_id":"00000000-0000-0000-0000-0000000000e1","entry_date":"2026-08-01","hours_worked":8,"source":"manual"}`

	firstResp, firstBody := post("/api/records/AttendanceRecord", body)
	if firstResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for the first AttendanceRecord, got %d: %s", firstResp.StatusCode, firstBody)
	}

	secondResp, secondBody := post("/api/records/AttendanceRecord", body)
	if secondResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a duplicate (employee_id, entry_date) double-submit, got %d: %s", secondResp.StatusCode, secondBody)
	}
	if !strings.Contains(secondBody, "already used by another record") {
		t.Fatalf("expected the translated unique-constraint message, got: %s", secondBody)
	}

	// Asserted through the real running server's own API, not a direct DB
	// query — router/control are already closed above (same reasoning
	// TestUniversalCore_StockTransferRoutes_ServedByRealBinary's post/get
	// helpers give: from this point on, the server process is the only
	// thing this test talks to).
	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Actor-ID", "smoke-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp, string(respBody)
	}
	listResp, listBody := get("/api/records/AttendanceRecord")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/records/AttendanceRecord, got %d: %s", listResp.StatusCode, listBody)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listBody), &list); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("expected exactly 1 AttendanceRecord after the rejected double-submit, got %d: %s", len(list.Data), listBody)
	}
}

// TestUniversalCore_AttachmentUploadDownload_ServedByRealBinary is the
// smoke layer for uc-infra#142 (ADR-0024): the real compiled binary,
// wired to a real filesystem-backed blob store via BLOB_STORAGE_ROOT,
// actually serves the generic attachment upload/download routes end to
// end — proving main.go's wiring (blobstoreFromEnv + SetBlobstore), not
// just internal/api's own in-process handler tests.
func TestUniversalCore_AttachmentUploadDownload_ServedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "Attachment Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	router.Close()
	control.Close()

	blobRoot := t.TempDir()
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true", "BLOB_STORAGE_ROOT="+blobRoot)

	// foundation.Publish doesn't itself create any Party rows — create
	// one directly through the real running server (the target record
	// this attachment attaches to), the same way any other smoke test in
	// this file drives record creation.
	createPartyReq, err := http.NewRequest(http.MethodPost, baseURL+"/api/records/Party",
		strings.NewReader(`{"party_type":"organization","name":"Attachment Smoke Test Org"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	createPartyReq.Header.Set("X-Tenant-ID", tenantID)
	createPartyReq.Header.Set("X-Actor-ID", "smoke-test")
	createPartyReq.Header.Set("Content-Type", "application/json")
	createPartyResp, err := http.DefaultClient.Do(createPartyReq)
	if err != nil {
		t.Fatalf("POST /api/records/Party: %v", err)
	}
	createPartyBody, _ := io.ReadAll(createPartyResp.Body)
	createPartyResp.Body.Close()
	if createPartyResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating the target Party, got %d: %s", createPartyResp.StatusCode, createPartyBody)
	}
	var createdParty struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createPartyBody, &createdParty); err != nil {
		t.Fatalf("unmarshal create-Party response: %v", err)
	}

	fileContent := []byte("real binary smoke test attachment contents")
	var uploadBuf bytes.Buffer
	mw := multipart.NewWriter(&uploadBuf)
	fw, err := mw.CreateFormFile("file", "smoke.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(fileContent); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	uploadReq, err := http.NewRequest(http.MethodPost, baseURL+"/api/attachments/Party/"+createdParty.Data.ID, &uploadBuf)
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	uploadReq.Header.Set("X-Tenant-ID", tenantID)
	uploadReq.Header.Set("X-Actor-ID", "smoke-test")
	uploadReq.Header.Set("Content-Type", mw.FormDataContentType())
	uploadResp, err := http.DefaultClient.Do(uploadReq)
	if err != nil {
		t.Fatalf("POST /api/attachments/Party/%s: %v", createdParty.Data.ID, err)
	}
	uploadBody, _ := io.ReadAll(uploadResp.Body)
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 uploading an attachment, got %d: %s", uploadResp.StatusCode, uploadBody)
	}
	var uploaded struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(uploadBody, &uploaded); err != nil {
		t.Fatalf("unmarshal upload response: %v", err)
	}

	downloadReq, err := http.NewRequest(http.MethodGet, baseURL+"/api/attachments/"+uploaded.Data.ID, nil)
	if err != nil {
		t.Fatalf("build download request: %v", err)
	}
	downloadReq.Header.Set("X-Tenant-ID", tenantID)
	downloadReq.Header.Set("X-Actor-ID", "smoke-test")
	downloadResp, err := http.DefaultClient.Do(downloadReq)
	if err != nil {
		t.Fatalf("GET /api/attachments/%s: %v", uploaded.Data.ID, err)
	}
	defer downloadResp.Body.Close()
	downloadBody, _ := io.ReadAll(downloadResp.Body)
	if downloadResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 downloading the attachment, got %d: %s", downloadResp.StatusCode, downloadBody)
	}
	if !bytes.Equal(downloadBody, fileContent) {
		t.Fatalf("downloaded content mismatch: got %q, want %q", downloadBody, fileContent)
	}

	// The real binary actually wrote through to BLOB_STORAGE_ROOT on
	// disk — not just returning bytes it happened to buffer in memory.
	var foundOnDisk bool
	if walkErr := filepath.Walk(blobRoot, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			foundOnDisk = true
		}
		return nil
	}); walkErr != nil {
		t.Fatalf("walk BLOB_STORAGE_ROOT: %v", walkErr)
	}
	if !foundOnDisk {
		t.Fatalf("expected at least one file under BLOB_STORAGE_ROOT=%s after a successful upload", blobRoot)
	}
}

// TestUniversalCore_IssueReportScreenRecording_ServedByRealBinary is the
// smoke layer for uc-infra#92: the real compiled binary, wired to a real
// filesystem-backed blob store via BLOB_STORAGE_ROOT (the same env var
// TestUniversalCore_AttachmentUploadDownload_ServedByRealBinary already
// proves main.go wires through blobstoreFromEnv), actually accepts a
// screen-recording part on /issue-report/submit and creates a real,
// linked, downloadable Attachment — proving main.go's wiring reaches
// this new code path too, not just internal/api's own in-process
// handler tests (issuereport_test.go) or the generic attachment routes
// this one already covers.
func TestUniversalCore_IssueReportScreenRecording_ServedByRealBinary(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_server_control")

	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	tenantID, err := router.Create(ctx, "Issue Report Screen Recording Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	router.Close()
	control.Close()

	blobRoot := t.TempDir()
	baseURL := startServer(t, controlDSN, "INSECURE_DEV_AUTH=true", "BLOB_STORAGE_ROOT="+blobRoot)

	recording := []byte("real binary smoke test screen recording contents")
	var submitBuf bytes.Buffer
	mw := multipart.NewWriter(&submitBuf)
	for name, value := range map[string]string{
		"title":       "Smoke tested screen recording",
		"description": "Filed by the real binary smoke test.",
	} {
		if err := mw.WriteField(name, value); err != nil {
			t.Fatalf("write field %s: %v", name, err)
		}
	}
	// Not mw.CreateFormFile: it hardcodes Content-Type:
	// application/octet-stream, but a real browser sends the captured
	// File's own "video/webm" type — see issueReportTmpl's
	// `new File([blob], ..., { type: "video/webm" })`.
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", `form-data; name="screen_recording"; filename="screen-recording.webm"`)
	partHeader.Set("Content-Type", "video/webm")
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("create screen_recording form part: %v", err)
	}
	if _, err := fw.Write(recording); err != nil {
		t.Fatalf("write screen_recording content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	submitReq, err := http.NewRequest(http.MethodPost, baseURL+"/issue-report/submit", &submitBuf)
	if err != nil {
		t.Fatalf("build submit request: %v", err)
	}
	submitReq.Header.Set("X-Tenant-ID", tenantID)
	submitReq.Header.Set("X-Actor-ID", "smoke-test")
	submitReq.Header.Set("Content-Type", mw.FormDataContentType())
	submitResp, err := http.DefaultClient.Do(submitReq)
	if err != nil {
		t.Fatalf("POST /issue-report/submit: %v", err)
	}
	submitBody, _ := io.ReadAll(submitResp.Body)
	submitResp.Body.Close()
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 submitting the report, got %d: %s", submitResp.StatusCode, submitBody)
	}
	if strings.Contains(string(submitBody), "could not be attached") {
		t.Fatalf("expected the recording to attach successfully, got the not-attached note: %s", submitBody)
	}

	listReq, err := http.NewRequest(http.MethodGet, baseURL+"/api/records/Attachment", nil)
	if err != nil {
		t.Fatalf("build list request: %v", err)
	}
	listReq.Header.Set("X-Tenant-ID", tenantID)
	listReq.Header.Set("X-Actor-ID", "smoke-test")
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("GET /api/records/Attachment: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing Attachment records, got %d: %s", listResp.StatusCode, listBody)
	}
	var attachments struct {
		Data []struct {
			ID   string `json:"id"`
			Data struct {
				EntityType string `json:"entity_type"`
				MimeType   string `json:"mime_type"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listBody, &attachments); err != nil {
		t.Fatalf("unmarshal Attachment list: %v", err)
	}
	var attachmentID string
	for _, a := range attachments.Data {
		if a.Data.EntityType == "IssueReport" {
			attachmentID = a.ID
			if a.Data.MimeType != "video/webm" {
				t.Errorf("expected mime_type video/webm, got %q", a.Data.MimeType)
			}
		}
	}
	if attachmentID == "" {
		t.Fatalf("expected a linked IssueReport Attachment, got: %s", listBody)
	}

	downloadReq, err := http.NewRequest(http.MethodGet, baseURL+"/api/attachments/"+attachmentID, nil)
	if err != nil {
		t.Fatalf("build download request: %v", err)
	}
	downloadReq.Header.Set("X-Tenant-ID", tenantID)
	downloadReq.Header.Set("X-Actor-ID", "smoke-test")
	downloadResp, err := http.DefaultClient.Do(downloadReq)
	if err != nil {
		t.Fatalf("GET /api/attachments/%s: %v", attachmentID, err)
	}
	defer downloadResp.Body.Close()
	downloadBody, _ := io.ReadAll(downloadResp.Body)
	if downloadResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 downloading the recording, got %d: %s", downloadResp.StatusCode, downloadBody)
	}
	if !bytes.Equal(downloadBody, recording) {
		t.Fatalf("downloaded content mismatch: got %q, want %q", downloadBody, recording)
	}
}
