package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/sales"
)

// TestAPI_CustomerInvoice_IssueViaRealHTTPPostsToLedger is the one
// integration test proving Handler.RegisterHook's wiring (the real
// production composition-root path, cmd/universal-core's main) actually
// works end to end through this package's own HTTP handlers — every
// other ledger-hook test in this repo registers the hook directly on a
// crud.Engine (kernel-level tests, cmd/seed-demo-data), none previously
// drove a real POST/second-POST-to-update through internal/api itself.
// Independent review flagged this exact gap.
func TestAPI_CustomerInvoice_IssueViaRealHTTPPostsToLedger(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := sales.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishForms: %v", err)
	}
	if err := sales.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	if _, err := glAccounts.UpsertByCode(ctx, "1200", "Accounts Receivable", "asset", "USD", true); err != nil {
		t.Fatalf("upsert AR gl_account: %v", err)
	}
	if _, err := glAccounts.UpsertByCode(ctx, "4100", "Sales Revenue", "income", "USD", true); err != nil {
		t.Fatalf("upsert revenue gl_account: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	customer, err := engine.Create(ctx, def("Party"), map[string]any{
		"party_type": "organization", "name": "Doha Retail Group", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	statusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "sales_order_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list sales_order_status StatusType: %v", err)
	}
	soStatuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var soDraftID string
	for _, s := range soStatuses {
		if code, _ := s.Data["code"].(string); code == "draft" {
			soDraftID = s.ID
		}
	}
	so, err := engine.Create(ctx, def("SalesOrder"), map[string]any{
		"so_number": "SO-1", "customer_id": customer.ID, "order_date": "2026-01-01", "status_id": soDraftID,
	}, actor)
	if err != nil {
		t.Fatalf("create SalesOrder: %v", err)
	}
	invStatusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "customer_invoice_status")
	if err != nil || len(invStatusTypes) == 0 {
		t.Fatalf("list customer_invoice_status StatusType: %v", err)
	}
	invStatuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", invStatusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	statusID := map[string]string{}
	for _, s := range invStatuses {
		if code, _ := s.Data["code"].(string); code != "" {
			statusID[code] = s.ID
		}
	}

	// The actual thing under test: Handler.RegisterHook, exactly as
	// cmd/universal-core's main wires it — not a direct engine.SetHook.
	handler := testHandler(t, router)
	handler.RegisterHook("CustomerInvoice", sales.PostCustomerInvoiceToLedger)
	mux := http.NewServeMux()
	handler.Routes(mux)

	createBody, err := json.Marshal(map[string]any{
		"invoice_number": "INV-1", "sales_order_id": so.ID, "customer_id": customer.ID,
		"invoice_date": "2026-01-15", "status_id": statusID["draft"], "total": 500.0,
	})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createReq := newRequest("POST", "/api/records/CustomerInvoice", tenantID, "farshid", createBody)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating CustomerInvoice, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	entries := data.NewJournalEntryRepo(tenantDB)
	if list, err := entries.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("expected no journal entry for a draft invoice, got %d (err=%v)", len(list), err)
	}

	updateBody, err := json.Marshal(map[string]any{
		"invoice_number": "INV-1", "sales_order_id": so.ID, "customer_id": customer.ID,
		"invoice_date": "2026-01-15", "status_id": statusID["issued"], "total": 500.0,
		"_version": created.Data.Version,
	})
	if err != nil {
		t.Fatalf("marshal update body: %v", err)
	}
	updateReq := newRequest("POST", "/api/records/CustomerInvoice/"+created.Data.ID, tenantID, "farshid", updateBody)
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200 issuing CustomerInvoice via real HTTP, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry posted by the real HTTP update, got %d", len(list))
	}
	entry := list[0]
	if entry.SourceType != "CustomerInvoice" || entry.SourceID != created.Data.ID {
		t.Fatalf("unexpected source: type=%s id=%s", entry.SourceType, entry.SourceID)
	}
	byCode := map[string]data.JournalLine{}
	for _, l := range entry.Lines {
		byCode[l.AccountCode] = l
	}
	wantMinor := int64(500 * 100)
	if ar := byCode["1200"]; ar.DebitMinor != wantMinor {
		t.Fatalf("expected AR (1200) debit %d, got %+v", wantMinor, ar)
	}
	if rev := byCode["4100"]; rev.CreditMinor != wantMinor {
		t.Fatalf("expected Revenue (4100) credit %d, got %+v", wantMinor, rev)
	}
}
