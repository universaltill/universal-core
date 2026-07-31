package zitadelmgmt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testProjectID = "proj-314159"
	testOrgID     = "org-271828"
)

// newTestClient wires a Client at a fake Zitadel served by handler —
// same construct-against-httptest pattern svcauth's Guard tests use.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient(Config{PAT: "test-pat", Issuer: srv.URL, ProjectID: testProjectID})
	if c == nil {
		t.Fatal("NewClient returned nil for a complete config")
	}
	return c
}

// decodeBody reads and JSON-decodes a request body inside a fake
// handler, failing the test on any malformed request.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
	return body
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestNewClient_IncompleteConfigReturnsNil(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"zero value", Config{}},
		{"missing PAT", Config{Issuer: "https://id.example.com", ProjectID: testProjectID}},
		{"missing Issuer", Config{PAT: "pat", ProjectID: testProjectID}},
		{"missing ProjectID", Config{PAT: "pat", Issuer: "https://id.example.com"}},
	}
	for _, c := range cases {
		if got := NewClient(c.cfg); got != nil {
			t.Errorf("%s: NewClient() = %v, want nil", c.name, got)
		}
	}
}

func TestNewClient_TrimsTrailingSlashFromIssuer(t *testing.T) {
	c := NewClient(Config{PAT: "pat", Issuer: "https://id.example.com/", ProjectID: testProjectID})
	if c == nil {
		t.Fatal("NewClient returned nil for a complete config")
	}
	if c.base != "https://id.example.com" {
		t.Fatalf("base = %q, want trailing slash trimmed", c.base)
	}
}

func TestClient_Enabled_NilReceiverSafe(t *testing.T) {
	var c *Client // the state every call site holds when the feature is unconfigured
	if c.Enabled() {
		t.Fatal("nil *Client must report Enabled() == false, not panic")
	}
	if got := NewClient(Config{PAT: "pat", Issuer: "https://id.example.com", ProjectID: testProjectID}); !got.Enabled() {
		t.Fatal("a constructed client must report Enabled() == true")
	}
}

func TestConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"zero value", Config{}, false},
		{"missing PAT", Config{Issuer: "https://id.example.com", ProjectID: testProjectID}, false},
		{"missing Issuer", Config{PAT: "pat", ProjectID: testProjectID}, false},
		{"missing ProjectID", Config{PAT: "pat", Issuer: "https://id.example.com"}, false},
		{"fully configured", Config{PAT: "pat", Issuer: "https://id.example.com", ProjectID: testProjectID}, true},
	}
	for _, c := range cases {
		if got := c.cfg.Enabled(); got != c.want {
			t.Errorf("%s: Enabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestListMembers_ReturnsMembersAndFiltersNonMemberGrants(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/management/v1/users/grants/_search" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-zitadel-orgid"); got != testOrgID {
			t.Errorf("x-zitadel-orgid = %q, want %q", got, testOrgID)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-pat" {
			t.Errorf("Authorization = %q, want the PAT as a Bearer token", got)
		}
		body := decodeBody(t, r)
		queries, _ := body["queries"].([]any)
		if len(queries) != 1 {
			t.Fatalf("queries = %v, want exactly one projectIdQuery", body["queries"])
		}
		q, _ := queries[0].(map[string]any)
		pq, _ := q["projectIdQuery"].(map[string]any)
		if pq == nil || pq["projectId"] != testProjectID {
			t.Errorf("projectIdQuery = %v, want projectId %q", q, testProjectID)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"result": []map[string]any{
				{
					"id": "grant-1", "userId": "user-1", "email": "alice@example.com",
					"displayName": "Alice Example", "state": "USER_GRANT_STATE_ACTIVE",
					"roleKeys": []string{TenantMemberRole},
				},
				{
					// The connector machine user's grant on the same project —
					// must be filtered out, this page manages members only.
					"id": "grant-2", "userId": "machine-1", "email": "svc@example.com",
					"displayName": "Acme Connector", "state": "USER_GRANT_STATE_ACTIVE",
					"roleKeys": []string{"tenant_integration"},
				},
				{
					"id": "grant-3", "userId": "user-2", "email": "bob@example.com",
					"displayName": "Bob Example", "state": "USER_GRANT_STATE_ACTIVE",
					"roleKeys": []string{"some_other_role", TenantMemberRole},
				},
			},
		})
	}))

	members, err := c.ListMembers(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2 (tenant_integration grant filtered out): %+v", len(members), members)
	}
	want := Member{
		UserID: "user-1", GrantID: "grant-1", Email: "alice@example.com",
		DisplayName: "Alice Example", GrantState: "USER_GRANT_STATE_ACTIVE",
	}
	if members[0] != want {
		t.Errorf("members[0] = %+v, want %+v", members[0], want)
	}
	if members[1].UserID != "user-2" {
		t.Errorf("members[1].UserID = %q, want user-2 (grant carrying tenant_member among other roles)", members[1].UserID)
	}
}

