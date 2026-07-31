// The self-service tenant member management page (universal-core#3,
// ADR-0010) — Farshid's ask: a tenant admin adds/removes their own
// tenant's members and assigns roles themselves, instead of every
// access change being a Terraform/management-API act by Farshid or an
// AI session, per person, per tenant, forever.
//
// The page is the runtime face of two existing mechanisms plus one new
// one: Zitadel-side access (tenant_member user grants, managed through
// internal/zitadelmgmt — the new outbound surface), Core-side roles
// (Role/UserRole records, ADR-0005, written through the same guarded
// engine as everything else), and ADR-0006's tenant_admin role code as
// the gate. Three deliberate behaviors, decided on the issue and in
// ADR-0010, that shape the handlers below:
//
//   - Invite is detect-and-branch: an email with an existing account in
//     the tenant's org is granted access; one without gets an account
//     created first. Both paths are idempotent, so a re-submitted form
//     after a half-failed flow converges instead of erroring.
//   - Success never depends on an email arriving: the member appears in
//     the list immediately, and the page offers a copyable set-password
//     link plus a re-send action.
//   - Removal revokes this tenant's grant only (never deletes the
//     account), refuses self-removal, and refuses revoking your own
//     tenant_admin — so this page never leaves the tenant adminless.
//     (A tenant_admin editing their own UserRole row through the
//     generic /api/records CRUD can still self-demote — a deliberate,
//     self-inflicted-only caveat, noted in ADR-0010.)
//
// Every action that names a target user validates the target against
// the tenant's own member list before calling Zitadel (independent
// review, 2026-07-31): a form-supplied user_id/grant_id is
// attacker-controlled, and the service user's org memberships span
// every managed tenant org, so trusting the form would otherwise let
// one tenant's admin mint set-password links for (or revoke grants of)
// users outside their tenant.
package api

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/mail"
	"slices"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/zitadelmgmt"
)

// isTenantAdmin reports whether rc's actor holds the tenant_admin role
// code (authz.AdminRoleCode) in this tenant — the server-side gate for
// every members route and for the nav link. Resolved per call, not
// cached on the session: a just-revoked admin loses the page on their
// next request, not at their next login.
func isTenantAdmin(ctx context.Context, ts tenantScope, rc httpx.RequestContext) (bool, error) {
	codes, err := foundation.RoleCodesForUser(ctx, ts.db, rc.Actor.ID)
	if err != nil {
		return false, err
	}
	return slices.Contains(codes, authz.AdminRoleCode), nil
}

// requireMembersAccess runs the shared preamble for every members
// route: request context, tenant scope, and the tenant_admin gate —
// rendering the localized 403 page itself when the actor isn't an
// admin. The gate is enforced here server-side on every route, not just
// reflected in whether the nav shows the link (BA acceptance criterion
// 8: hiding a link is not access control).
func (h *Handler) requireMembersAccess(w http.ResponseWriter, r *http.Request) (httpx.RequestContext, tenantScope, string, bool) {
	rc, ok := requestContext(w, r)
	if !ok {
		return rc, tenantScope{}, "", false
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return rc, tenantScope{}, "", false
	}
	locale := localeFromRequest(w, r)
	admin, err := isTenantAdmin(r.Context(), ts, rc)
	if err != nil {
		writeInternalError(w, "resolve tenant_admin gate", err)
		return rc, tenantScope{}, "", false
	}
	if !admin {
		w.WriteHeader(http.StatusForbidden)
		nav := h.renderNav(r, &rc, locale)
		body := template.HTML("<div class=\"uc-members\"><h1>" +
			template.HTMLEscapeString(h.catalog.T(locale, "members.heading")) + "</h1><p class=\"uc-members-error\">" +
			template.HTMLEscapeString(h.catalog.T(locale, "members.forbidden")) + "</p></div>")
		if err := h.renderShell(w, locale, nav, body); err != nil {
			log.Printf("api: render members 403 shell: %v", err)
		}
		return rc, tenantScope{}, "", false
	}
	return rc, ts, locale, true
}

