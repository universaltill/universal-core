// Handler tests for the self-service member-management page
// (/settings/members, ADR-0010) — the tenant_admin gate, the
// detect-and-branch invite flow, removal's self-guard, role
// assign/revoke (incl. the cannot-revoke-own-admin guard), and the
// visible-error posture when Zitadel is down. The Zitadel side is an
// in-memory httptest fake implementing exactly the endpoints
// internal/zitadelmgmt calls, with mutable users/grants state so the
// tests assert effects, not just response codes.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/zitadelmgmt"
)

// ---------------------------------------------------------------------
// Fake Zitadel
// ---------------------------------------------------------------------

type fakeZitadelUser struct {
	ID          string
	Email       string
	OrgID       string
	DisplayName string
}

type fakeZitadelGrant struct {
	ID       string
	UserID   string
	RoleKeys []string
}

// fakeZitadel is an in-memory Zitadel management API implementing the
// exact endpoints internal/zitadelmgmt calls, keeping mutable state so
// tests can assert what an action actually did to the identity side.
type fakeZitadel struct {
	srv *httptest.Server

	mu     sync.Mutex
	users  map[string]*fakeZitadelUser
	grants map[string]*fakeZitadelGrant
	seq    int

	// totalCalls counts every HTTP request; createUserCalls and
	// grantAddCalls count the mutating endpoints specifically (a page
	// re-render legitimately performs a grants/_search read, so
	// "nothing happened" assertions are about mutations).
	totalCalls      int
	createUserCalls int
	grantAddCalls   int

	// failGrantSearch makes /management/v1/users/grants/_search return
	// HTTP 500 — the Zitadel-down scenario.
	failGrantSearch bool
}