func TestListMembers_APIErrorSurfacesZitadelMessage(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusForbidden, map[string]any{
			"code": 7, "message": "membership not found (AUTHZ-cdgFk)",
		})
	}))

	_, err := c.ListMembers(context.Background(), testOrgID)
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "membership not found (AUTHZ-cdgFk)") {
		t.Fatalf("error %q does not surface Zitadel's own message", err)
	}
}

func TestLookupUserByEmail(t *testing.T) {
	// One fake serves all three cardinalities; the response is switched
	// per test case, the request-shape assertions run every time.
	var results []map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/users" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-zitadel-orgid"); got != "" {
			t.Errorf("v2 search must carry org context in the body, not the header; got x-zitadel-orgid %q", got)
		}
		body := decodeBody(t, r)
		queries, _ := body["queries"].([]any)
		if len(queries) != 2 {
			t.Fatalf("queries = %v, want organizationIdQuery + emailQuery", body["queries"])
		}
		oq, _ := queries[0].(map[string]any)
		org, _ := oq["organizationIdQuery"].(map[string]any)
		if org == nil || org["organizationId"] != testOrgID {
			t.Errorf("organizationIdQuery = %v, want organizationId %q", oq, testOrgID)
		}
		eq, _ := queries[1].(map[string]any)
		em, _ := eq["emailQuery"].(map[string]any)
		if em == nil || em["emailAddress"] != "alice@example.com" {
			t.Errorf("emailQuery = %v, want emailAddress alice@example.com", eq)
		}
		if em != nil && em["method"] != "TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE" {
			t.Errorf("emailQuery method = %v, want TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE", em["method"])
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"result": results})
	}))

	t.Run("no match is ErrUserNotFound", func(t *testing.T) {
		results = nil
		_, err := c.LookupUserByEmail(context.Background(), testOrgID, "alice@example.com")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("err = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("single match returns its id", func(t *testing.T) {
		results = []map[string]any{{"userId": "user-1"}}
		id, err := c.LookupUserByEmail(context.Background(), testOrgID, "alice@example.com")
		if err != nil {
			t.Fatalf("LookupUserByEmail: %v", err)
		}
		if id != "user-1" {
			t.Fatalf("id = %q, want user-1", id)
		}
	})

	t.Run("multiple matches refuse to guess", func(t *testing.T) {
		results = []map[string]any{{"userId": "user-1"}, {"userId": "user-2"}}
		_, err := c.LookupUserByEmail(context.Background(), testOrgID, "alice@example.com")
		if err == nil {
			t.Fatal("expected an error for an ambiguous email")
		}
		if !strings.Contains(err.Error(), "refusing to guess") {
			t.Fatalf("error %q should say it refuses to guess", err)
		}
	})
}

func TestLookupUserByEmail_APIErrorSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{"code": 16, "message": "invalid token"})
	}))
	_, err := c.LookupUserByEmail(context.Background(), testOrgID, "alice@example.com")
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("err = %v, want the API error surfaced", err)
	}
}

func TestCreateUser_SendsFullProfile(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/users/human" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body := decodeBody(t, r)
		org, _ := body["organization"].(map[string]any)
		if org == nil || org["orgId"] != testOrgID {
			t.Errorf("organization = %v, want orgId %q", body["organization"], testOrgID)
		}
		if body["username"] != "alice@example.com" {
			t.Errorf("username = %v, want the email", body["username"])
		}
		profile, _ := body["profile"].(map[string]any)
		if profile == nil || profile["givenName"] != "Alice" || profile["familyName"] != "Example" {
			t.Errorf("profile = %v, want givenName Alice / familyName Example", body["profile"])
		}
		email, _ := body["email"].(map[string]any)
		if email == nil || email["email"] != "alice@example.com" {
			t.Errorf("email = %v, want email alice@example.com", body["email"])
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{"userId": "new-user-1"})
	}))

	id, err := c.CreateUser(context.Background(), testOrgID, "alice@example.com", "Alice", "Example")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id != "new-user-1" {
		t.Fatalf("id = %q, want new-user-1", id)
	}
}