// membersBanner is one outcome message (success or error) rendered at
// the top of the page after an action, plus — for invite/link actions —
// the freshly-minted set-password link to copy.
type membersBanner struct {
	MessageKey string
	// MessageArg is appended after the catalog string (the member's
	// email, typically) — appended rather than interpolated, since the
	// catalog has no placeholder mechanism.
	MessageArg string
	IsError    bool
	// Link is a set-password URL to display for copying; LinkEmail says
	// whose it is.
	Link      string
	LinkEmail string
	// ErrDetail carries the underlying failure text on IsError — the
	// visible-error requirement (BA R5): never a silent failure, never a
	// bare "something went wrong" with the real cause only in logs.
	ErrDetail string
}

type memberRow struct {
	UserID      string
	Email       string
	DisplayName string
	StateLabel  string
	// Roles are the member's Core-side role grants (UserRole rows) —
	// RecordID is what the revoke form posts.
	Roles []memberRoleView
}

type memberRoleView struct {
	RecordID string
	Name     string
}

type roleOption struct {
	ID   string
	Name string
}

type membersView struct {
	Heading string
	Intro   string

	Unavailable      bool
	UnavailableLabel string

	// BannerMessage/BannerDetail are the resolved strings (template
	// stays logic-light).
	BannerMessage string
	BannerDetail  string
	BannerIsError bool
	BannerLink    string
	BannerLinkFor string
	LinkHint      string
	CopyLabel     string

	ColEmail  string
	ColName   string
	ColState  string
	ColRoles  string
	ColAction string

	Rows []memberRow

	RoleOptions     []roleOption
	RoleSelectLabel string
	AssignLabel     string
	RevokeLabel     string
	RemoveLabel     string
	LinkLabel       string
	EmailLabel      string

	InviteHeading     string
	InviteEmailLabel  string
	InviteGivenLabel  string
	InviteFamilyLabel string
	InviteButton      string
}

// membersPage renders GET /settings/members.
func (h *Handler) membersPage(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, ok := h.requireMembersAccess(w, r)
	if !ok {
		return
	}
	h.renderMembersPage(w, r, rc, ts, locale, nil)
}

// renderMembersPage builds and writes the full page — shared by the GET
// route and by every POST action's response (each action re-renders the
// page with a banner rather than redirecting, mirroring
// aiprovidersettings.go's submit-response shape).
func (h *Handler) renderMembersPage(w http.ResponseWriter, r *http.Request, rc httpx.RequestContext, ts tenantScope, locale string, banner *membersBanner) {
	view := membersView{
		Heading:           h.catalog.T(locale, "members.heading"),
		Intro:             h.catalog.T(locale, "members.intro"),
		UnavailableLabel:  h.catalog.T(locale, "members.unavailable"),
		LinkHint:          h.catalog.T(locale, "members.link_hint"),
		ColEmail:          h.catalog.T(locale, "members.col_email"),
		ColName:           h.catalog.T(locale, "members.col_name"),
		ColState:          h.catalog.T(locale, "members.col_state"),
		ColRoles:          h.catalog.T(locale, "members.col_roles"),
		ColAction:         h.catalog.T(locale, "members.col_actions"),
		RoleSelectLabel:   h.catalog.T(locale, "members.role_select"),
		AssignLabel:       h.catalog.T(locale, "members.assign_role"),
		RevokeLabel:       h.catalog.T(locale, "members.revoke_role"),
		RemoveLabel:       h.catalog.T(locale, "members.remove"),
		LinkLabel:         h.catalog.T(locale, "members.password_link"),
		EmailLabel:        h.catalog.T(locale, "members.password_email"),
		InviteHeading:     h.catalog.T(locale, "members.invite_heading"),
		InviteEmailLabel:  h.catalog.T(locale, "members.invite_email"),
		InviteGivenLabel:  h.catalog.T(locale, "members.invite_given_name"),
		InviteFamilyLabel: h.catalog.T(locale, "members.invite_family_name"),
		InviteButton:      h.catalog.T(locale, "members.invite_button"),
	}
	if banner != nil {
		view.BannerMessage = h.catalog.T(locale, banner.MessageKey)
		if banner.MessageArg != "" {
			view.BannerMessage += " " + banner.MessageArg
		}
		view.BannerIsError = banner.IsError
		view.BannerDetail = banner.ErrDetail
		view.BannerLink = banner.Link
		view.BannerLinkFor = banner.LinkEmail
	}

	orgID := h.membersOrgID(r.Context(), rc)
	if !h.members.Enabled() || orgID == "" {
		view.Unavailable = true
		h.writeMembersView(w, r, rc, locale, view)
		return
	}

	members, err := h.members.ListMembers(r.Context(), orgID)
	if err != nil {
		// The list is the page — a failure here renders as the page's
		// own visible error, not a bare 500 with the cause hidden in
		// logs (BA R5).
		view.Unavailable = true
		view.BannerIsError = true
		view.BannerMessage = h.catalog.T(locale, "members.error")
		view.BannerDetail = err.Error()
		log.Printf("api: members: list members (org %s): %v", orgID, err)
		h.writeMembersView(w, r, rc, locale, view)
		return
	}

	// One Role load for the whole page: it feeds both the assign-role
	// picker and every member row's role names — not re-derived per
	// member (independent review, 2026-07-31: an earlier draft did a
	// full Role scan per member, and worse, a transient role-lookup
	// failure silently rendered members as holding zero roles, which an
	// admin would "fix" by re-assigning roles they already had). A
	// failure here is a visible page error, not a silent empty state.
	nameByRoleID := map[string]string{}
	roleDef, err := ts.entityDef(r.Context(), "Role")
	var roles []data.Record
	if err == nil {
		roles, err = ts.crud.List(r.Context(), roleDef)
	}
	if err != nil {
		log.Printf("api: members: load roles: %v", err)
		view.BannerIsError = true
		view.BannerMessage = h.catalog.T(locale, "members.error")
		view.BannerDetail = err.Error()
	} else {
		for _, rec := range roles {
			name, _ := rec.Data["name"].(string)
			if name == "" {
				name, _ = rec.Data["code"].(string)
			}
			view.RoleOptions = append(view.RoleOptions, roleOption{ID: rec.ID, Name: name})
			nameByRoleID[rec.ID] = name
		}
	}

	for _, m := range members {
		row := memberRow{
			UserID:      m.UserID,
			Email:       m.Email,
			DisplayName: m.DisplayName,
			StateLabel:  membersStateLabel(m.GrantState),
			Roles:       h.memberRoleViews(r.Context(), ts, m.UserID, nameByRoleID),
		}
		view.Rows = append(view.Rows, row)
	}

	h.writeMembersView(w, r, rc, locale, view)
}