func newFakeZitadel(t *testing.T) *fakeZitadel {
	t.Helper()
	f := &fakeZitadel{
		users:  map[string]*fakeZitadelUser{},
		grants: map[string]*fakeZitadelGrant{},
	}

	mux := http.NewServeMux()

	// POST /v2/users — search by org + email.
	mux.HandleFunc("POST /v2/users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Queries []struct {
				OrganizationIDQuery *struct {
					OrganizationID string `json:"organizationId"`
				} `json:"organizationIdQuery"`
				EmailQuery *struct {
					EmailAddress string `json:"emailAddress"`
				} `json:"emailQuery"`
			} `json:"queries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fakeZitadelError(w, http.StatusBadRequest, 3, "bad search body")
			return
		}
		var orgID, email string
		for _, q := range body.Queries {
			if q.OrganizationIDQuery != nil {
				orgID = q.OrganizationIDQuery.OrganizationID
			}
			if q.EmailQuery != nil {
				email = q.EmailQuery.EmailAddress
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		type hit struct {
			UserID string `json:"userId"`
		}
		result := []hit{}
		for _, u := range f.users {
			if orgID != "" && u.OrgID != orgID {
				continue
			}
			if email != "" && !strings.EqualFold(u.Email, email) {
				continue
			}
			result = append(result, hit{UserID: u.ID})
		}
		fakeZitadelJSON(w, map[string]any{"result": result})
	})

	// POST /v2/users/human — create a human user.
	mux.HandleFunc("POST /v2/users/human", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Organization struct {
				OrgID string `json:"orgId"`
			} `json:"organization"`
			Username string `json:"username"`
			Profile  struct {
				GivenName  string `json:"givenName"`
				FamilyName string `json:"familyName"`
			} `json:"profile"`
			Email struct {
				Email string `json:"email"`
			} `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fakeZitadelError(w, http.StatusBadRequest, 3, "bad create body")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.createUserCalls++
		f.seq++
		id := fmt.Sprintf("u-%d", f.seq)
		f.users[id] = &fakeZitadelUser{
			ID:          id,
			Email:       body.Email.Email,
			OrgID:       body.Organization.OrgID,
			DisplayName: body.Profile.GivenName + " " + body.Profile.FamilyName,
		}
		fakeZitadelJSON(w, map[string]any{"userId": id})
	})

	// POST /v2/users/{id}/password_reset — returnCode mints a
	// verification code; sendLink just succeeds.
	mux.HandleFunc("POST /v2/users/{id}/password_reset", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fakeZitadelError(w, http.StatusBadRequest, 3, "bad password_reset body")
			return
		}
		f.mu.Lock()
		_, known := f.users[r.PathValue("id")]
		f.mu.Unlock()
		if !known {
			fakeZitadelError(w, http.StatusNotFound, 5, "user not found")
			return
		}
		if _, ok := body["returnCode"]; ok {
			fakeZitadelJSON(w, map[string]any{"verificationCode": "code123"})
			return
		}
		fakeZitadelJSON(w, map[string]any{})
	})

	// POST /management/v1/users/grants/_search — list grants, joined
	// with user email/displayName the way Zitadel's projection does.
	mux.HandleFunc("POST /management/v1/users/grants/_search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failGrantSearch {
			fakeZitadelError(w, http.StatusInternalServerError, 13, "zitadel is on fire")
			return
		}
		orgID := r.Header.Get("x-zitadel-orgid")
		type row struct {
			ID          string   `json:"id"`
			UserID      string   `json:"userId"`
			Email       string   `json:"email"`
			DisplayName string   `json:"displayName"`
			State       string   `json:"state"`
			RoleKeys    []string `json:"roleKeys"`
		}
		result := []row{}
		for _, g := range f.grants {
			u := f.users[g.UserID]
			if u == nil || (orgID != "" && u.OrgID != orgID) {
				continue
			}
			result = append(result, row{
				ID:          g.ID,
				UserID:      g.UserID,
				Email:       u.Email,
				DisplayName: u.DisplayName,
				State:       "USER_GRANT_STATE_ACTIVE",
				RoleKeys:    g.RoleKeys,
			})
		}
		fakeZitadelJSON(w, map[string]any{"result": result})
	})

	// POST /management/v1/granted_projects/_search — the org's project
	// grant.
	mux.HandleFunc("POST /management/v1/granted_projects/_search", func(w http.ResponseWriter, r *http.Request) {
		fakeZitadelJSON(w, map[string]any{"result": []map[string]any{
			{"grantId": "pg-1", "projectId": "proj-1"},
		}})
	})

	// POST /management/v1/users/{id}/grants — add a user grant;
	// duplicate → HTTP 409 with gRPC AlreadyExists (6).
	mux.HandleFunc("POST /management/v1/users/{id}/grants", func(w http.ResponseWriter, r *http.Request) {
		userID := r.PathValue("id")
		var body struct {
			RoleKeys []string `json:"roleKeys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fakeZitadelError(w, http.StatusBadRequest, 3, "bad grant body")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.grantAddCalls++
		for _, g := range f.grants {
			if g.UserID == userID {
				fakeZitadelError(w, http.StatusConflict, 6, "already exists")
				return
			}
		}
		f.seq++
		id := fmt.Sprintf("g-%d", f.seq)
		f.grants[id] = &fakeZitadelGrant{ID: id, UserID: userID, RoleKeys: body.RoleKeys}
		fakeZitadelJSON(w, map[string]any{"userGrantId": id})
	})

	// DELETE /management/v1/users/{id}/grants/{gid} — revoke.
	mux.HandleFunc("DELETE /management/v1/users/{id}/grants/{gid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		gid := r.PathValue("gid")
		if _, ok := f.grants[gid]; !ok {
			fakeZitadelError(w, http.StatusNotFound, 5, "grant not found")
			return
		}
		delete(f.grants, gid)
		fakeZitadelJSON(w, map[string]any{})
	})

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.totalCalls++
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func fakeZitadelJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fakeZitadelError(w http.ResponseWriter, status, grpcCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": grpcCode, "message": message})
}

// seedUser adds a user account in org-1 with no grant (the
// invite-existing-account starting state).
func (f *fakeZitadel) seedUser(userID, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[userID] = &fakeZitadelUser{ID: userID, Email: email, OrgID: "org-1", DisplayName: "Seeded " + userID}
}

// seedMember adds a user in org-1 plus a grant with roleKeys (defaults
// to tenant_member) and returns the grant id.
func (f *fakeZitadel) seedMember(userID, email string, roleKeys ...string) string {
	f.seedUser(userID, email)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(roleKeys) == 0 {
		roleKeys = []string{zitadelmgmt.TenantMemberRole}
	}
	f.seq++
	id := fmt.Sprintf("g-seed-%d", f.seq)
	f.grants[id] = &fakeZitadelGrant{ID: id, UserID: userID, RoleKeys: roleKeys}
	return id
}

func (f *fakeZitadel) userCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users)
}

func (f *fakeZitadel) userByEmail(email string) *fakeZitadelUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if strings.EqualFold(u.Email, email) {
			copied := *u
			return &copied
		}
	}
	return nil
}

func (f *fakeZitadel) hasGrantForUser(userID, roleKey string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.grants {
		if g.UserID == userID && slices.Contains(g.RoleKeys, roleKey) {
			return true
		}
	}
	return false
}

func (f *fakeZitadel) grantExists(grantID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.grants[grantID]
	return ok
}

func (f *fakeZitadel) mutationCalls() (createUser, grantAdd int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createUserCalls, f.grantAddCalls
}

func (f *fakeZitadel) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totalCalls
}

func (f *fakeZitadel) setFailGrantSearch(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failGrantSearch = fail
}

// ---------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------

type membersHarness struct {
	t        *testing.T
	mux      *http.ServeMux
	tenantID string
	db       *sql.DB
	engine   *crud.Engine
	fake     *fakeZitadel
	roleIDs  map[string]string
}

// newMembersHarness builds the full members-page setup: fresh control
// DB + router (built inline rather than via newTestRouter because
// data.NewTenantRepo needs the same control *sql.DB the router wraps),
// a provisioned tenant with the foundation published, tenant_admin
// granted to "user-admin" and clerk to "user-pleb" (plus an unassigned
// "viewer" role for the assign flow), and — when wireClient — the
// zitadelmgmt client pointed at an in-memory fake with the tenant
// linked to org-1.
func newMembersHarness(t *testing.T, wireClient bool) *membersHarness {
	t.Helper()
	ctx := context.Background()

	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	if err := foundation.Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	roleIDs := seedRBAC(t, tenantDB, map[string][]string{
		authz.AdminRoleCode: {"user-admin"},
		"clerk":             {"user-pleb"},
		"viewer":            {},
	}, nil)

	h := testHandler(t, router)
	harness := &membersHarness{
		t:        t,
		tenantID: tenantID,
		db:       tenantDB,
		engine:   crud.NewEngine(tenantDB),
		roleIDs:  roleIDs,
	}

	if wireClient {
		fake := newFakeZitadel(t)
		client := zitadelmgmt.NewClient(zitadelmgmt.Config{
			PAT:       "test-pat",
			Issuer:    fake.srv.URL,
			ProjectID: "proj-1",
		})
		if client == nil {
			t.Fatal("expected a usable zitadelmgmt client")
		}
		tenants := data.NewTenantRepo(control)
		h.SetMemberMgmt(client, tenants)
		if err := tenants.SetZitadelOrgID(ctx, tenantID, "org-1"); err != nil {
			t.Fatalf("SetZitadelOrgID: %v", err)
		}
		harness.fake = fake
	}

	mux := http.NewServeMux()
	h.Routes(mux)
	harness.mux = mux
	return harness
}

func (m *membersHarness) get(actorID string) *httptest.ResponseRecorder {
	m.t.Helper()
	req := newRequest("GET", "/settings/members", m.tenantID, actorID, nil)
	rec := httptest.NewRecorder()
	m.mux.ServeHTTP(rec, req)
	return rec
}

func (m *membersHarness) post(actorID, target string, form url.Values) *httptest.ResponseRecorder {
	m.t.Helper()
	req := newRequest("POST", target, m.tenantID, actorID, []byte(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	m.mux.ServeHTTP(rec, req)
	return rec
}

// userRoleRows lists userID's UserRole rows through the raw engine —
// the assertion side of what the page's assign/revoke/remove actions
// write through the guarded one.
func (m *membersHarness) userRoleRows(userID string) []data.Record {
	m.t.Helper()
	rows, err := m.engine.ListByField(context.Background(), foundation.UserRole(), "user_id", userID)
	if err != nil {
		m.t.Fatalf("list UserRole rows for %s: %v", userID, err)
	}
	return rows
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

// TestMembers_NonAdmin_Forbidden: the tenant_admin gate is enforced
// server-side on GET and on the POST actions (BA acceptance criterion
// 8: hiding a nav link is not access control), and a denied POST never
// reaches Zitadel at all.
func TestMembers_NonAdmin_Forbidden(t *testing.T) {
	m := newMembersHarness(t, true)

	rec := m.get("user-pleb")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tenant administrator") {
		t.Fatalf("expected the localized forbidden text, got:\n%s", rec.Body.String())
	}

	rec = m.post("user-pleb", "/settings/members/invite", url.Values{"email": {"someone@example.com"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin invite: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := m.fake.requestCount(); n != 0 {
		t.Fatalf("expected no Zitadel calls for a denied POST, got %d", n)
	}
}

// TestMembers_Admin_NoClientWired_ShowsUnavailable: with member
// management not wired at all (no SetMemberMgmt), an admin still gets a
// working 200 page in its unavailable state — never a panic or 500.
func TestMembers_Admin_NoClientWired_ShowsUnavailable(t *testing.T) {
	m := newMembersHarness(t, false)

	rec := m.get("user-admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Member management is not available") {
		t.Fatalf("expected the unavailable text, got:\n%s", rec.Body.String())
	}
}

// TestMembers_Admin_ListsTenantMembers: the table lists tenant_member
// grant holders' emails; a grant carrying only tenant_integration (a
// connector machine credential on the same project) is filtered out.
func TestMembers_Admin_ListsTenantMembers(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")
	m.fake.seedMember("user-pleb", "pleb@example.com")
	m.fake.seedMember("user-machine", "machine@example.com", "tenant_integration")

	rec := m.get("user-admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "admin@example.com") || !strings.Contains(body, "pleb@example.com") {
		t.Fatalf("expected both members' emails in the table, got:\n%s", body)
	}
	if strings.Contains(body, "machine@example.com") {
		t.Fatalf("expected the tenant_integration-only grant to be filtered out, got:\n%s", body)
	}
}

// TestMembers_Invite_NewAccount: the create branch — no account with
// that email exists, so one is created in the tenant's org, granted
// tenant_member, and the response carries the copyable set-password
// link plus the new member's row (the page re-lists).
func TestMembers_Invite_NewAccount(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")

	rec := m.post("user-admin", "/settings/members/invite", url.Values{
		"email":       {"new.member@example.com"},
		"given_name":  {"New"},
		"family_name": {"Member"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	created := m.fake.userByEmail("new.member@example.com")
	if created == nil {
		t.Fatal("expected the fake to now hold a created user for new.member@example.com")
	}
	if created.OrgID != "org-1" {
		t.Fatalf("expected the user created in org-1, got %q", created.OrgID)
	}
	if !m.fake.hasGrantForUser(created.ID, zitadelmgmt.TenantMemberRole) {
		t.Fatal("expected a tenant_member grant for the created user")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Account created") {
		t.Fatalf("expected the account-created message, got:\n%s", body)
	}
	if !strings.Contains(body, "/ui/login/password/init?userID=") {
		t.Fatalf("expected the set-password link in the response, got:\n%s", body)
	}
	if !strings.Contains(body, "<td>new.member@example.com</td>") {
		t.Fatalf("expected the new member's row in the re-listed table, got:\n%s", body)
	}
}

// TestMembers_Invite_ExistingAccount: the grant-only branch — the email
// already has an account in the org, so no user is created, just the
// grant, with the existing-account message.
func TestMembers_Invite_ExistingAccount(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")
	m.fake.seedUser("user-existing", "existing@example.com")
	usersBefore := m.fake.userCount()

	rec := m.post("user-admin", "/settings/members/invite", url.Values{"email": {"existing@example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := m.fake.userCount(); got != usersBefore {
		t.Fatalf("expected no new user account (had %d, now %d)", usersBefore, got)
	}
	if !m.fake.hasGrantForUser("user-existing", zitadelmgmt.TenantMemberRole) {
		t.Fatal("expected a tenant_member grant for the existing account")
	}
	if !strings.Contains(rec.Body.String(), "existing account") {
		t.Fatalf("expected the existing-account message, got:\n%s", rec.Body.String())
	}
}

// TestMembers_Invite_InvalidEmail: a non-address is rejected with the
// error banner before any Zitadel mutation happens (the page re-render
// still performs its read-only member listing).
func TestMembers_Invite_InvalidEmail(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")
	usersBefore := m.fake.userCount()

	rec := m.post("user-admin", "/settings/members/invite", url.Values{"email": {"not-an-email"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Enter a valid email address.") {
		t.Fatalf("expected the invalid-email banner, got:\n%s", rec.Body.String())
	}
	createUser, grantAdd := m.fake.mutationCalls()
	if createUser != 0 || grantAdd != 0 {
		t.Fatalf("expected no Zitadel mutations for an invalid email, got createUser=%d grantAdd=%d", createUser, grantAdd)
	}
	if got := m.fake.userCount(); got != usersBefore {
		t.Fatalf("expected no new user account (had %d, now %d)", usersBefore, got)
	}
}

// TestMembers_Remove: removing another member revokes their Zitadel
// grant AND deletes their Core-side UserRole rows; removing yourself is
// refused with the grant intact — the guarantee the acting admin always
// survives.
func TestMembers_Remove(t *testing.T) {
	m := newMembersHarness(t, true)
	adminGrantID := m.fake.seedMember("user-admin", "admin@example.com")
	plebGrantID := m.fake.seedMember("user-pleb", "pleb@example.com")

	// seedRBAC granted clerk to user-pleb — the row removal must clean up.
	if got := len(m.userRoleRows("user-pleb")); got != 1 {
		t.Fatalf("precondition: expected 1 UserRole row for user-pleb, got %d", got)
	}

	rec := m.post("user-admin", "/settings/members/remove", url.Values{
		"user_id":  {"user-pleb"},
		"grant_id": {plebGrantID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Member removed.") {
		t.Fatalf("expected the removed message, got:\n%s", rec.Body.String())
	}
	if m.fake.grantExists(plebGrantID) {
		t.Fatal("expected the removed member's Zitadel grant to be deleted")
	}
	if got := len(m.userRoleRows("user-pleb")); got != 0 {
		t.Fatalf("expected the removed member's UserRole rows to be gone, got %d", got)
	}

	// Self-removal is refused and the grant survives.
	rec = m.post("user-admin", "/settings/members/remove", url.Values{
		"user_id":  {"user-admin"},
		"grant_id": {adminGrantID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "You cannot remove your own access.") {
		t.Fatalf("expected the self-removal refusal, got:\n%s", rec.Body.String())
	}
	if !m.fake.grantExists(adminGrantID) {
		t.Fatal("expected the admin's own grant to survive the refused self-removal")
	}
}

// TestMembers_AssignRole: assigning writes exactly one UserRole row
// through the guarded engine; re-assigning the same role no-ops instead
// of stacking a duplicate global grant.
func TestMembers_AssignRole(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")
	m.fake.seedMember("user-pleb", "pleb@example.com")
	viewerID := m.roleIDs["viewer"]

	countViewerRows := func() int {
		n := 0
		for _, row := range m.userRoleRows("user-pleb") {
			if roleID, _ := row.Data["role_id"].(string); roleID == viewerID {
				n++
			}
		}
		return n
	}

	rec := m.post("user-admin", "/settings/members/roles/assign", url.Values{
		"user_id": {"user-pleb"},
		"role_id": {viewerID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Role assigned.") {
		t.Fatalf("expected the role-assigned message, got:\n%s", rec.Body.String())
	}
	if got := countViewerRows(); got != 1 {
		t.Fatalf("expected exactly 1 viewer UserRole row, got %d", got)
	}

	// Same role again — must not create a duplicate row.
	rec = m.post("user-admin", "/settings/members/roles/assign", url.Values{
		"user_id": {"user-pleb"},
		"role_id": {viewerID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := countViewerRows(); got != 1 {
		t.Fatalf("expected the duplicate assign to no-op (1 row), got %d", got)
	}
}

// TestMembers_RevokeRole: revoking another member's UserRole row
// deletes it; revoking your own tenant_admin row is refused and the row
// survives — the second half of the never-adminless guarantee.
func TestMembers_RevokeRole(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")
	m.fake.seedMember("user-pleb", "pleb@example.com")

	// user-pleb's clerk row (seeded by seedRBAC) is the revoke target.
	plebRows := m.userRoleRows("user-pleb")
	if len(plebRows) != 1 {
		t.Fatalf("precondition: expected 1 UserRole row for user-pleb, got %d", len(plebRows))
	}
	rec := m.post("user-admin", "/settings/members/roles/revoke", url.Values{
		"user_role_id": {plebRows[0].ID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Role revoked.") {
		t.Fatalf("expected the role-revoked message, got:\n%s", rec.Body.String())
	}
	if got := len(m.userRoleRows("user-pleb")); got != 0 {
		t.Fatalf("expected user-pleb's UserRole row deleted, got %d rows", got)
	}

	// The admin's own tenant_admin row must be refused.
	adminRows := m.userRoleRows("user-admin")
	if len(adminRows) != 1 {
		t.Fatalf("precondition: expected 1 UserRole row for user-admin, got %d", len(adminRows))
	}
	rec = m.post("user-admin", "/settings/members/roles/revoke", url.Values{
		"user_role_id": {adminRows[0].ID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "You cannot revoke your own administrator role.") {
		t.Fatalf("expected the own-admin refusal, got:\n%s", rec.Body.String())
	}
	if got := len(m.userRoleRows("user-admin")); got != 1 {
		t.Fatalf("expected the admin's tenant_admin row to survive, got %d rows", got)
	}
}

// TestMembers_ZitadelFailure_VisibleErrorNot500: the member listing
// failing (Zitadel 500) renders as the page's own visible error banner
// carrying the cause — BA R5's no-silent-failure rule — not a bare 500.
func TestMembers_ZitadelFailure_VisibleErrorNot500(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com")
	m.fake.setFailGrantSearch(true)

	rec := m.get("user-admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with a visible error banner, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "The action failed.") {
		t.Fatalf("expected the members.error banner text, got:\n%s", rec.Body.String())
	}
}

// TestMembers_PasswordLink: the mint route resolves the target against
// the tenant's member list — a member gets a copyable link (named by
// the member's email from the lookup, not the form), an empty or
// foreign user_id is refused without any Zitadel mutation, and a mint
// failure surfaces as a visible error. This route is where the
// independent review (2026-07-31) found the original blocker (an
// unvalidated form user_id = cross-tenant reset-code mint), so the
// foreign-id case is the regression test for that exact hole.
func TestMembers_PasswordLink(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com", "tenant_member")
	m.fake.seedMember("user-target", "target@example.com", "tenant_member")
	// A user who exists in the org but holds NO tenant_member grant —
	// exactly the "any human in your own org, member or not" case.
	m.fake.seedUser("user-outside", "outside@example.com")

	rec := m.post("user-admin", "/settings/members/password-link", url.Values{"user_id": {"user-target"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("mint for member: expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/login/password/init?userID=user-target") {
		t.Fatalf("expected a set-password link for user-target, body:\n%s", body)
	}
	if !strings.Contains(body, "target@example.com") {
		t.Fatalf("expected the link labeled with the member's email from the lookup, body:\n%s", body)
	}

	for name, uid := range map[string]string{"empty": "", "non-member": "user-outside", "unknown": "user-nowhere"} {
		rec := m.post("user-admin", "/settings/members/password-link", url.Values{"user_id": {uid}})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s user_id: expected 200 page re-render, got %d", name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not a member of this tenant") {
			t.Fatalf("%s user_id: expected the unknown-member refusal, body:\n%s", name, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "/ui/login/password/init?userID=") {
			t.Fatalf("%s user_id: a set-password link was minted for a non-member", name)
		}
	}
}

// TestMembers_PasswordEmail: the re-send route — same membership gate,
// success banner on a member, refusal on a non-member.
func TestMembers_PasswordEmail(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com", "tenant_member")
	m.fake.seedMember("user-target", "target@example.com", "tenant_member")
	m.fake.seedUser("user-outside", "outside@example.com")

	rec := m.post("user-admin", "/settings/members/password-email", url.Values{"user_id": {"user-target"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("send for member: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Password reset email sent.") {
		t.Fatalf("expected the email-sent banner, body:\n%s", rec.Body.String())
	}

	rec = m.post("user-admin", "/settings/members/password-email", url.Values{"user_id": {"user-outside"}})
	if !strings.Contains(rec.Body.String(), "not a member of this tenant") {
		t.Fatalf("non-member: expected the unknown-member refusal, body:\n%s", rec.Body.String())
	}
}

// TestMembers_Remove_ForgedGrantID: the server derives the grant to
// revoke from the member lookup, so a forged grant_id form value can't
// revoke some other grant in the org (the tenant_integration connector
// credential was the concrete risk — independent review, 2026-07-31).
func TestMembers_Remove_ForgedGrantID(t *testing.T) {
	m := newMembersHarness(t, true)
	m.fake.seedMember("user-admin", "admin@example.com", "tenant_member")
	targetGrant := m.fake.seedMember("user-target", "target@example.com", "tenant_member")
	// The org's connector credential — a grant the page must never touch.
	integrationGrant := m.fake.seedMember("user-machine", "machine@example.com", "tenant_integration")

	rec := m.post("user-admin", "/settings/members/remove",
		url.Values{"user_id": {"user-target"}, "grant_id": {integrationGrant}})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: expected 200, got %d", rec.Code)
	}
	if m.fake.grantExists(integrationGrant) == false {
		t.Fatal("forged grant_id revoked the tenant_integration grant — the form value must be ignored")
	}
	if m.fake.grantExists(targetGrant) {
		t.Fatal("the member's own grant should have been revoked via the server-side lookup")
	}
}