func TestCreateUser_EmptyNamesFallBackToEmail(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		profile, _ := body["profile"].(map[string]any)
		if profile == nil || profile["givenName"] != "alice@example.com" || profile["familyName"] != "alice@example.com" {
			t.Errorf("profile = %v, want both names replaced by the email", body["profile"])
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{"userId": "new-user-2"})
	}))

	if _, err := c.CreateUser(context.Background(), testOrgID, "alice@example.com", "", ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
}

func TestCreateUser_ResponseWithoutUserIDIsError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"details": map[string]any{"sequence": "42"}})
	}))
	_, err := c.CreateUser(context.Background(), testOrgID, "alice@example.com", "Alice", "Example")
	if err == nil || !strings.Contains(err.Error(), "no userId") {
		t.Fatalf("err = %v, want a no-userId error", err)
	}
}

func TestCreateUser_APIErrorSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, map[string]any{"code": 6, "message": "User already exists (V2-zzz)"})
	}))
	_, err := c.CreateUser(context.Background(), testOrgID, "alice@example.com", "Alice", "Example")
	if err == nil || !strings.Contains(err.Error(), "User already exists") {
		t.Fatalf("err = %v, want the API error surfaced", err)
	}
}

// grantFake is the two-endpoint fake GrantTenantMember talks to:
// granted_projects/_search to resolve the project grant, then the user
// grants endpoint itself.
type grantFake struct {
	t                  *testing.T
	grantSearchCalls   atomic.Int64
	grantSearchResult  []map[string]any
	userGrantStatus    int
	userGrantResponse  map[string]any
	lastUserGrantBody  map[string]any
	lastUserGrantPath  string
	lastUserGrantOrgID string
}

func (f *grantFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/management/v1/granted_projects/_search":
			f.grantSearchCalls.Add(1)
			if got := r.Header.Get("x-zitadel-orgid"); got != testOrgID {
				f.t.Errorf("granted_projects search x-zitadel-orgid = %q, want %q", got, testOrgID)
			}
			body := decodeBody(f.t, r)
			queries, _ := body["queries"].([]any)
			if len(queries) != 1 {
				f.t.Fatalf("granted_projects queries = %v, want one projectIdQuery", body["queries"])
			}
			q, _ := queries[0].(map[string]any)
			pq, _ := q["projectIdQuery"].(map[string]any)
			if pq == nil || pq["projectId"] != testProjectID {
				f.t.Errorf("granted_projects projectIdQuery = %v, want projectId %q", q, testProjectID)
			}
			writeJSON(f.t, w, http.StatusOK, map[string]any{"result": f.grantSearchResult})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/grants"):
			f.lastUserGrantPath = r.URL.Path
			f.lastUserGrantOrgID = r.Header.Get("x-zitadel-orgid")
			f.lastUserGrantBody = decodeBody(f.t, r)
			status := f.userGrantStatus
			if status == 0 {
				status = http.StatusOK
			}
			resp := f.userGrantResponse
			if resp == nil {
				resp = map[string]any{}
			}
			writeJSON(f.t, w, status, resp)
		default:
			f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestGrantTenantMember_ResolvesGrantAndCachesIt(t *testing.T) {
	fake := &grantFake{
		t: t,
		grantSearchResult: []map[string]any{
			// A grant of some other project rides the same search response —
			// the matching one must be picked by projectId, not position.
			{"grantId": "pg-other", "projectId": "some-other-project"},
			{"grantId": "pg-1", "projectId": testProjectID},
		},
	}
	c := newTestClient(t, fake.handler())

	if err := c.GrantTenantMember(context.Background(), testOrgID, "user-1"); err != nil {
		t.Fatalf("GrantTenantMember: %v", err)
	}
	if fake.lastUserGrantPath != "/management/v1/users/user-1/grants" {
		t.Errorf("user grant path = %q, want /management/v1/users/user-1/grants", fake.lastUserGrantPath)
	}
	if fake.lastUserGrantOrgID != testOrgID {
		t.Errorf("user grant x-zitadel-orgid = %q, want %q", fake.lastUserGrantOrgID, testOrgID)
	}
	if fake.lastUserGrantBody["projectId"] != testProjectID {
		t.Errorf("projectId = %v, want %q", fake.lastUserGrantBody["projectId"], testProjectID)
	}
	if fake.lastUserGrantBody["projectGrantId"] != "pg-1" {
		t.Errorf("projectGrantId = %v, want pg-1 (matched by projectId, not first row)", fake.lastUserGrantBody["projectGrantId"])
	}
	roles, _ := fake.lastUserGrantBody["roleKeys"].([]any)
	if len(roles) != 1 || roles[0] != TenantMemberRole {
		t.Errorf("roleKeys = %v, want [%q]", fake.lastUserGrantBody["roleKeys"], TenantMemberRole)
	}

	// Second grant in the same org must reuse the cached project-grant id.
	if err := c.GrantTenantMember(context.Background(), testOrgID, "user-2"); err != nil {
		t.Fatalf("second GrantTenantMember: %v", err)
	}
	if got := fake.grantSearchCalls.Load(); got != 1 {
		t.Fatalf("granted_projects searched %d times across two grants, want 1 (cached)", got)
	}
	if fake.lastUserGrantPath != "/management/v1/users/user-2/grants" {
		t.Errorf("second user grant path = %q, want /management/v1/users/user-2/grants", fake.lastUserGrantPath)
	}
}

func TestGrantTenantMember_AlreadyExistsIsSuccess(t *testing.T) {
	fake := &grantFake{
		t:                 t,
		grantSearchResult: []map[string]any{{"grantId": "pg-1", "projectId": testProjectID}},
		userGrantStatus:   http.StatusConflict,
		userGrantResponse: map[string]any{"code": 6, "message": "User grant already exists (V3-6J8Gs)"},
	}
	c := newTestClient(t, fake.handler())

	if err := c.GrantTenantMember(context.Background(), testOrgID, "user-1"); err != nil {
		t.Fatalf("an AlreadyExists grant must be idempotent success, got: %v", err)
	}
}

func TestGrantTenantMember_OtherAPIErrorSurfaces(t *testing.T) {
	fake := &grantFake{
		t:                 t,
		grantSearchResult: []map[string]any{{"grantId": "pg-1", "projectId": testProjectID}},
		userGrantStatus:   http.StatusForbidden,
		userGrantResponse: map[string]any{"code": 7, "message": "permission denied (AUTHZ-xyz)"},
	}
	c := newTestClient(t, fake.handler())

	err := c.GrantTenantMember(context.Background(), testOrgID, "user-1")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the non-AlreadyExists API error surfaced", err)
	}
}