// memberRoleViews lists one member's UserRole rows (the record ids the
// revoke form posts) and resolves their display names from the page's
// already-loaded Role map. A row whose role_id isn't in the map is a
// dangling grant (Role deleted; crud soft-deletes with no cascade) —
// skipped, consistent with foundation.RoleGrantsForUser's posture that
// a dangling grant confers nothing.
func (h *Handler) memberRoleViews(ctx context.Context, ts tenantScope, userID string, nameByRoleID map[string]string) []memberRoleView {
	userRoleDef, err := ts.entityDef(ctx, "UserRole")
	if err != nil {
		log.Printf("api: members: load UserRole definition: %v", err)
		return nil
	}
	rows, err := ts.crud.ListByField(ctx, userRoleDef, "user_id", userID)
	if err != nil {
		log.Printf("api: members: list UserRole for %s: %v", userID, err)
		return nil
	}
	var views []memberRoleView
	for _, row := range rows {
		roleID, _ := row.Data["role_id"].(string)
		name, ok := nameByRoleID[roleID]
		if !ok {
			continue
		}
		views = append(views, memberRoleView{RecordID: row.ID, Name: name})
	}
	return views
}

// membersOrgID resolves the current tenant's linked Zitadel org — empty
// (⇒ unavailable state) when the tenant isn't linked or the lookup
// fails.
func (h *Handler) membersOrgID(ctx context.Context, rc httpx.RequestContext) string {
	if h.tenants == nil {
		return ""
	}
	orgID, err := h.tenants.GetZitadelOrgID(ctx, rc.TenantID)
	if err != nil {
		log.Printf("api: members: resolve tenant org: %v", err)
		return ""
	}
	return orgID
}

func (h *Handler) writeMembersView(w http.ResponseWriter, r *http.Request, rc httpx.RequestContext, locale string, view membersView) {
	var buf bytes.Buffer
	if err := membersTmpl.ExecuteTemplate(&buf, "page", view); err != nil {
		writeInternalError(w, "render members page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render members page shell", err)
	}
}

