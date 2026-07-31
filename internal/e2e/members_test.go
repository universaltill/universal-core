// Real-browser E2E for the self-service member management page
// (universal-core#3, ADR-0010): the page rendering with real CSS/DOM,
// the invite flow driven through the actual form (a plain POST, not
// HTMX — asserting the re-rendered page carries the new member row and
// the copyable set-password link), and the tenant_admin gate as a
// browser actually experiences it. The Zitadel management API is a
// local in-memory fake speaking the same endpoints
// internal/zitadelmgmt calls — the live-instance path is verified once
// against real Zitadel at deploy time (see the issue's close-out), not
// here.
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
	"github.com/universaltill/universal-core/internal/zitadelmgmt"
)

const (
	membersAdminActor = "00000000-0000-0000-0000-0000000000e2"
	membersOrgID      = "org-e2e"
	membersProjectID  = "proj-e2e"
)

// fakeZitadel is an in-memory Zitadel management API covering exactly
// the endpoints internal/zitadelmgmt calls, with mutable state so the
// invite flow's effects are assertable.
type fakeZitadel struct {
	mu     sync.Mutex
	nextID int
	// users: userID -> email
	users map[string]string
	// grants: grantID -> userID
	grants map[string]string
}

func newFakeZitadel() *fakeZitadel {
	return &fakeZitadel{
		users:  map[string]string{},
		grants: map[string]string{},
		nextID: 1,
	}
}

func (f *fakeZitadel) addMember(userID, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[userID] = email
	f.grants["grant-"+userID] = userID
}

func (f *fakeZitadel) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	// v2 user search (lookup by email).
	mux.HandleFunc("POST /v2/users", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queries []map[string]map[string]any `json:"queries"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		email := ""
		for _, q := range req.Queries {
			if eq, ok := q["emailQuery"]; ok {
				email, _ = eq["emailAddress"].(string)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		type result struct {
			UserID string `json:"userId"`
		}
		var results []result
		for id, e := range f.users {
			if strings.EqualFold(e, email) {
				results = append(results, result{UserID: id})
			}
		}
		writeJSON(w, map[string]any{"result": results})
	})
	// v2 create human user.
	mux.HandleFunc("POST /v2/users/human", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email struct {
				Email string `json:"email"`
			} `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		id := "user-new-" + strings.Repeat("x", f.nextID)
		f.nextID++
		f.users[id] = req.Email.Email
		writeJSON(w, map[string]any{"userId": id})
	})
	// v2 password reset (returnCode / sendLink).
	mux.HandleFunc("POST /v2/users/{id}/password_reset", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"verificationCode": "e2e-code"})
	})
	// v1 user-grant search — the member list.
	mux.HandleFunc("POST /management/v1/users/grants/_search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		type grant struct {
			ID          string   `json:"id"`
			UserID      string   `json:"userId"`
			Email       string   `json:"email"`
			DisplayName string   `json:"displayName"`
			State       string   `json:"state"`
			RoleKeys    []string `json:"roleKeys"`
		}
		var results []grant
		for gid, uid := range f.grants {
			results = append(results, grant{
				ID: gid, UserID: uid, Email: f.users[uid],
				DisplayName: "E2E " + f.users[uid],
				State:       "USER_GRANT_STATE_ACTIVE",
				RoleKeys:    []string{"tenant_member"},
			})
		}
		writeJSON(w, map[string]any{"result": results})
	})
	// v1 granted-project lookup.
	mux.HandleFunc("POST /management/v1/granted_projects/_search", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"result": []map[string]any{
			{"grantId": "pg-1", "projectId": membersProjectID},
		}})
	})
	// v1 add user grant.
	mux.HandleFunc("POST /management/v1/users/{id}/grants", func(w http.ResponseWriter, r *http.Request) {
		uid := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, existing := range f.grants {
			if existing == uid {
				w.WriteHeader(http.StatusConflict)
				writeJSON(w, map[string]any{"code": 6, "message": "already exists"})
				return
			}
		}
		f.grants["grant-"+uid] = uid
		writeJSON(w, map[string]any{"userGrantId": "grant-" + uid})
	})
	// v1 remove user grant.
	mux.HandleFunc("DELETE /management/v1/users/{id}/grants/{gid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.grants, r.PathValue("gid"))
		writeJSON(w, map[string]any{})
	})
	return mux
}

