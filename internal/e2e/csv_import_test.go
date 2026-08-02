// Package e2e is the browser-driven end-to-end test harness flagged
// repeatedly in erp/QUEUE.md since 2026-07-19 (Farshid, directly: "do you
// have the rule of having tests end to end even the ui completely?"):
// internal/kernel/formrender's tests parse rendered HTML *strings* in Go
// — real coverage of the markup, but not the same thing as a real
// browser driving real htmx swaps against a real running server. That
// was blocked on there being no HTTP handler serving any page at all;
// now that /import and /forms are real, this closes the gap.
//
// Uses chromedp (real headless Chrome via the DevTools Protocol) rather
// than Playwright specifically to stay pure-Go — this repo has no
// Node.js toolchain anywhere else, and adding one just for E2E tests
// would be a second language/package-manager surface for a Go kernel.
// Skips (not fails) when TEST_DATABASE_URL isn't set, same convention
// as every other integration test in this repo, plus a second skip if
// no Chrome/Chromium binary is found and CI isn't set — this test needs
// a real browser, which isn't guaranteed on every machine running
// `go test ./...` (ubuntu-latest GitHub Actions runners ship Chrome
// pre-installed; verified in CI, not just assumed). In CI itself
// (detected via the CI env var GitHub Actions and most CI systems set),
// a missing browser is a hard failure, not a skip — see
// browserMissingAction below. Found 2026-08-02, via uc-infra#56's
// coverage-gate independent review: a coverage baseline measured on a
// machine with no browser silently skipped all 49 of this package's
// tests and would have gone uncaught the same way in CI if its ambient
// Chrome ever disappeared.
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

// freshControlDB returns a connection to a brand-new, uniquely-named
// control-plane database (ADR-0003) with the control migration set
// applied — same pattern internal/api/handlers_test.go's own
// freshControlDB establishes. Skips (not fails) if TEST_DATABASE_URL
// isn't set, same convention as every other integration test in this
// repo.
func freshControlDB(t *testing.T) (controlDB *sql.DB, controlDSN string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_e2e_control_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	dsn := u.String()
	control, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open control database %s: %v", name, err)
	}
	t.Cleanup(func() { control.Close() })
	if err := control.Ping(); err != nil {
		t.Fatalf("ping control database %s: %v", name, err)
	}
	if _, err := control.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyControl(context.Background(), control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	return control, dsn
}

// newTestRouter builds a Router against a fresh control database — every
// e2e server in this package is wired against this, exactly as
// cmd/universal-core wires it in production.
func newTestRouter(t *testing.T) *tenantdb.Router {
	t.Helper()
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	return router
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "e2e"}
}

// withDevAuthEnabled mirrors internal/api's own test helper of the same
// name: httpx.DevAuth fails every request closed unless
// INSECURE_DEV_AUTH is set (see internal/httpx/devauth.go), so the real
// server this package stands up needs it too, restored afterward rather
// than left set for any other test process sharing this environment.
func withDevAuthEnabled(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv("INSECURE_DEV_AUTH")
	os.Setenv("INSECURE_DEV_AUTH", "true")
	t.Cleanup(func() {
		if had {
			os.Setenv("INSECURE_DEV_AUTH", prev)
		} else {
			os.Unsetenv("INSECURE_DEV_AUTH")
		}
	})
}

// findBrowser looks for a Chrome/Chromium binary in the handful of
// places it's actually installed across this repo's real environments —
// GitHub Actions' ubuntu-latest runners (google-chrome-stable on PATH)
// and a local macOS dev machine (Google Chrome.app, not on PATH by
// default). chromedp can locate a browser itself on Linux/PATH, but not
// macOS's .app bundle path, so this is only load-bearing for local
// development; CI never needs the macOS branch.
func findBrowser(t *testing.T) string {
	t.Helper()
	candidates := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	if runtime.GOOS == "darwin" {
		macPath := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(macPath); err == nil {
			return macPath
		}
	}
	if fail, msg := browserMissingAction(os.Getenv("CI")); fail {
		t.Fatal(msg)
	} else {
		t.Skip(msg)
	}
	return ""
}

// browserMissingAction decides what happens when no Chrome/Chromium
// binary is found: a hard failure when CI is set (GitHub Actions and
// most CI systems set this by default), never a silent skip — a runner
// image that quietly drops its browser must not let all 49 of this
// package's tests quietly skip along with it (CLAUDE.md's "CI must
// never silently skip" rule, applied to the browser dependency itself,
// not just the tests that need it). Skips locally so a laptop without
// Chrome installed doesn't block an ordinary `go test ./...`. A pure
// function so the branch is unit-testable without needing a real
// missing-browser environment.
func browserMissingAction(ciEnv string) (fail bool, message string) {
	if ciEnv != "" {
		return true, "no Chrome/Chromium binary found on PATH in CI — internal/e2e's real-browser tests must never silently skip"
	}
	return false, "no Chrome/Chromium binary found; skipping browser E2E test"
}