// membersStateLabel trims Zitadel's enum prefix
// ("USER_GRANT_STATE_ACTIVE" → "ACTIVE") — a read-only diagnostic
// column, deliberately not translated per state (the raw state is what
// an admin would quote in a support conversation).
func membersStateLabel(state string) string {
	return strings.TrimPrefix(state, "USER_GRANT_STATE_")
}

// membersActionScope is the shared preamble for the POST routes: access
// gate plus the org/client availability check (an action against an
// unavailable backend re-renders the unavailable page — with an error
// banner saying the action was dropped, not silently — rather than
// half-running).
func (h *Handler) membersActionScope(w http.ResponseWriter, r *http.Request) (httpx.RequestContext, tenantScope, string, string, bool) {
	rc, ts, locale, ok := h.requireMembersAccess(w, r)
	if !ok {
		return rc, ts, locale, "", false
	}
	orgID := h.membersOrgID(r.Context(), rc)
	if !h.members.Enabled() || orgID == "" {
		h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.unavailable", IsError: true})
		return rc, ts, locale, "", false
	}
	return rc, ts, locale, orgID, true
}

// memberForAction resolves a form-supplied user id against the tenant's
// actual member list — the authorization check every targeted action
// runs before touching Zitadel (independent review, 2026-07-31: the
// service user's org memberships span every managed tenant org, so an
// unvalidated form user_id would let one tenant's admin act on another
// tenant's users; the reset-code mint was a cross-tenant account
// takeover). Returns ok == false — with the page already rendered —
// when the id names no member of this tenant.
func (h *Handler) memberForAction(w http.ResponseWriter, r *http.Request, rc httpx.RequestContext, ts tenantScope, locale, orgID, userID string) (zitadelmgmt.Member, bool) {
	if userID == "" {
		h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.unknown_member", IsError: true})
		return zitadelmgmt.Member{}, false
	}
	members, err := h.members.ListMembers(r.Context(), orgID)
	if err != nil {
		h.membersActionError(w, r, rc, ts, locale, "list members for validation", err)
		return zitadelmgmt.Member{}, false
	}
	for _, m := range members {
		if m.UserID == userID {
			return m, true
		}
	}
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.unknown_member", IsError: true})
	return zitadelmgmt.Member{}, false
}

// auditMembersAction records a Zitadel-side mutation in this tenant's
// own audit log — the identity change lives in Zitadel, outside any
// local transaction, so this is written immediately after the remote
// call succeeds (best effort, loudly logged on failure) rather than
// atomically with it; the alternative was the page's most
// security-sensitive actions (minting a credential-equivalent link,
// granting/revoking access) leaving no record of who did what to whom
// (independent review, 2026-07-31). entityType is a synthetic
// audit-only name (TenantMemberGrant / TenantMemberPasswordReset), the
// same free-form convention foundation's own
// "form_definition:<type>" audit rows already use.
func (h *Handler) auditMembersAction(ctx context.Context, ts tenantScope, rc httpx.RequestContext, entityType, recordID string, action audit.Action, diff map[string]any) {
	entry, err := audit.New(entityType, recordID, action, rc.Actor, diff)
	if err != nil {
		log.Printf("api: members: build audit entry (%s %s %s): %v", action, entityType, recordID, err)
		return
	}
	if err := data.NewAuditRepo(ts.db).Insert(ctx, ts.db, entry); err != nil {
		log.Printf("api: members: write audit entry (%s %s %s): %v", action, entityType, recordID, err)
	}
}

