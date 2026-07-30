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

// TestWorkflowInbox_ApproveButton_RealBrowser is the regression test —
// driven by a real browser, not curl — for the workflow inbox's Approve
// button (internal/api/workflow.go's renderWorkflowInbox/
// approveWorkflowJob). Every prior claim that "the approve endpoint
// works" in this codebase was verified via curl or an httptest.Recorder,
// neither of which executes JavaScript — this proves the real htmx
// interaction: a real click, a real hx-post, a real DOM row removal via
// hx-swap="outerHTML" against an empty response body, the same class of
// gap the earlier htmx.js-missing-script-tag bug (found by
// TestCSVImportWizard_RealBrowser) exists to catch.
func TestWorkflowInbox_ApproveButton_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, db := testServer(t)
	actor := humanActor()

	def := &workflow.Definition{
		Name: "party_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Party"},
		Steps:   []workflow.Step{{Kind: workflow.StepRequireApproval}, {Kind: workflow.StepNotify}},
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal workflow def: %v", err)
	}
	wfRepo := data.NewWorkflowDefinitionRepo(db)
	ctx := context.Background()
	if _, err := wfRepo.CreateDraft(ctx, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := wfRepo.Approve(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := wfRepo.Publish(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Create the record and drive its triggered job to waiting_approval
	// via the same mechanism internal/worker uses — setup, not what this
	// test is actually proving (the browser interaction below is).
	recordRepo := data.NewRecordRepo(db)
	rec, err := recordRepo.Create(ctx, "Party", map[string]any{"party_type": "organization", "name": "Globex Corp"})
	if err != nil {
		t.Fatalf("create Party record: %v", err)
	}
	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.Enqueue(ctx, def, "Party", rec.ID, actor); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}

	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	browserCtxVal := browserCtx(t, tenantID)
	rowSel := "#workflow-job-" + jobID

	// Not clickAndSettle here (used everywhere else in this package):
	// that helper listens for htmx:afterSettle on document.body, which
	// never bubbles for this specific row, because the button that
	// dispatches the request lives *inside* the exact element
	// (hx-target="closest tr") the swap removes — by the time htmx fires
	// afterSettle, the button and its former row are already detached
	// from the document, so the event has nowhere to bubble to. Found by
	// this test hanging to its full context timeout even against a
	// verified-correct server (confirmed separately via a raw click +
	// direct DB poll). WaitNotPresent sidesteps the issue entirely: it
	// doesn't depend on htmx's own event system at all, just polls the
	// DOM directly for the row's actual disappearance.
	if err := chromedp.Run(browserCtxVal,
		chromedp.Navigate(srv.URL+"/workflow-jobs"),
		chromedp.WaitVisible(rowSel, chromedp.ByQuery),
		chromedp.Click(rowSel+" button", chromedp.ByQuery),
		chromedp.WaitNotPresent(rowSel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate + approve via real browser: %v", err)
	}

	jobRepo := data.NewWorkflowJobRepo(db)
	got, err := jobRepo.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("Get after browser approve: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("expected the real click to have actually resumed the job (status queued, ready for the worker), got %q", got.Status)
	}
}

// TestWorkflowInbox_BlockedRowRendersNoButton is the real-browser half of
// the inbox role-filtering work. The api-level tests prove the markup;
// only a browser proves what a person actually ends up able to click.
//
// The bug being guarded against was specifically a UI dead end: the
// button was rendered, clicking it 403'd, and htmx does not swap on a
// non-2xx response — so the click did nothing at all, with no message.
// A test that only checks for a missing string would still pass if the
// button were merely hidden by CSS while remaining clickable.
func TestWorkflowInbox_BlockedRowRendersNoButton(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()

	// A role the browser's actor does NOT hold, gating the workflow.
	engine := crud.NewEngine(tenantDB)
	if _, err := engine.Create(ctx, foundation.Role(),
		map[string]any{"code": "finance_manager", "name": "Finance Manager"}, humanActor()); err != nil {
		t.Fatalf("create Role: %v", err)
	}

	def := &workflow.Definition{
		Name: "e2e_gated_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{
			{Kind: workflow.StepRequireApproval, Params: map[string]any{"role": "finance_manager"}},
		},
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal workflow def: %v", err)
	}
	wfRepo := data.NewWorkflowDefinitionRepo(tenantDB)
	if _, err := wfRepo.CreateDraft(ctx, def.Name, def.Version, raw, humanActor()); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := wfRepo.Approve(ctx, def.Name, def.Version, humanActor()); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := wfRepo.Publish(ctx, def.Name, def.Version, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	q, err2 := workflow.NewQueue(tenantDB, nil)
	if err2 != nil {
		t.Fatalf("NewQueue: %v", err2)
	}
	if _, err := q.Enqueue(ctx, def, "Item", "11111111-1111-1111-1111-111111111111", humanActor()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(tenantDB)); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/workflow-jobs"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the workflow inbox: %v", err)
	}

	// No clickable Approve control exists in the live DOM at all — not
	// merely hidden.
	var buttons int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('table.uc-table button').length`, &buttons,
	)); err != nil {
		t.Fatalf("count buttons: %v", err)
	}
	if buttons != 0 {
		t.Fatalf("a viewer without the required role still has %d clickable Approve button(s)", buttons)
	}

	// The row is still THERE — a pending approval is information the
	// viewer may need, so this greys the action out rather than hiding
	// the work — and it says why.
	var blockedText string
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`(document.querySelector('.uc-inbox-blocked')||{}).textContent || ''`, &blockedText,
	)); err != nil {
		t.Fatalf("read the blocked explanation: %v", err)
	}
	if !strings.Contains(blockedText, "finance_manager") {
		t.Fatalf("blocked row does not name the required role, got %q", blockedText)
	}
}