func TestBrowserMissingAction_CIEnvironment_Fails(t *testing.T) {
	fail, msg := browserMissingAction("true")
	if !fail {
		t.Fatal("expected fail=true when the CI env var is set, got fail=false")
	}
	if msg == "" {
		t.Fatal("expected a non-empty failure message")
	}
}

func TestBrowserMissingAction_NoCIEnvironment_Skips(t *testing.T) {
	fail, msg := browserMissingAction("")
	if fail {
		t.Fatal("expected fail=false when the CI env var is unset, got fail=true")
	}
	if msg == "" {
		t.Fatal("expected a non-empty skip message")
	}
}

// testServer provisions a brand-new tenant (via router.Create — the real
// production provisioning path, same as cmd/provision-tenant) with
// foundation + purchasing published (entities and forms), and starts a
// real HTTP server backed by api.Routes, the same wiring
// cmd/universal-core uses. Each tenant is its own physical database
// (ADR-0003), so — unlike the old shared-database version of this helper
// — there's no cross-test workflow_jobs cleanup needed: a job left behind
// by one test can never be claimed by another test's ProcessOne call.
func testServer(t *testing.T) (srv *httptest.Server, tenantID string, tenantDB *sql.DB) {
	t.Helper()
	router := newTestRouter(t)
	ctx := context.Background()
	actor := humanActor()

	id, err := router.Create(ctx, "E2E Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	// See internal/api's newTestTenant: the tenant's own physical
	// database (ADR-0003) outlives the control database's cleanup and
	// leaked permanently without this.
	testexec.DropConnectedDatabase(t, tenantDB)

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	mux := http.NewServeMux()
	api.New(router, catalog, nil, nil, nil, nil, nil).Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, id, tenantDB
}

// browserCtx starts a real headless Chrome instance and configures every
// request it makes to carry the dev-auth headers httpx.DevAuth requires
// (see internal/httpx/devauth.go) — network.SetExtraHTTPHeaders applies
// to every subsequent request from this page, including htmx's own AJAX
// calls, not just the initial navigation, which is what makes an htmx
// swap actually work against an auth-gated route in this test the same
// way a real logged-in browser session would once real auth exists.
func browserCtx(t *testing.T, tenantID string) context.Context {
	t.Helper()
	execPath := findBrowser(t)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(execPath))...)
	t.Cleanup(cancelAlloc)

	ctx, cancel := chromedp.NewContext(allocCtx)
	t.Cleanup(cancel)

	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancelTimeout)

	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return network.SetExtraHTTPHeaders(network.Headers{
			"X-Tenant-ID": tenantID,
			"X-Actor-ID":  "00000000-0000-0000-0000-0000000000e2",
		}).Do(ctx)
	})); err != nil {
		t.Fatalf("set extra headers: %v", err)
	}
	return ctx
}

// clickAndSettle clicks selector and waits for the resulting htmx swap to
// fully settle (htmx's own "htmx:afterSettle" event) before returning —
// not just for the click to be dispatched.
//
// Found the hard way: WaitVisible on the swapped-in content's own
// selector isn't enough when the element that triggered the swap lives
// *inside* the region being replaced (e.g. this wizard's "Preview
// again"/Commit buttons, which live inside #uc-import-result, the same
// element they swap). htmx's settle step is scheduled via
// requestAnimationFrame, matching its default CSS-transition timing —
// which a headless, non-visible tab doesn't reliably fire promptly for,
// so a click issued immediately after the new DOM content merely
// *appears* can land before htmx has actually bound handlers to it, and
// silently does nothing. A real, actively-rendered browser tab doesn't
// have this delay (confirmed empirically: the same click succeeds
// immediately if the tab has had any time to render a frame, and always
// succeeds after this explicit wait) — this is a headless-testing
// synchronization gap, not a product bug, but a real one worth encoding
// here rather than fixing with an arbitrary sleep.
func clickAndSettle(selector string) chromedp.Action {
	sel, err := json.Marshal(selector)
	if err != nil {
		panic(err) // selector is always a Go string literal from this file
	}
	// Also listens for htmx:afterRequest with detail.successful === false
	// (a non-2xx response, or the request never completing) and rejects
	// immediately with the real status — without this, a server-side
	// regression would otherwise hang this promise for the full 30s
	// context timeout instead of failing fast with a useful message,
	// since htmx's default responseHandling skips the swap (and so never
	// fires htmx:afterSettle) on a failed request.
	expr := fmt.Sprintf(`new Promise((resolve, reject) => {
  const el = document.querySelector(%s);
  if (!el) { reject(new Error("clickAndSettle: no element matching " + %s)); return; }
  function onSettle() { cleanup(); resolve(true); }
  function onAfterRequest(e) {
    if (e.detail.successful === false) {
      cleanup();
      reject(new Error("clickAndSettle: request failed, status " + (e.detail.xhr && e.detail.xhr.status)));
    }
  }
  function cleanup() {
    document.body.removeEventListener('htmx:afterSettle', onSettle);
    document.body.removeEventListener('htmx:afterRequest', onAfterRequest);
  }
  document.body.addEventListener('htmx:afterSettle', onSettle);
  document.body.addEventListener('htmx:afterRequest', onAfterRequest);
  el.click();
})`, sel, sel)
	return chromedp.Evaluate(expr, nil, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})
}

