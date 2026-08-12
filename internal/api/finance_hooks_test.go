package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestAPI_Account_SavedViaRealHTTPSyncsGLAccounts is uc-infra#204's own
// integration-level regression test, mirroring
// TestAPI_CustomerInvoice_IssueViaRealHTTPPostsToLedger's own reasoning:
// every other finance.SyncGLAccount* test in this repo drives a
// crud.Engine directly; this is the one proving
// Handler.RegisterHook("Account", finance.SyncGLAccountOnWrite) — the
// real production composition-root wiring cmd/universal-core's main.go
// adds — actually works end to end through a real HTTP create AND
// update, which is exactly the path a real tenant admin's Account save
// takes and which never reached gl_accounts at all before this fix.
func TestAPI_Account_SavedViaRealHTTPSyncsGLAccounts(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}

	handler := testHandler(t, router)
	handler.RegisterHook("Account", finance.SyncGLAccountOnWrite)
	mux := http.NewServeMux()
	handler.Routes(mux)

	createBody, err := json.Marshal(map[string]any{
		"code": "1000", "name": "Assets", "type": "asset", "is_active": true,
	})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createReq := newRequest("POST", "/api/records/Account", tenantID, "farshid", createBody)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating Account, got %d: %s", createRec.Code, createRec.Body.String())
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

	glAccounts := data.NewGLAccountRepo(tenantDB)
	id, isActive, err := glAccounts.IDByCode(ctx, "1000")
	if err != nil {
		t.Fatalf("expected gl_accounts row for code 1000 right after the real HTTP create, got: %v", err)
	}
	if !isActive {
		t.Fatal("expected the synced gl_account to be active")
	}

	// The actual bug uc-infra#204 reported: an EDIT through the ordinary
	// save path (not just the initial create) must also reach
	// gl_accounts, with no separate sync step anywhere in this test.
	updateBody, err := json.Marshal(map[string]any{
		"code": "1000", "name": "Total Assets", "type": "asset", "is_active": false,
		"_version": created.Data.Version,
	})
	if err != nil {
		t.Fatalf("marshal update body: %v", err)
	}
	updateReq := newRequest("POST", "/api/records/Account/"+created.Data.ID, tenantID, "farshid", updateBody)
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200 updating Account via real HTTP, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	var name string
	var stillActive bool
	if err := tenantDB.QueryRowContext(ctx, `SELECT name, is_active FROM gl_accounts WHERE id = $1`, id).Scan(&name, &stillActive); err != nil {
		t.Fatalf("read gl_account after update: %v", err)
	}
	if name != "Total Assets" {
		t.Fatalf("expected the real HTTP update's rename to have reached gl_accounts, got name=%q", name)
	}
	if stillActive {
		t.Fatal("expected the real HTTP update's is_active=false to have reached gl_accounts")
	}
}
