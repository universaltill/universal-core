// Real-browser E2E for the tenant picker (/ui/auth/choose) and the nav
// switcher dropdown (universaltill/uc-infra#25, ADR-0011): deferred from
// #25's own independent review (uc-infra#25, finding 6) because
// internal/e2e's other tests drive DevAuth's spoofable headers, which
// cannot fake the sealed webauth session cookie these two surfaces
// actually require (webauth.Guard reads the cookie directly, never
// DevAuth's headers). Lifted via webauth.NewForE2ETests /
// SealSessionForE2ETests (internal/webauth/e2e_support.go) — a real
// Authenticator sealing a real session, so this drives the exact
// browser-visible behavior a completed OIDC login would leave behind,
// not a substitute. See universaltill/uc-infra#52.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
	"github.com/universaltill/universal-core/internal/webauth"
)

// twoTenantWebauthServer provisions two separate tenants (ADR-0003: two
// physical databases) under one control database, and a real
// *api.Handler wired with a genuine webauth.Authenticator — unlike
// testServer's auth: nil, the picker and nav switcher only ever render
// for h.auth.Enabled() (see internal/api/nav.go, choose.go), so a
// DevAuth-only server can never exercise either surface at all.
func twoTenantWebauthServer(t *testing.T) (srv *httptest.Server, tenant1, tenant2 string, auth *webauth.Authenticator) {
	t.Helper()
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close() })

	actor := humanActor()
	tenant1 = createPublishedTenant(t, router, actor, "Picker Tenant Alpha")
	tenant2 = createPublishedTenant(t, router, actor, "Picker Tenant Beta")

	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	cookieKeyB64 := base64.StdEncoding.EncodeToString(key[:])
	auth, err = webauth.NewForE2ETests(cookieKeyB64, data.NewTenantRepo(control))
	if err != nil {
		t.Fatalf("webauth.NewForE2ETests: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	// Production wires this too (cmd/universal-core/main.go's
	// webauth.New call site sits alongside a SetI18n call before
	// Routes() is ever built) — without it, choose.go's picker page
	// falls back to its own hardcoded English copy instead of the real
	// catalog, and TestNavTenantSwitcher_RTL_RealBrowser's dir="rtl"
	// coverage would be the nav's alone, not the picker's own
	// requestLocale/chooseRTLLocales path too.
	auth.SetI18n(catalog)

	mux := http.NewServeMux()
	api.New(router, catalog, auth, nil, nil, nil, nil).Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tenant1, tenant2, auth
}

// createPublishedTenant provisions one tenant (the real production path,
// router.Create) with foundation published — enough for the dashboard
// and nav to render without error; neither the picker nor the switcher
// need any entity module beyond that.
func createPublishedTenant(t *testing.T, router *tenantdb.Router, actor audit.Actor, name string) string {
	t.Helper()
	ctx := context.Background()
	id, err := router.Create(ctx, name, "eu-west")
	if err != nil {
		t.Fatalf("router.Create(%q): %v", name, err)
	}
	tenantDB, err := router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get(%q): %v", name, err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish(%q): %v", name, err)
	}
	return id
}

// webauthBrowserCtx is browserCtx's sibling for webauth-driven pages: no
// DevAuth headers (this server never needs them — webauth.Guard runs
// ahead of DevAuth and populates the request context itself, see
// internal/webauth/bridge.go), just a real headless Chrome instance.
func webauthBrowserCtx(t *testing.T) context.Context {
	t.Helper()
	execPath := findBrowser(t)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(execPath))...)
	t.Cleanup(cancelAlloc)

	ctx, cancel := chromedp.NewContext(allocCtx)
	t.Cleanup(cancel)

	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancelTimeout)
	return ctx
}

// setSession seals sess with auth and sets it as srv's real session
// cookie on the browser context before any navigation — the exact
// cookie a completed OIDC login would have left (writeSession), so
// webauth.Guard and handleChoose treat it identically to one.
func setSession(t *testing.T, ctx context.Context, auth *webauth.Authenticator, srv *httptest.Server, sess *webauth.Session) {
	t.Helper()
	sealed, err := auth.SealSessionForE2ETests(sess)
	if err != nil {
		t.Fatalf("seal session: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return network.SetCookie(webauth.SessionCookieName, sealed).
			WithURL(srv.URL).
			WithPath("/").
			WithHTTPOnly(true).
			Do(ctx)
	})); err != nil {
		t.Fatalf("set session cookie: %v", err)
	}
}