// TestCSVImportWizard_RealBrowser is the regression test — driven by a
// real browser, not curl or a Go-string HTML parse — for the exact bug
// found by manually dogfooding the purchasing module (see
// uc-infra/docs/code-reviews/2026-07-20-purchasing-module.md): a CSV
// upload whose headers don't exactly name-match every required field
// used to make the import wizard unusable, because the mapping-editor
// UI never rendered at all. This proves the fix works through the real
// interaction a user has: pick a file in a real <input type="file">,
// click a real button, watch a real htmx swap render the mapping table,
// fix the mapping via real <select> elements, click through to a real
// commit, and see the real result — end to end, no protocol shortcuts.
func TestCSVImportWizard_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	csvPath := filepath.Join(t.TempDir(), "items.csv")
	csvContent := "SKU,Item Name,Type\nSTEEL-BAR-10,10mm Steel Rebar,stock\nCEMENT-50KG,Portland Cement 50kg Bag,stock\n"
	if err := os.WriteFile(csvPath, []byte(csvContent), 0o644); err != nil {
		t.Fatalf("write temp csv: %v", err)
	}

	var mappingErrorText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/import/Item"),
		chromedp.WaitVisible(`#uc-import-form`, chromedp.ByQuery),
		chromedp.SetUploadFiles(`#uc-import-file`, []string{csvPath}, chromedp.ByQuery),
		clickAndSettle(`button[hx-post="/import/Item/preview"]`),
		chromedp.Text(`.uc-import-mapping-error`, &mappingErrorText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("upload + first preview: %v", err)
	}
	if !strings.Contains(mappingErrorText, `required field`) {
		t.Fatalf(`expected a mapping-incomplete error in the rendered page, got: %q`, mappingErrorText)
	}

	// Fix the mapping via the real <select> elements the mapping editor
	// rendered, then re-preview — the exact recovery path the fix added.
	// The click target is scoped to #uc-import-preview specifically: the
	// original page's own "Preview" button (still present, untouched,
	// outside the swapped region) also matches
	// button[hx-post="/import/Item/preview"] — clicking that one instead
	// would resubmit with no mapping.* fields at all, re-triggering
	// SuggestMapping's original incomplete guess instead of the
	// hand-fixed mapping this step is actually testing.
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`select[name="mapping.SKU"]`, "sku", chromedp.ByQuery),
		chromedp.SetValue(`select[name="mapping.Item Name"]`, "name", chromedp.ByQuery),
		chromedp.SetValue(`select[name="mapping.Type"]`, "item_type", chromedp.ByQuery),
		clickAndSettle(`#uc-import-preview button[hx-post="/import/Item/preview"]`),
	); err != nil {
		t.Fatalf("fix mapping + re-preview: %v", err)
	}

	var rowsHTML string
	if err := chromedp.Run(ctx, chromedp.InnerHTML(`.uc-import-rows`, &rowsHTML, chromedp.ByQuery)); err != nil {
		t.Fatalf("read preview rows: %v", err)
	}
	if !strings.Contains(rowsHTML, "10mm Steel Rebar") {
		t.Fatalf("expected the completed mapping to preview real row data, got:\n%s", rowsHTML)
	}

	var resultText string
	if err := chromedp.Run(ctx,
		clickAndSettle(`button[hx-post="/import/Item/commit"]`),
		chromedp.Text(`.uc-import-result`, &resultText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !strings.Contains(resultText, "2 succeeded") {
		t.Fatalf("expected the commit result to report 2 succeeded, got: %q", resultText)
	}
}
