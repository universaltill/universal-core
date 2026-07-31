package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestDelegation_RealBrowser drives the generated Delegation form
// (uc-infra#8, "substitute approver") through a real headless browser —
// the string/bool/date field trio this entity is made of has no
// dedicated e2e coverage anywhere else in this package (every existing
// form test either uses only string fields or exercises a reference
// picker), so this is this codebase's first real-browser proof that a
// FieldBool checkbox actually round-trips: a rendered-HTML-string test
// would confirm the `type="checkbox"` markup exists, never that
// checking it and saving actually persists `revoked=true` and that the
// checkbox reflects that as *checked* on reload, not just present.
//
// testServer() only publishes purchasing's forms by default — same
// reason TestDepartmentPosition_RealBrowser calls foundation.PublishForms
// explicitly.
func TestDelegation_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Delegation/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="delegator_user_id"]`, "alice", chromedp.ByQuery),
		chromedp.SetValue(`input[name="delegate_user_id"]`, "bob", chromedp.ByQuery),
		chromedp.SetValue(`input[name="ends_at"]`, "2026-12-31", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save new Delegation: %v", err)
	}
	id := savedRecordID(t, ctx, "Delegation")

	// Reload the edit form from scratch (a fresh navigation, not just
	// reading the DOM the create->edit swap left behind) — this is what
	// actually proves the values persisted server-side rather than just
	// surviving in the page's current, unreloaded state.
	var delegator, delegate, endsAt string
	var revokedChecked bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Delegation/"+id),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`input[name="delegator_user_id"]`, &delegator, chromedp.ByQuery),
		chromedp.Value(`input[name="delegate_user_id"]`, &delegate, chromedp.ByQuery),
		chromedp.Value(`input[name="ends_at"]`, &endsAt, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read back Delegation fields after reload: %v", err)
	}
	if delegator != "alice" || delegate != "bob" || endsAt != "2026-12-31" {
		t.Fatalf("expected delegator_user_id=alice delegate_user_id=bob ends_at=2026-12-31, got %q/%q/%q", delegator, delegate, endsAt)
	}
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.querySelector('input[name="revoked"][type="checkbox"]').checked`, &revokedChecked)); err != nil {
		t.Fatalf("read revoked checkbox state: %v", err)
	}
	if revokedChecked {
		t.Fatalf("expected revoked to be unchecked on a freshly-created Delegation, got checked")
	}

	// Check the revoked box and save — the real user gesture that
	// should immediately end a substitute-approver relationship.
	if err := chromedp.Run(ctx,
		chromedp.Click(`input[name="revoked"][type="checkbox"]`, chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("check revoked + save: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Delegation/"+id),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('input[name="revoked"][type="checkbox"]').checked`, &revokedChecked),
	); err != nil {
		t.Fatalf("read back revoked after save+reload: %v", err)
	}
	if !revokedChecked {
		t.Fatalf("expected revoked=true to persist and the checkbox to reflect it as checked after reload, got unchecked")
	}
}
