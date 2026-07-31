// Real-browser E2E for path-prefix tenancy (universal-core#25,
// ADR-0011): a /t/{slug}/ page renders fully through the re-dispatch
// (CSS applied, table populated — a broken prefix strip would 404 or
// serve unstyled), the page stamps its tenant onto real htmx requests
// (data-uc-tenant + the configRequest hook in layout.go), and a
// tampered stamp is refused with the localized 409 surfaced through the
// existing error toast — the stale-tab guard as a browser actually
// experiences it. The webauth-session halves (picker, switcher,
// canonicalizing redirect) are covered by internal/webauth's flow tests
// against the fake OIDC provider; DevAuth can't fake a sealed cookie,
// so this file doesn't pretend to test them.
package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

// awaitPromise makes chromedp.Evaluate resolve a Promise-returning
// expression instead of handing back the pending Promise object.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

func TestE2E_TenantURL_PrefixedPageRendersAndStampsTenant(t *testing.T) {
	withDevAuthEnabled(t)

	// testServer doesn't wire the tenant directory the prefix router
	// needs, so this test builds its own stack (same pattern
	// members_test.go's membersTestServer uses).
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	bctx := context.Background()
	actor := humanActor()

	tenantID, err := router.Create(bctx, "E2E Tenant URL", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err := router.Get(bctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)
	if err := foundation.Publish(bctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(bctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if _, err := crud.NewEngine(tenantDB).Create(bctx, purchasing.Item(), map[string]any{
		"sku": "REBAR-10", "name": "Prefixed Rebar", "item_type": "stock",
	}, actor); err != nil {
		t.Fatalf("seed Item: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	tenants := data.NewTenantRepo(control)
	handler := api.New(router, catalog, nil, nil, nil, nil, nil)
	handler.SetTenantRepo(tenants)
	mux := http.NewServeMux()
	handler.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	slug, err := tenants.SlugByID(bctx, tenantID)
	if err != nil {
		t.Fatalf("SlugByID: %v", err)
	}

	ctx := browserCtx(t, tenantID)

	// 1. The prefixed list page renders for real: table present, CSS
	// actually applied (the uc-table border collapse is a computed-style
	// fact, not a markup string).
	var rowText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/t/"+slug+"/records/Item"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`table.uc-table`, &rowText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("prefixed list page: %v", err)
	}
	if !strings.Contains(rowText, "Prefixed Rebar") {
		t.Fatalf("prefixed page missing seeded record; table text:\n%s", rowText)
	}

	// 2. The nav carries the tenant stamp the configRequest hook reads.
	var stamped string
	if err := chromedp.Run(ctx,
		chromedp.AttributeValue(`nav.uc-nav`, "data-uc-tenant", &stamped, nil, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read data-uc-tenant: %v", err)
	}
	if stamped != tenantID {
		t.Fatalf("data-uc-tenant = %q, want %q", stamped, tenantID)
	}

	// 3. A real htmx request from this page succeeds (the stamp matches
	// the session) — driven through htmx's own ajax API so the
	// configRequest hook actually runs.
	var okStatus int
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`new Promise((resolve) => {
			const h = (evt) => { document.body.removeEventListener("htmx:afterRequest", h); resolve(evt.detail.xhr.status); };
			document.body.addEventListener("htmx:afterRequest", h);
			htmx.ajax("GET", "/api/records/Item", {swap: "none"});
		})`, &okStatus, awaitPromise),
	); err != nil {
		t.Fatalf("stamped htmx request: %v", err)
	}
	if okStatus != 200 {
		t.Fatalf("stamped htmx request status = %d, want 200", okStatus)
	}

	// 4. Tamper the stamp (the stale-tab simulation: this page now
	// claims a different tenant than the session) — the server refuses
	// with 409 and the localized message lands in the error toast.
	var tamperedStatus int
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector("nav.uc-nav").setAttribute("data-uc-tenant", "some-other-tenant")`, nil),
		chromedp.Evaluate(`new Promise((resolve) => {
			const h = (evt) => { document.body.removeEventListener("htmx:afterRequest", h); resolve(evt.detail.xhr.status); };
			document.body.addEventListener("htmx:afterRequest", h);
			htmx.ajax("GET", "/api/records/Item", {swap: "none"});
		})`, &tamperedStatus, awaitPromise),
	); err != nil {
		t.Fatalf("tampered htmx request: %v", err)
	}
	if tamperedStatus != 409 {
		t.Fatalf("tampered htmx request status = %d, want 409", tamperedStatus)
	}
	var toastText string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`#uc-toast.uc-toast-visible`, chromedp.ByQuery),
		chromedp.Text(`#uc-toast`, &toastText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("mismatch toast: %v", err)
	}
	if !strings.Contains(toastText, "different tenant") {
		t.Fatalf("toast text %q missing the localized mismatch message", toastText)
	}

	// 5. Unknown slug → 404, indistinguishable from nonexistent.
	var pageBody string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/t/no-such-tenant/records/Item"),
		chromedp.Text(`body`, &pageBody, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("unknown slug: %v", err)
	}
	if !strings.Contains(pageBody, "404") && !strings.Contains(strings.ToLower(pageBody), "not found") {
		t.Fatalf("unknown slug should 404, body:\n%s", pageBody)
	}
}