// membersTestServer stands up the full stack: fresh control DB +
// tenant, foundation published, tenant_admin seeded for the browser's
// actor, the fake Zitadel wired through api.SetMemberMgmt, and the
// tenant linked to the fake org.
func membersTestServer(t *testing.T) (srv *httptest.Server, fake *fakeZitadel, tenantDB *sql.DB) {
	t.Helper()
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	ctx := context.Background()
	actor := humanActor()

	id, err := router.Create(ctx, "E2E Members Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	// The browser's actor is a tenant_admin — the raw-engine "system
	// setup" path, same as internal/api's seedRBAC.
	engine := crud.NewEngine(tenantDB)
	role, err := engine.Create(ctx, foundation.Role(), map[string]any{
		"code": authz.AdminRoleCode, "name": "Tenant Administrator",
	}, actor)
	if err != nil {
		t.Fatalf("create tenant_admin Role: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.UserRole(), map[string]any{
		"user_id": membersAdminActor, "role_id": role.ID,
	}, actor); err != nil {
		t.Fatalf("grant tenant_admin: %v", err)
	}

	fake = newFakeZitadel()
	fake.addMember(membersAdminActor, "admin@example.invalid")
	fakeSrv := httptest.NewServer(fake.handler())
	t.Cleanup(fakeSrv.Close)

	tenants := data.NewTenantRepo(control)
	if err := tenants.SetZitadelOrgID(ctx, id, membersOrgID); err != nil {
		t.Fatalf("SetZitadelOrgID: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	handler := api.New(router, catalog, nil, nil, nil, nil, nil)
	handler.SetMemberMgmt(zitadelmgmt.NewClient(zitadelmgmt.Config{
		PAT: "e2e-pat", Issuer: fakeSrv.URL, ProjectID: membersProjectID,
	}), tenants)
	mux := http.NewServeMux()
	handler.Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Recorded for membersServerTenantID — browserCtx
	// (csv_import_test.go) hardcodes membersAdminActor's id as its
	// X-Actor-ID, so the admin browser context comes for free once a
	// test has the tenant id to pair with it.
	membersTenantMu.Lock()
	membersTenantByServer[srv.URL] = id
	membersTenantMu.Unlock()
	return srv, fake, tenantDB
}

// TestE2E_Members_PageRendersAndGate exercises the page as both an
// admin (full table) and a non-admin (403 with the localized message).
func TestE2E_Members_PageRendersAndGate(t *testing.T) {
	withDevAuthEnabled(t)
	srv, _, _ := membersTestServer(t)
	tid := membersServerTenantID(t, srv)

	ctx := browserCtx(t, tid)
	var pageText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/settings/members"),
		chromedp.WaitVisible(`table.uc-members-table`, chromedp.ByQuery),
		chromedp.Text(`div.uc-members`, &pageText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("admin members page: %v", err)
	}
	if !strings.Contains(pageText, "admin@example.invalid") {
		t.Fatalf("member list missing the seeded member; page text:\n%s", pageText)
	}
	// The nav link is shown to an admin.
	var navText string
	if err := chromedp.Run(ctx, chromedp.Text(`nav.uc-nav`, &navText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read nav: %v", err)
	}
	if !strings.Contains(navText, "Members") {
		t.Fatalf("nav missing Members link for admin; nav text: %s", navText)
	}

	// Non-admin: distinct browser context with a different actor id.
	nonAdmin := browserCtxWithActor(t, tid, "00000000-0000-0000-0000-0000000000aa")
	var forbiddenText string
	if err := chromedp.Run(nonAdmin,
		chromedp.Navigate(srv.URL+"/settings/members"),
		chromedp.WaitVisible(`p.uc-members-error`, chromedp.ByQuery),
		chromedp.Text(`p.uc-members-error`, &forbiddenText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("non-admin members page: %v", err)
	}
	if !strings.Contains(forbiddenText, "tenant administrator") {
		t.Fatalf("expected the localized forbidden message, got: %s", forbiddenText)
	}
	// And the nav hides the link for them.
	var nonAdminNav string
	if err := chromedp.Run(nonAdmin, chromedp.Text(`nav.uc-nav`, &nonAdminNav, chromedp.ByQuery)); err != nil {
		t.Fatalf("read non-admin nav: %v", err)
	}
	if strings.Contains(nonAdminNav, "Members") {
		t.Fatalf("nav shows Members link to a non-admin; nav text: %s", nonAdminNav)
	}
}

// TestE2E_Members_InviteFlow drives the real invite form: a brand-new
// email lands as a created account with a grant, the row appears in the
// re-rendered list immediately, and the copyable set-password link is
// on the page (ADR-0010's email-independence rule, as a user sees it).
func TestE2E_Members_InviteFlow(t *testing.T) {
	withDevAuthEnabled(t)
	srv, fake, _ := membersTestServer(t)
	tid := membersServerTenantID(t, srv)

	ctx := browserCtx(t, tid)
	var linkValue, pageText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/settings/members"),
		chromedp.WaitVisible(`form.uc-members-invite`, chromedp.ByQuery),
		chromedp.SetValue(`#uc-members-invite-email`, "newcomer@example.invalid", chromedp.ByQuery),
		chromedp.SetValue(`#uc-members-invite-given`, "New", chromedp.ByQuery),
		chromedp.SetValue(`#uc-members-invite-family`, "Comer", chromedp.ByQuery),
		chromedp.Click(`form.uc-members-invite button[type=submit]`, chromedp.ByQuery),
		chromedp.WaitVisible(`input.uc-members-link`, chromedp.ByQuery),
		chromedp.Value(`input.uc-members-link`, &linkValue, chromedp.ByQuery),
		chromedp.Text(`div.uc-members`, &pageText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("invite flow: %v", err)
	}
	if !strings.Contains(linkValue, "/ui/login/password/init?userID=") || !strings.Contains(linkValue, "code=e2e-code") {
		t.Fatalf("set-password link malformed: %s", linkValue)
	}
	if !strings.Contains(pageText, "newcomer@example.invalid") {
		t.Fatalf("re-rendered list missing the invited member; page text:\n%s", pageText)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	found := false
	for _, email := range fake.users {
		if email == "newcomer@example.invalid" {
			found = true
		}
	}
	if !found {
		t.Fatal("fake Zitadel has no created account for the invited email")
	}
	granted := false
	for _, uid := range fake.grants {
		if fake.users[uid] == "newcomer@example.invalid" {
			granted = true
		}
	}
	if !granted {
		t.Fatal("invited account holds no tenant_member grant in the fake")
	}
}

// membersServerTenantID recovers the tenant id the server was built
// with — the control DB registers exactly one tenant per test server,
// so the lookup is unambiguous.
func membersServerTenantID(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	// The tenant id travels via browserCtx's X-Tenant-ID header; tests
	// get it from the test server construction — stash it on the server
	// URL is not possible, so membersTestServer records it here instead.
	membersTenantMu.Lock()
	defer membersTenantMu.Unlock()
	id, ok := membersTenantByServer[srv.URL]
	if !ok {
		t.Fatal("membersTestServer did not record a tenant id for this server")
	}
	return id
}

var (
	membersTenantMu       sync.Mutex
	membersTenantByServer = map[string]string{}
)

// browserCtxWithActor is browserCtx with a caller-chosen actor id — the
// non-admin browser identity.
func browserCtxWithActor(t *testing.T, tenantID, actorID string) context.Context {
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
			"X-Actor-ID":  actorID,
		}).Do(ctx)
	})); err != nil {
		t.Fatalf("set extra headers: %v", err)
	}
	return ctx
}
