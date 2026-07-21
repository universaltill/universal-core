package e2e

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
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
	db := testDB(t)
	withDevAuthEnabled(t)
	srv, tenantID := testServer(t, db)
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
	if _, err := wfRepo.CreateDraft(ctx, tenantID, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := wfRepo.Approve(ctx, tenantID, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := wfRepo.Publish(ctx, tenantID, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Create the record and drive its triggered job to waiting_approval
	// via the same mechanism internal/worker uses — setup, not what this
	// test is actually proving (the browser interaction below is).
	recordRepo := data.NewRecordRepo(db)
	rec, err := recordRepo.Create(ctx, tenantID, "Party", map[string]any{"party_type": "organization", "name": "Globex Corp"})
	if err != nil {
		t.Fatalf("create Party record: %v", err)
	}
	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.Enqueue(ctx, def, tenantID, "Party", rec.ID, actor); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}

	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE tenant_id = $1 AND workflow_name = $2`, tenantID, def.Name).Scan(&jobID); err != nil {
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
	got, err := jobRepo.Get(ctx, tenantID, jobID)
	if err != nil {
		t.Fatalf("Get after browser approve: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("expected the real click to have actually resumed the job (status queued, ready for the worker), got %q", got.Status)
	}
}