// membersInvite handles POST /settings/members/invite — the
// detect-and-branch flow (ADR-0010): existing account → grant only; no
// account → create, grant, and immediately mint the set-password link
// so the admin leaves this request with working access to hand over,
// email or no email.
func (h *Handler) membersInvite(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, orgID, ok := h.membersActionScope(w, r)
	if !ok {
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if _, err := mail.ParseAddress(email); email == "" || err != nil {
		h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.invalid_email", IsError: true})
		return
	}

	userID, err := h.members.LookupUserByEmail(r.Context(), orgID, email)
	created := false
	switch {
	case errors.Is(err, zitadelmgmt.ErrUserNotFound):
		userID, err = h.members.CreateUser(r.Context(), orgID, email,
			strings.TrimSpace(r.FormValue("given_name")), strings.TrimSpace(r.FormValue("family_name")))
		if err != nil {
			h.membersActionError(w, r, rc, ts, locale, "create user", err)
			return
		}
		created = true
	case err != nil:
		h.membersActionError(w, r, rc, ts, locale, "lookup user", err)
		return
	}

	if err := h.members.GrantTenantMember(r.Context(), orgID, userID); err != nil {
		h.membersActionError(w, r, rc, ts, locale, "grant tenant_member", err)
		return
	}
	h.auditMembersAction(r.Context(), ts, rc, "TenantMemberGrant", userID, audit.ActionCreate,
		map[string]any{"email": email, "created_account": created})

	banner := &membersBanner{MessageKey: "members.invited_existing", MessageArg: email}
	if created {
		banner.MessageKey = "members.invited_new"
		link, err := h.members.PasswordSetLink(r.Context(), orgID, userID)
		h.auditMembersAction(r.Context(), ts, rc, "TenantMemberPasswordReset", userID, audit.ActionCreate,
			map[string]any{"mode": "link", "during": "invite", "succeeded": err == nil})
		if err != nil {
			// The invite itself succeeded — report that, with the link
			// mint's failure visible next to it, rather than failing the
			// whole action after the grant landed.
			log.Printf("api: members: mint set-password link for %s: %v", userID, err)
			banner.ErrDetail = err.Error()
		} else {
			banner.Link = link
			banner.LinkEmail = email
		}
	}
	h.renderMembersPage(w, r, rc, ts, locale, banner)
}

// membersPasswordLink handles POST /settings/members/password-link —
// mints (and displays for copying) a fresh set-password link. Each mint
// invalidates the previous code, which is also the honest "revoke a
// link I pasted somewhere wrong" affordance.
func (h *Handler) membersPasswordLink(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, orgID, ok := h.membersActionScope(w, r)
	if !ok {
		return
	}
	member, ok := h.memberForAction(w, r, rc, ts, locale, orgID, r.FormValue("user_id"))
	if !ok {
		return
	}
	link, err := h.members.PasswordSetLink(r.Context(), orgID, member.UserID)
	h.auditMembersAction(r.Context(), ts, rc, "TenantMemberPasswordReset", member.UserID, audit.ActionCreate,
		map[string]any{"mode": "link", "succeeded": err == nil})
	if err != nil {
		h.membersActionError(w, r, rc, ts, locale, "mint set-password link", err)
		return
	}
	// LinkEmail comes from the resolved member, never the form — the
	// display must name whoever the code was actually minted for.
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.link_ready", Link: link, LinkEmail: member.Email})
}

// membersPasswordEmail handles POST /settings/members/password-email —
// the re-send affordance (never the only path to access, ADR-0010).
func (h *Handler) membersPasswordEmail(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, orgID, ok := h.membersActionScope(w, r)
	if !ok {
		return
	}
	member, ok := h.memberForAction(w, r, rc, ts, locale, orgID, r.FormValue("user_id"))
	if !ok {
		return
	}
	err := h.members.SendPasswordResetEmail(r.Context(), orgID, member.UserID)
	h.auditMembersAction(r.Context(), ts, rc, "TenantMemberPasswordReset", member.UserID, audit.ActionCreate,
		map[string]any{"mode": "email", "succeeded": err == nil})
	if err != nil {
		h.membersActionError(w, r, rc, ts, locale, "send password reset email", err)
		return
	}
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.email_sent"})
}

// membersRemove handles POST /settings/members/remove — revokes the
// Zitadel grant and cleans up the member's Core-side UserRole rows.
// Refuses self-removal: the acting admin always survives, which is what
// guarantees the tenant is never left adminless by this page.
func (h *Handler) membersRemove(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, orgID, ok := h.membersActionScope(w, r)
	if !ok {
		return
	}
	userID := r.FormValue("user_id")
	if userID == rc.Actor.ID {
		h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.cannot_remove_self", IsError: true})
		return
	}
	// The grant id is NOT taken from the form — the member lookup below
	// yields the real tenant_member grant, so a forged grant_id can't
	// revoke some other grant in the org (e.g. the tenant_integration
	// connector credential — independent review, 2026-07-31).
	member, ok := h.memberForAction(w, r, rc, ts, locale, orgID, userID)
	if !ok {
		return
	}
	if err := h.members.RevokeTenantMember(r.Context(), orgID, member.UserID, member.GrantID); err != nil {
		h.membersActionError(w, r, rc, ts, locale, "revoke tenant_member", err)
		return
	}
	h.auditMembersAction(r.Context(), ts, rc, "TenantMemberGrant", member.UserID, audit.ActionDelete,
		map[string]any{"email": member.Email, "grant_id": member.GrantID})
	// Zitadel access is gone; now the Core-side role grants. A failure
	// here is reported but doesn't undo the revoke — orphaned UserRole
	// rows grant nothing to someone who can no longer resolve this
	// tenant, and the rows surface again (deletable) if the user is
	// re-invited.
	if userRoleDef, err := ts.entityDef(r.Context(), "UserRole"); err == nil {
		if rows, err := ts.crud.ListByField(r.Context(), userRoleDef, "user_id", userID); err == nil {
			for _, row := range rows {
				if err := ts.crud.Delete(r.Context(), userRoleDef, row.ID, rc.Actor); err != nil {
					log.Printf("api: members: delete UserRole %s: %v", row.ID, err)
				}
			}
		}
	}
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.removed"})
}

// membersAssignRole handles POST /settings/members/roles/assign — one
// new UserRole row, written through the guarded engine like any other
// record (normal audit trail, rc.Actor as the actor).
func (h *Handler) membersAssignRole(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, _, ok := h.membersActionScope(w, r)
	if !ok {
		return
	}
	userID := r.FormValue("user_id")
	roleID := r.FormValue("role_id")
	if userID == "" || roleID == "" {
		h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.error", IsError: true})
		return
	}
	userRoleDef, err := ts.entityDef(r.Context(), "UserRole")
	if err != nil {
		h.membersActionError(w, r, rc, ts, locale, "load UserRole definition", err)
		return
	}
	// Assigning a role someone already holds: skip the duplicate rather
	// than stacking identical global grants (a scoped duplicate with a
	// department is legitimate — but this page only writes global
	// grants, so an exact user+role match is enough to no-op on).
	if rows, err := ts.crud.ListByField(r.Context(), userRoleDef, "user_id", userID); err == nil {
		for _, row := range rows {
			existingRole, _ := row.Data["role_id"].(string)
			existingDept, _ := row.Data["department_id"].(string)
			if existingRole == roleID && existingDept == "" {
				h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.role_assigned"})
				return
			}
		}
	}
	if _, err := ts.crud.Create(r.Context(), userRoleDef, map[string]any{
		"user_id": userID,
		"role_id": roleID,
	}, rc.Actor); err != nil {
		h.membersActionError(w, r, rc, ts, locale, "assign role", err)
		return
	}
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.role_assigned"})
}

// membersRevokeRole handles POST /settings/members/roles/revoke —
// deletes one UserRole row. Refuses removing the actor's own
// tenant_admin grant: combined with membersRemove's self-guard, the
// acting admin can never lock themselves (or the tenant) out from this
// page.
func (h *Handler) membersRevokeRole(w http.ResponseWriter, r *http.Request) {
	rc, ts, locale, _, ok := h.membersActionScope(w, r)
	if !ok {
		return
	}
	recordID := r.FormValue("user_role_id")
	if recordID == "" {
		h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.error", IsError: true})
		return
	}
	userRoleDef, err := ts.entityDef(r.Context(), "UserRole")
	if err != nil {
		h.membersActionError(w, r, rc, ts, locale, "load UserRole definition", err)
		return
	}
	row, err := ts.crud.Get(r.Context(), userRoleDef, recordID)
	if err != nil {
		h.membersActionError(w, r, rc, ts, locale, "load UserRole record", err)
		return
	}
	rowUserID, _ := row.Data["user_id"].(string)
	roleID, _ := row.Data["role_id"].(string)
	if rowUserID == rc.Actor.ID {
		if roleDef, err := ts.entityDef(r.Context(), "Role"); err == nil {
			if roleRec, err := ts.crud.Get(r.Context(), roleDef, roleID); err == nil {
				if code, _ := roleRec.Data["code"].(string); code == authz.AdminRoleCode {
					h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.cannot_revoke_own_admin", IsError: true})
					return
				}
			}
		}
	}
	if err := ts.crud.Delete(r.Context(), userRoleDef, recordID, rc.Actor); err != nil {
		h.membersActionError(w, r, rc, ts, locale, "revoke role", err)
		return
	}
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{MessageKey: "members.role_revoked"})
}

