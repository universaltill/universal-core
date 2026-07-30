package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// TestWorkflowInbox_ForbiddenApproveShowsToast pins down something that was
// twice asserted to be broken without anyone checking — including by me, in
// the commit message and review record for the inbox role-filtering change
// earlier the same day.
//
// The claim was: htmx does not swap a non-2xx response into the DOM, so a
// 403 from Approve leaves the click looking like it did nothing. The first
// half is true. The second half stopped being true on 2026-07-26, when a
// global htmx:responseError listener was added to the page shell
// (layout.go) that reads the {"data":null,"error":"..."} envelope and shows
// it as a toast. Reasoning from the htmx behaviour alone skipped over that.
//
// This test exists so the correction cannot quietly rot back: it drives the
// exact race the inbox's role filtering narrows but cannot close — the
// button renders because the viewer holds the role, the grant is revoked,
// then they click — and asserts the real server-side message reaches the
// screen. If the shell listener is ever removed or the approve endpoint
// stops returning the JSON envelope, this goes red.
func TestWorkflowInbox_ForbiddenApproveShowsToast(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, db := testServer(t)
	ctx := context.Background()

	engine := crud.NewEngine(db)
	role, err := engine.Create(ctx, foundation.Role(), map[string]any{"code": "prober", "name": "Prober"}, humanActor())
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	grant, err := engine.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": e2eActorID, "role_id": role.ID}, humanActor())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	def := &workflow.Definition{
		Name: "probe_gated", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepRequireApproval, Params: map[string]any{"role": "prober"}}},
	}
	raw, _ := json.Marshal(def)
	wf := data.NewWorkflowDefinitionRepo(db)
	if _, err := wf.CreateDraft(ctx, def.Name, def.Version, raw, humanActor()); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := wf.Approve(ctx, def.Name, def.Version, humanActor()); err != nil {
		t.Fatalf("approve def: %v", err)
	}
	if err := wf.Publish(ctx, def.Name, def.Version, humanActor()); err != nil {
		t.Fatalf("publish: %v", err)
	}

	q, _ := workflow.NewQueue(db, nil)
	if _, err := q.Enqueue(ctx, def, "Item", "11111111-1111-1111-1111-111111111111", humanActor()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("processone: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/workflow-jobs"),
		chromedp.WaitVisible(`table.uc-table button`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Revoke AFTER render — the button is on screen, the grant is gone.
	if err := engine.Delete(ctx, foundation.UserRole(), grant.ID, humanActor()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if err := chromedp.Run(bctx, chromedp.Click(`table.uc-table button`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click: %v", err)
	}
	// WaitVisible on the toast rather than a fixed sleep: the toast is
	// created by the listener only when the failure actually arrives, so
	// waiting for it IS the assertion that the failure surfaced.
	if err := chromedp.Run(bctx, chromedp.WaitVisible(`#uc-toast.uc-toast-visible`, chromedp.ByQuery)); err != nil {
		t.Fatalf("no visible toast after a forbidden Approve — the failure is invisible to the user: %v", err)
	}
	var toast string
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-toast").textContent`, &toast,
	)); err != nil {
		t.Fatalf("read toast text: %v", err)
	}
	// The REAL server message, not a generic placeholder — that is what
	// proves the shell script parses the JSON error envelope rather than
	// merely reacting to the failure.
	if !strings.Contains(toast, "prober") {
		t.Fatalf("toast does not carry the server's own message (expected the required role name), got %q", toast)
	}
}