// waitForSwitchedTenant polls the post-switch page until the nav
// genuinely reflects wantTenantID active — not merely that some page
// loaded, but that the request context itself re-scoped (nav.go's own
// `data-uc-tenant` attribute, sourced from rc.TenantID, not just
// SessionTenants' display text) and, when wantURL is non-empty, that
// the browser actually navigated there.
//
// A single WaitVisible+Text read immediately after Submit is not
// reliable here: the switch is a real full-page navigation (a plain
// <form method=post>, not htmx), and .uc-nav-tenant exists on both the
// pre- and post-switch page, so an immediate read can win the race and
// observe the OLD document — confirmed empirically during this change's
// review (a probe sampling the same read 10x in a loop caught two stale
// reads before the page actually navigated). chromedp.Poll can't
// substitute either: its own docs say a poll task does not survive a
// navigation and errors out when one happens mid-poll. This instead
// re-issues a fresh, independent Evaluate/Location each iteration, the
// same way clickAndSettle (csv_import_test.go) hand-rolls a wait for
// htmx's own async settle instead of trusting a single WaitVisible.
func waitForSwitchedTenant(t *testing.T, ctx context.Context, wantTenantID, wantSummary, wantURL string) {
	t.Helper()
	const timeout = 10 * time.Second
	deadline := time.Now().Add(timeout)
	var gotTenant, gotSummary, gotURL string
	for {
		if err := chromedp.Run(ctx,
			chromedp.Evaluate(`(function(){
				var n = document.querySelector('nav.uc-nav');
				return n ? n.getAttribute('data-uc-tenant') : '';
			})()`, &gotTenant),
			chromedp.Evaluate(`(function(){
				var el = document.querySelector('.uc-nav-tenant summary');
				return el ? el.textContent : '';
			})()`, &gotSummary),
			chromedp.Location(&gotURL),
		); err != nil {
			t.Fatalf("poll post-switch nav state: %v", err)
		}
		if gotTenant == wantTenantID && gotSummary == wantSummary && (wantURL == "" || gotURL == wantURL) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for the switch to take effect: data-uc-tenant=%q (want %q) summary=%q (want %q) url=%q (want %q, empty=don't care)",
				timeout, gotTenant, wantTenantID, gotSummary, wantSummary, gotURL, wantURL)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTenantPicker_RendersBothTenantsAndPostsSwitch_RealBrowser drives
// the post-login picker (a session authenticated against two tenants,
// neither yet active) through a real submit of a real <form> — not just
// asserting the rendered markup contains both forms
// (internal/webauth/choose_test.go's TestHandleChoose_ListsAccessibleTenantsAsSwitchForms
// already covers that), but that clicking one actually completes the
// POST /ui/auth/switch round trip and lands the browser on returnTo
// with that tenant now active.
func TestTenantPicker_RendersBothTenantsAndPostsSwitch_RealBrowser(t *testing.T) {
	srv, tenant1, tenant2, auth := twoTenantWebauthServer(t)
	ctx := webauthBrowserCtx(t)

	setSession(t, ctx, auth, srv, &webauth.Session{
		Subject:   "picker-user",
		TenantIDs: []string{tenant1, tenant2},
		Expiry:    time.Now().Add(time.Hour),
	})

	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/ui/auth/choose"),
		chromedp.WaitVisible(`form[action="/ui/auth/switch"]`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the picker: %v", err)
	}
	if !strings.Contains(bodyText, "Picker Tenant Alpha") || !strings.Contains(bodyText, "Picker Tenant Beta") {
		t.Fatalf("expected the picker to render both tenant names, got body: %q", bodyText)
	}

	// Submit tenant2's own form specifically, scoped by its hidden
	// tenant_id value — a bare `button[type=submit]` selector would
	// ambiguously match either tenant's form.
	sel := `form:has(input[name="tenant_id"][value="` + tenant2 + `"])`
	if err := chromedp.Run(ctx, chromedp.Submit(sel, chromedp.ByQuery)); err != nil {
		t.Fatalf("submit the tenant2 switch form: %v", err)
	}
	waitForSwitchedTenant(t, ctx, tenant2, "Picker Tenant Beta", srv.URL+"/")
}

// TestNavTenantSwitcher_DropdownOpensAboveContentAndSwitches_RealBrowser
// proves the nav switcher (internal/api/nav.go, styled by
// internal/api/static/app.css's ".uc-nav-tenant" rules) is a real,
// working dropdown in a real browser: closed by default, opened by an
// actual click on the native <details> summary (not just present in the
// DOM), and genuinely stacked at its own footprint rather than merely
// carrying a z-index value on paper — confirmed via elementFromPoint,
// which returns whatever a real user would actually be able to click
// at the menu's own center point. In this layout that footprint sits
// over the nav bar itself (the switcher's single-item menu is short
// enough that it never extends past the nav's own bottom edge into the
// page body below it) — the same elementFromPoint check would just as
// well catch a broken z-index letting page content underneath it show
// through instead, whichever element that happened to be. Its own
// switch form completes a real navigation too.
func TestNavTenantSwitcher_DropdownOpensAboveContentAndSwitches_RealBrowser(t *testing.T) {
	srv, tenant1, tenant2, auth := twoTenantWebauthServer(t)
	ctx := webauthBrowserCtx(t)

	setSession(t, ctx, auth, srv, &webauth.Session{
		Subject:   "switcher-user",
		TenantID:  tenant1,
		TenantIDs: []string{tenant1, tenant2},
		Expiry:    time.Now().Add(time.Hour),
	})

	var openBefore bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`.uc-nav-tenant`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.uc-nav-tenant').hasAttribute('open')`, &openBefore),
	); err != nil {
		t.Fatalf("load the dashboard: %v", err)
	}
	if openBefore {
		t.Fatal("expected the switcher to start closed")
	}

	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-nav-tenant > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-nav-tenant-menu`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click to open the switcher: %v", err)
	}

	// Real, browser-computed geometry: the menu's own bounding box, the
	// container's (its offset parent, per app.css's position:relative on
	// .uc-nav-tenant), and computed position/z-index — not the CSS
	// source text, which formrender-style markup tests already cover.
	var geo struct {
		MenuTop, MenuRight, ContainerRight float64
		Position                           string
		ZIndex                             string
		TopElementInMenu                   bool
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
		var menu = document.querySelector('.uc-nav-tenant-menu');
		var container = document.querySelector('.uc-nav-tenant');
		var mr = menu.getBoundingClientRect();
		var cr = container.getBoundingClientRect();
		var cs = getComputedStyle(menu);
		var mid = document.elementFromPoint(mr.left + mr.width/2, mr.top + mr.height/2);
		return {
			MenuTop: mr.top, MenuRight: mr.right, ContainerRight: cr.right,
			Position: cs.position, ZIndex: cs.zIndex,
			TopElementInMenu: !!(mid && mid.closest('.uc-nav-tenant-menu'))
		};
	})()`, &geo)); err != nil {
		t.Fatalf("read switcher geometry: %v", err)
	}
	if geo.Position != "absolute" {
		t.Fatalf("expected the open menu to be position:absolute, got %q", geo.Position)
	}
	if geo.ZIndex != "30" {
		t.Fatalf("expected the open menu's computed z-index to be 30, got %q", geo.ZIndex)
	}
	if diff := geo.MenuRight - geo.ContainerRight; diff > 1 || diff < -1 {
		t.Fatalf("expected the menu's right edge to align with the switcher's (inset-inline-end:0 in LTR), got menu.right=%v container.right=%v", geo.MenuRight, geo.ContainerRight)
	}
	if !geo.TopElementInMenu {
		t.Fatal("expected the open menu to actually be the topmost element at its own center point, not merely carry a z-index on paper")
	}

	// Complete a real switch through the open menu's own form. Submit
	// (not Click on the nested button) targets the <form> element
	// itself, avoiding any flakiness from resolving/scrolling to a node
	// that only just became visible inside a native <details>.
	formSel := `.uc-nav-tenant-menu form:has(input[name="tenant_id"][value="` + tenant2 + `"])`
	if err := chromedp.Run(ctx, chromedp.Submit(formSel, chromedp.ByQuery)); err != nil {
		t.Fatalf("submit the switcher's tenant2 form: %v", err)
	}
	waitForSwitchedTenant(t, ctx, tenant2, "Picker Tenant Beta", "")
}

