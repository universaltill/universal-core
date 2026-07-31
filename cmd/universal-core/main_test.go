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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
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
