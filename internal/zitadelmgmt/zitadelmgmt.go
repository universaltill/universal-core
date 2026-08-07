// Package zitadelmgmt is universal-core's outbound Zitadel
// management-API client — the runtime half of what
// uc-infra's Terraform does at provisioning time (create a user, grant
// tenant_member, mint a set-password link), driving the self-service
// /settings/members page (ADR-0010) instead of a human running
// `terraform apply` per person, per tenant.
//
// Deliberately mirrors ut-cloud's internal/platform/zitadelmgmt shape
// (Config/nil-safe-when-unconfigured, plain net/http, no SDK) — the
// pattern is borrowed per the reuse-not-reimplement rule, not imported:
// different module, different credential, wider surface. Like that
// client, this one authenticates with a dedicated machine-user PAT
// scoped to exactly what this package calls — org-level user +
// user-grant management inside managed tenant orgs, nothing
// instance-level (see ADR-0010 for the scoping decision and its
// verified blast radius).
//
// API-version note: user operations (search/create/password reset) use
// Zitadel's v2 user API; user-grant operations use the v1 management
// API (v2 has no user-grant surface yet as of the deployed v4.16) —
// each call uses the best non-deprecated endpoint available for it.
package zitadelmgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// TenantMemberRole is the project role this client grants and revokes —
// a contract with uc-infra/infra/terraform/zitadel/main.tf's
// zitadel_project_role ("tenant_member") and internal/webauth's
// org-claim resolution, same as the Terraform file's own comment says.
const TenantMemberRole = "tenant_member"

// Config comes from the environment. An empty PAT disables member
// management entirely — callers must treat that as "feature
// unavailable" (the members page renders its unavailable state), never
// as an error: the exact contract aiassist/speechassist/secretcrypt
// already follow for optional integrations.
type Config struct {
	// PAT is a dedicated management machine user's Personal Access Token
	// (its identity is deployment configuration; ZITADEL_MGMT_PAT ← the
	// deployment's secret store).
	PAT string
	// Issuer is the Zitadel base URL — the same value webauth's
	// OIDC_ISSUER_URL carries (one identity instance for login and for
	// management).
	Issuer string
	// ProjectID is the Universal Core project whose tenant_member role
	// this client grants (ZITADEL_PROJECT_ID ← the deployment's secret
	// store — mirrored that way so this public repo hardcodes no
	// Zitadel ids, ADR-0010).
	ProjectID string
}

// Enabled reports whether the config is complete enough to construct a
// working client.
func (c Config) Enabled() bool {
	return c.PAT != "" && c.Issuer != "" && c.ProjectID != ""
}

// Member is one row of a tenant's member list: a user holding
// tenant_member on the Universal Core project in that tenant's org.
// Field values come from the user-grant search projection (which
// already carries the user's email/display name), so listing members
// costs one API call, not one-plus-one-per-member.
type Member struct {
	UserID      string
	GrantID     string
	Email       string
	DisplayName string
	// GrantState is Zitadel's user-grant state string (e.g.
	// "USER_GRANT_STATE_ACTIVE") — surfaced read-only on the page.
	GrantState string
}

// ErrUserNotFound is LookupUserByEmail's "no such account" result — a
// branch condition for the invite flow (create-then-grant), not a
// failure.
var ErrUserNotFound = errors.New("zitadelmgmt: no user with that email in this organization")

// Client calls Zitadel's management APIs for one instance. The zero
// value is unusable — construct via NewClient, which is also where
// "not configured" becomes a legitimate, always-safe nil state (same
// nil-receiver ergonomics as aiassist.Client and ut-cloud's own
// zitadelmgmt.Client).
type Client struct {
	pat       string
	base      string
	projectID string
	http      *http.Client

	// projectGrantIDs caches the per-org project-grant id lookup
	// (granted_projects search) — the id is stable for the life of the
	// grant, and every GrantTenantMember would otherwise pay an extra
	// round trip.
	mu              sync.Mutex
	projectGrantIDs map[string]string
}

// NewClient builds a Client for cfg, or nil if cfg is incomplete —
// callers use Enabled() and get the unavailable-state rendering for
// free.
func NewClient(cfg Config) *Client {
	if !cfg.Enabled() {
		return nil
	}
	return &Client{
		pat:             cfg.PAT,
		base:            strings.TrimRight(cfg.Issuer, "/"),
		projectID:       cfg.ProjectID,
		http:            &http.Client{Timeout: requestTimeout},
		projectGrantIDs: map[string]string{},
	}
}

// Enabled reports whether c is usable — false for a nil *Client, so
// call sites write `if c.Enabled()` without a separate nil check.
func (c *Client) Enabled() bool {
	return c != nil && c.pat != ""
}

const requestTimeout = 15 * time.Second