// TestNavTenantSwitcher_RTL_RealBrowser proves the switcher's positioning
// genuinely mirrors for a right-to-left locale, not just that dir="rtl"
// lands on <html>: app.css anchors the open menu with the CSS logical
// property inset-inline-end (not a physical `right`), which a real
// browser must resolve to the LEFT edge once the document's writing
// direction flips — a plain `right: 0` would have stayed on the right in
// both directions, and only a real layout engine evaluating dir="rtl"
// against a logical property proves the distinction; nothing about this
// is visible in the CSS or HTML source text alone.
func TestNavTenantSwitcher_RTL_RealBrowser(t *testing.T) {
	srv, tenant1, tenant2, auth := twoTenantWebauthServer(t)
	ctx := webauthBrowserCtx(t)

	setSession(t, ctx, auth, srv, &webauth.Session{
		Subject:   "rtl-user",
		TenantID:  tenant1,
		TenantIDs: []string{tenant1, tenant2},
		Expiry:    time.Now().Add(time.Hour),
	})

	var dir string
	if err := chromedp.Run(ctx,
		// ?lang=ar persists the locale cookie and renders this very
		// response in Arabic (internal/api/locale.go's languageFromRequest).
		chromedp.Navigate(srv.URL+"/?lang=ar"),
		chromedp.WaitVisible(`.uc-nav-tenant`, chromedp.ByQuery),
		chromedp.Evaluate(`document.documentElement.getAttribute('dir')`, &dir),
		chromedp.Click(`.uc-nav-tenant > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-nav-tenant-menu`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open the switcher under an RTL locale: %v", err)
	}
	if dir != "rtl" {
		t.Fatalf("expected <html dir=\"rtl\"> for locale=ar, got %q", dir)
	}

	var geo struct{ MenuLeft, ContainerLeft, MenuRight, ContainerRight float64 }
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
		var menu = document.querySelector('.uc-nav-tenant-menu');
		var container = document.querySelector('.uc-nav-tenant');
		var mr = menu.getBoundingClientRect();
		var cr = container.getBoundingClientRect();
		return {MenuLeft: mr.left, ContainerLeft: cr.left, MenuRight: mr.right, ContainerRight: cr.right};
	})()`, &geo)); err != nil {
		t.Fatalf("read RTL switcher geometry: %v", err)
	}
	if diff := geo.MenuLeft - geo.ContainerLeft; diff > 1 || diff < -1 {
		t.Fatalf("expected the RTL menu's LEFT edge to align with the switcher's (inset-inline-end:0 flips under dir=rtl), got menu.left=%v container.left=%v", geo.MenuLeft, geo.ContainerLeft)
	}
	if diff := geo.MenuRight - geo.ContainerRight; diff > -1 && diff < 1 {
		t.Fatalf("expected the RTL menu NOT to hug the container's right edge (that would mean the logical property never actually flipped), menu.right=%v container.right=%v", geo.MenuRight, geo.ContainerRight)
	}
}

// TestNavTenantSwitcher_SafariMarkerOptOutRuleShips_RealBrowser is the
// one assertion this repo's chromedp-driven, Chromium-only harness
// cannot fully prove: ::-webkit-details-marker is a Safari-specific
// mechanism (app.css's own comment: "Safari ignores list-style:none on
// summary; this is its own opt-out"), and there is no real Safari engine
// in this test toolchain to confirm the marker glyph is actually
// suppressed there (see CLAUDE.md/uc-infra#52's review — no Playwright,
// pure-Go chromedp only). What a real Chromium instance CAN prove,
// and what this test asserts: the rule is present and syntactically
// valid in app.css as this browser's own CSS parser actually loaded and
// parsed it (an invalid selector drops the whole rule from
// document.styleSheets, so this is a stronger check than grepping the
// source text), and Chromium's own marker-hiding mechanism
// (list-style:none) does take effect. Real Safari rendering itself
// remains unverified — noted, not silently assumed, in this change's
// review record.
func TestNavTenantSwitcher_SafariMarkerOptOutRuleShips_RealBrowser(t *testing.T) {
	srv, tenant1, tenant2, auth := twoTenantWebauthServer(t)
	ctx := webauthBrowserCtx(t)

	setSession(t, ctx, auth, srv, &webauth.Session{
		Subject:   "marker-user",
		TenantID:  tenant1,
		TenantIDs: []string{tenant1, tenant2},
		Expiry:    time.Now().Add(time.Hour),
	})

	var ruleShipped bool
	var listStyle string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`.uc-nav-tenant`, chromedp.ByQuery),
		chromedp.Evaluate(`Array.from(document.styleSheets).some(function(sheet){
			try {
				return Array.from(sheet.cssRules).some(function(r){
					return r.selectorText && r.selectorText.indexOf('details-marker') !== -1
						&& r.selectorText.indexOf('uc-nav-tenant') !== -1
						&& r.style.display === 'none';
				});
			} catch (e) { return false; }
		})`, &ruleShipped),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('.uc-nav-tenant > summary')).listStyleType`, &listStyle),
	); err != nil {
		t.Fatalf("inspect the marker opt-out rule: %v", err)
	}
	if !ruleShipped {
		t.Fatal("expected a parsed CSS rule hiding ::-webkit-details-marker for .uc-nav-tenant > summary")
	}
	if listStyle != "none" {
		t.Fatalf("expected the switcher summary's computed list-style-type to be none, got %q", listStyle)
	}
}