// membersActionError renders an action failure as a visible page banner
// carrying the underlying detail (BA R5's no-silent-failure rule),
// alongside the server-side log line.
func (h *Handler) membersActionError(w http.ResponseWriter, r *http.Request, rc httpx.RequestContext, ts tenantScope, locale, logContext string, err error) {
	log.Printf("api: members: %s: %v", logContext, err)
	h.renderMembersPage(w, r, rc, ts, locale, &membersBanner{
		MessageKey: "members.error",
		IsError:    true,
		ErrDetail:  err.Error(),
	})
}

var membersTmpl = template.Must(template.New("members").Parse(`
{{define "page"}}
<div class="uc-members">
<h1>{{.Heading}}</h1>
<p>{{.Intro}}</p>
{{if .BannerMessage}}
<div class="uc-members-banner{{if .BannerIsError}} uc-members-banner-error{{end}}">
<p>{{.BannerMessage}}</p>
{{if .BannerDetail}}<p class="uc-members-banner-detail">{{.BannerDetail}}</p>{{end}}
{{if .BannerLink}}
<p class="uc-members-link-for">{{.LinkHint}} {{.BannerLinkFor}}</p>
<input class="uc-members-link" type="text" readonly value="{{.BannerLink}}" onclick="this.select()">
{{end}}
</div>
{{end}}
{{if .Unavailable}}
<p class="uc-members-unavailable">{{.UnavailableLabel}}</p>
{{else}}
<table class="uc-table uc-members-table">
<thead><tr><th>{{.ColEmail}}</th><th>{{.ColName}}</th><th>{{.ColState}}</th><th>{{.ColRoles}}</th><th>{{.ColAction}}</th></tr></thead>
<tbody>
{{$v := .}}
{{range .Rows}}
<tr data-user-id="{{.UserID}}">
<td>{{.Email}}</td>
<td>{{.DisplayName}}</td>
<td>{{.StateLabel}}</td>
<td class="uc-members-roles">
{{range .Roles}}
<span class="uc-members-role">{{.Name}}
<form method="post" action="/settings/members/roles/revoke" class="uc-members-inline-form">
<input type="hidden" name="user_role_id" value="{{.RecordID}}">
<button type="submit" title="{{$v.RevokeLabel}}">&times;</button>
</form>
</span>
{{end}}
{{if $v.RoleOptions}}
<form method="post" action="/settings/members/roles/assign" class="uc-members-inline-form">
<input type="hidden" name="user_id" value="{{.UserID}}">
<select name="role_id" aria-label="{{$v.RoleSelectLabel}}">
{{range $v.RoleOptions}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
</select>
<button type="submit">{{$v.AssignLabel}}</button>
</form>
{{end}}
</td>
<td class="uc-members-actions">
<form method="post" action="/settings/members/password-link" class="uc-members-inline-form">
<input type="hidden" name="user_id" value="{{.UserID}}">
<button type="submit">{{$v.LinkLabel}}</button>
</form>
<form method="post" action="/settings/members/password-email" class="uc-members-inline-form">
<input type="hidden" name="user_id" value="{{.UserID}}">
<button type="submit">{{$v.EmailLabel}}</button>
</form>
<form method="post" action="/settings/members/remove" class="uc-members-inline-form">
<input type="hidden" name="user_id" value="{{.UserID}}">
<button type="submit" class="uc-members-remove">{{$v.RemoveLabel}}</button>
</form>
</td>
</tr>
{{end}}
</tbody>
</table>

<h2>{{.InviteHeading}}</h2>
<form method="post" action="/settings/members/invite" class="uc-members-invite">
<label for="uc-members-invite-email">{{.InviteEmailLabel}}</label>
<input type="email" id="uc-members-invite-email" name="email" required>
<label for="uc-members-invite-given">{{.InviteGivenLabel}}</label>
<input type="text" id="uc-members-invite-given" name="given_name">
<label for="uc-members-invite-family">{{.InviteFamilyLabel}}</label>
<input type="text" id="uc-members-invite-family" name="family_name">
<button type="submit">{{.InviteButton}}</button>
</form>
{{end}}
</div>
{{end}}
`))