// apiError is Zitadel's JSON error envelope ({"code": …, "message": …}
// — gRPC status codes, not HTTP ones).
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// do runs one JSON request. orgID, when non-empty, is sent as
// x-zitadel-orgid — the header the v1 management API scopes org-level
// calls by (v2 calls carry org context in the body instead and pass
// orgID == "").
func (c *Client) do(ctx context.Context, method, path, orgID string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("zitadelmgmt: marshal %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return fmt.Errorf("zitadelmgmt: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if orgID != "" {
		req.Header.Set("x-zitadel-orgid", orgID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zitadelmgmt: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("zitadelmgmt: read %s %s response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var ae apiError
		_ = json.Unmarshal(raw, &ae)
		if ae.Message != "" {
			return &callError{status: resp.StatusCode, code: ae.Code, message: ae.Message, method: method, path: path}
		}
		return &callError{status: resp.StatusCode, message: strings.TrimSpace(string(raw)), method: method, path: path}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("zitadelmgmt: decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// callError keeps the gRPC-style code around so callers can branch on
// AlreadyExists (idempotent grant) without string-matching messages.
type callError struct {
	status  int
	code    int
	message string
	method  string
	path    string
}

func (e *callError) Error() string {
	return fmt.Sprintf("zitadelmgmt: %s %s: HTTP %d: %s", e.method, e.path, e.status, e.message)
}

// grpc AlreadyExists = 6 — the code Zitadel returns for a duplicate
// user grant.
const codeAlreadyExists = 6

// ListMembers lists every user holding tenant_member on the configured
// project in orgID, via the v1 user-grant search (whose projection
// already includes each user's email and display name).
func (c *Client) ListMembers(ctx context.Context, orgID string) ([]Member, error) {
	body := map[string]any{
		"queries": []map[string]any{
			{"projectIdQuery": map[string]any{"projectId": c.projectID}},
		},
	}
	var out struct {
		Result []struct {
			ID          string   `json:"id"`
			UserID      string   `json:"userId"`
			Email       string   `json:"email"`
			DisplayName string   `json:"displayName"`
			State       string   `json:"state"`
			RoleKeys    []string `json:"roleKeys"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/management/v1/users/grants/_search", orgID, body, &out); err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(out.Result))
	for _, g := range out.Result {
		// The search matches the project; the machine tenant_integration
		// grant (a connector's credential, INTEGRATIONS.md) rides the same
		// project — filter to human-facing tenant_member grants only, this
		// page manages members, not connector credentials.
		if !slices.Contains(g.RoleKeys, TenantMemberRole) {
			continue
		}
		members = append(members, Member{
			UserID:      g.UserID,
			GrantID:     g.ID,
			Email:       g.Email,
			DisplayName: g.DisplayName,
			GrantState:  g.State,
		})
	}
	return members, nil
}

// LookupUserByEmail finds the id of the user with this email inside
// orgID. Returns ErrUserNotFound when no account matches (the invite
// flow's create branch), and an error when more than one does —
// ambiguity needs a human, not a best-guess pick (ut-cloud precedent).
//
// Detection is deliberately org-scoped: the service user has no
// instance-wide read (ADR-0010), so an account living only outside the
// managed orgs is invisible here and the invite flow creates a fresh
// org-local account instead. Accepted trade-off, documented in the ADR.
func (c *Client) LookupUserByEmail(ctx context.Context, orgID, email string) (string, error) {
	body := map[string]any{
		"queries": []map[string]any{
			{"organizationIdQuery": map[string]any{"organizationId": orgID}},
			{"emailQuery": map[string]any{
				"emailAddress": email,
				"method":       "TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE",
			}},
		},
	}
	var out struct {
		Result []struct {
			UserID string `json:"userId"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/users", "", body, &out); err != nil {
		return "", err
	}
	switch len(out.Result) {
	case 0:
		return "", ErrUserNotFound
	case 1:
		return out.Result[0].UserID, nil
	default:
		return "", fmt.Errorf("zitadelmgmt: %d users match email in org %s — refusing to guess", len(out.Result), orgID)
	}
}

// CreateUser creates a human user inside orgID with no password set —
// access to the account comes from the set-password link
// (PasswordSetLink), never from an email having arrived (ADR-0010's
// email-is-not-the-success-condition rule). The username is the email,
// matching how every existing account on this instance is named.
func (c *Client) CreateUser(ctx context.Context, orgID, email, givenName, familyName string) (string, error) {
	// Zitadel requires both profile names non-empty; an invite form
	// offering optional names still has the email to fall back on —
	// better a fixable placeholder profile than a failed invite.
	if givenName == "" {
		givenName = email
	}
	if familyName == "" {
		familyName = email
	}
	body := map[string]any{
		"organization": map[string]any{"orgId": orgID},
		"username":     email,
		"profile": map[string]any{
			"givenName":  givenName,
			"familyName": familyName,
		},
		"email": map[string]any{"email": email},
	}
	var out struct {
		UserID string `json:"userId"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/users/human", "", body, &out); err != nil {
		return "", err
	}
	if out.UserID == "" {
		return "", fmt.Errorf("zitadelmgmt: create user: response carried no userId")
	}
	return out.UserID, nil
}

// projectGrantID resolves (and caches) the id of the project grant that
// authorizes orgID to hold roles on the configured project — the same
// two-step model the Terraform side documents on
// zitadel_project_grant.tenant: a user grant in a granted (not owning)
// org must reference the project grant, or Zitadel can't resolve the
// project from that org's context.
func (c *Client) projectGrantID(ctx context.Context, orgID string) (string, error) {
	c.mu.Lock()
	if id, ok := c.projectGrantIDs[orgID]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	body := map[string]any{
		"queries": []map[string]any{
			{"projectIdQuery": map[string]any{"projectId": c.projectID}},
		},
	}
	var out struct {
		Result []struct {
			GrantID   string `json:"grantId"`
			ProjectID string `json:"projectId"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/management/v1/granted_projects/_search", orgID, body, &out); err != nil {
		return "", err
	}
	for _, r := range out.Result {
		if r.ProjectID == c.projectID && r.GrantID != "" {
			c.mu.Lock()
			c.projectGrantIDs[orgID] = r.GrantID
			c.mu.Unlock()
			return r.GrantID, nil
		}
	}
	return "", fmt.Errorf("zitadelmgmt: org %s holds no grant of project %s — has uc-infra's zitadel_project_grant been applied for this tenant?", orgID, c.projectID)
}

// GrantTenantMember grants tenant_member on the configured project to
// userID, scoped to orgID's project grant. Idempotent: a grant that
// already exists is success, so a retried invite converges instead of
// erroring halfway.
func (c *Client) GrantTenantMember(ctx context.Context, orgID, userID string) error {
	grantID, err := c.projectGrantID(ctx, orgID)
	if err != nil {
		return err
	}
	body := map[string]any{
		"projectId":      c.projectID,
		"projectGrantId": grantID,
		"roleKeys":       []string{TenantMemberRole},
	}
	err = c.do(ctx, http.MethodPost, "/management/v1/users/"+url.PathEscape(userID)+"/grants", orgID, body, nil)
	var ce *callError
	if errors.As(err, &ce) && ce.code == codeAlreadyExists {
		return nil
	}
	return err
}

// RevokeTenantMember removes the user grant grantID from userID —
// revoking this tenant's access only. Deliberately never deletes or
// deactivates the account itself: it may serve other tenants, and
// account deletion stays a deliberate human/Terraform act (ADR-0010).
func (c *Client) RevokeTenantMember(ctx context.Context, orgID, userID, grantID string) error {
	return c.do(ctx, http.MethodDelete,
		"/management/v1/users/"+url.PathEscape(userID)+"/grants/"+url.PathEscape(grantID), orgID, nil, nil)
}

// PasswordSetLink mints a fresh password-reset code for userID and
// returns the classic-login URL that consumes it — the copyable link
// that makes access independent of any email arriving (ADR-0010). Each
// call invalidates the previous code (Zitadel keeps one active code per
// user), so mint-on-demand, don't store.
//
// The URL shape matches Zitadel's own default password-init
// notification template for the classic login UI this instance runs —
// verified against the live instance when this shipped; if a Zitadel
// upgrade ever changes the login UI's path, this constant is the one
// place to update.
func (c *Client) PasswordSetLink(ctx context.Context, orgID, userID string) (string, error) {
	body := map[string]any{"returnCode": map[string]any{}}
	var out struct {
		VerificationCode string `json:"verificationCode"`
	}
	// orgID rides as the x-zitadel-orgid header even though v2 resolves
	// the user by id — defence in depth (independent review, 2026-07-31):
	// the caller's own membership check is the primary guard against
	// minting a link for a user outside the tenant, and the org context
	// here means Zitadel gets a chance to refuse a mismatch too.
	if err := c.do(ctx, http.MethodPost, "/v2/users/"+url.PathEscape(userID)+"/password_reset", orgID, body, &out); err != nil {
		return "", err
	}
	if out.VerificationCode == "" {
		return "", fmt.Errorf("zitadelmgmt: password reset returned no verification code")
	}
	link := fmt.Sprintf("%s/ui/login/password/init?userID=%s&code=%s&orgID=%s",
		c.base, url.QueryEscape(userID), url.QueryEscape(out.VerificationCode), url.QueryEscape(orgID))
	return link, nil
}

// SendPasswordResetEmail asks Zitadel to (re-)send its own
// password-reset email to userID — the "re-send" affordance next to the
// copyable link, never the only path to access. orgID scopes the call
// the same way (and for the same defence-in-depth reason) as
// PasswordSetLink.
func (c *Client) SendPasswordResetEmail(ctx context.Context, orgID, userID string) error {
	body := map[string]any{
		"sendLink": map[string]any{"notificationType": "NOTIFICATION_TYPE_Email"},
	}
	return c.do(ctx, http.MethodPost, "/v2/users/"+url.PathEscape(userID)+"/password_reset", orgID, body, nil)
}