func TestGrantTenantMember_OrgWithoutProjectGrantIsError(t *testing.T) {
	fake := &grantFake{
		t: t,
		// The org holds a grant of a different project only — no match.
		grantSearchResult: []map[string]any{{"grantId": "pg-other", "projectId": "some-other-project"}},
	}
	c := newTestClient(t, fake.handler())

	err := c.GrantTenantMember(context.Background(), testOrgID, "user-1")
	if err == nil {
		t.Fatal("expected an error when the org holds no grant of the configured project")
	}
	if !strings.Contains(err.Error(), "zitadel_project_grant") {
		t.Fatalf("error %q should point at the missing zitadel_project_grant Terraform", err)
	}
	if fake.lastUserGrantPath != "" {
		t.Fatalf("user grant endpoint must not be called without a project grant; got %s", fake.lastUserGrantPath)
	}
}

func TestGrantTenantMember_GrantSearchErrorSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": 13, "message": "backend exploded"})
	}))
	err := c.GrantTenantMember(context.Background(), testOrgID, "user-1")
	if err == nil || !strings.Contains(err.Error(), "backend exploded") {
		t.Fatalf("err = %v, want the granted_projects search error surfaced", err)
	}
}

func TestRevokeTenantMember(t *testing.T) {
	var gotMethod, gotPath, gotOrg string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotOrg = r.Method, r.URL.Path, r.Header.Get("x-zitadel-orgid")
		writeJSON(t, w, http.StatusOK, map[string]any{})
	}))

	if err := c.RevokeTenantMember(context.Background(), testOrgID, "user-1", "grant-1"); err != nil {
		t.Fatalf("RevokeTenantMember: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/management/v1/users/user-1/grants/grant-1" {
		t.Errorf("path = %q, want /management/v1/users/user-1/grants/grant-1", gotPath)
	}
	if gotOrg != testOrgID {
		t.Errorf("x-zitadel-orgid = %q, want %q", gotOrg, testOrgID)
	}
}

func TestRevokeTenantMember_APIErrorSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{"code": 5, "message": "user grant not found (MGMT-abc)"})
	}))
	err := c.RevokeTenantMember(context.Background(), testOrgID, "user-1", "grant-1")
	if err == nil || !strings.Contains(err.Error(), "user grant not found") {
		t.Fatalf("err = %v, want the API error surfaced", err)
	}
}

func TestPasswordSetLink(t *testing.T) {
	const code = "C0DE+with/reserved chars"
	var base string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/users/user-1/password_reset" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body := decodeBody(t, r)
		if rc, ok := body["returnCode"].(map[string]any); !ok || len(rc) != 0 {
			t.Errorf(`body = %v, want {"returnCode":{}}`, body)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"verificationCode": code})
	}))
	base = c.base

	link, err := c.PasswordSetLink(context.Background(), testOrgID, "user-1")
	if err != nil {
		t.Fatalf("PasswordSetLink: %v", err)
	}
	want := base + "/ui/login/password/init?userID=user-1&code=" + url.QueryEscape(code) + "&orgID=" + testOrgID
	if link != want {
		t.Fatalf("link = %q, want %q", link, want)
	}
}

func TestPasswordSetLink_MissingVerificationCodeIsError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"details": map[string]any{"sequence": "9"}})
	}))
	_, err := c.PasswordSetLink(context.Background(), testOrgID, "user-1")
	if err == nil || !strings.Contains(err.Error(), "no verification code") {
		t.Fatalf("err = %v, want a no-verification-code error", err)
	}
}

func TestPasswordSetLink_APIErrorSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{"code": 5, "message": "user not found (V2-abc)"})
	}))
	_, err := c.PasswordSetLink(context.Background(), testOrgID, "user-1")
	if err == nil || !strings.Contains(err.Error(), "user not found") {
		t.Fatalf("err = %v, want the API error surfaced", err)
	}
}

func TestSendPasswordResetEmail(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/users/user-1/password_reset" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body := decodeBody(t, r)
		sl, _ := body["sendLink"].(map[string]any)
		if sl == nil || sl["notificationType"] != "NOTIFICATION_TYPE_Email" {
			t.Errorf("body = %v, want a sendLink with NOTIFICATION_TYPE_Email", body)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{})
	}))

	if err := c.SendPasswordResetEmail(context.Background(), testOrgID, "user-1"); err != nil {
		t.Fatalf("SendPasswordResetEmail: %v", err)
	}
}

func TestSendPasswordResetEmail_APIErrorSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{"code": 5, "message": "user not found (V2-def)"})
	}))
	err := c.SendPasswordResetEmail(context.Background(), testOrgID, "user-1")
	if err == nil || !strings.Contains(err.Error(), "user not found") {
		t.Fatalf("err = %v, want the API error surfaced", err)
	}
}

func TestDo_TransportErrorSurfaces(t *testing.T) {
	// A server closed before the call guarantees a transport-level
	// failure (same trick svcauth's introspection-down test uses).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := NewClient(Config{PAT: "test-pat", Issuer: srv.URL, ProjectID: testProjectID})

	_, err := c.ListMembers(context.Background(), testOrgID)
	if err == nil {
		t.Fatal("expected an error when Zitadel is unreachable")
	}
	if !strings.Contains(err.Error(), "zitadelmgmt:") {
		t.Fatalf("error %q should be wrapped with the package prefix", err)
	}
}

func TestDo_MalformedSuccessBodyIsDecodeError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "this is not json")
	}))
	_, err := c.ListMembers(context.Background(), testOrgID)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want a decode error for a malformed 2xx body", err)
	}
}

func TestDo_UnmarshalableBodyIsMarshalError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request must be sent when marshalling the body fails")
	}))
	err := c.do(context.Background(), http.MethodPost, "/whatever", "", func() {}, nil)
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("err = %v, want a marshal error", err)
	}
}

func TestDo_InvalidMethodIsBuildError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request must be sent when building the request fails")
	}))
	err := c.do(context.Background(), "bad method", "/whatever", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "build") {
		t.Fatalf("err = %v, want a build-request error", err)
	}
}

func TestDo_BodyReadFailureSurfaces(t *testing.T) {
	// Advertise more bytes than are sent: the client's body read ends in
	// an unexpected EOF, exercising the read-response error branch.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	_, err := c.ListMembers(context.Background(), testOrgID)
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("err = %v, want a read-response error", err)
	}
}

func TestDo_NonJSONErrorBodyStillSurfaces(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream connect error or disconnect/reset before headers")
	}))
	_, err := c.ListMembers(context.Background(), testOrgID)
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "upstream connect error") {
		t.Fatalf("error %q should carry the raw non-JSON body text", err)
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error %q should carry the HTTP status", err)
	}
}
