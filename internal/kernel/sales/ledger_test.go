package sales

import (
	"context"
	"database/sql"
	"math"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

type customerInvoiceFixture struct {
	tenantDB   *sql.DB
	engine     *crud.Engine
	soID       string
	customerID string
	statusID   map[string]string // customer_invoice_status code -> Status id
}

func defFor(t *testing.T, tenantDB *sql.DB, entityType string) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), entityType)
	if err != nil {
		t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	return d
}

func setUpCustomerInvoiceFixture(t *testing.T) customerInvoiceFixture {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	if _, err := glAccounts.UpsertByCode(ctx, glAccountAR, "Accounts Receivable", "asset", "USD", true); err != nil {
		t.Fatalf("upsert AR gl_account: %v", err)
	}
	if _, err := glAccounts.UpsertByCode(ctx, glAccountRevenue, "Sales Revenue", "income", "USD", true); err != nil {
		t.Fatalf("upsert revenue gl_account: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	customer, err := engine.Create(ctx, defFor(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Doha Retail Group", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	// uc-infra#78: SalesOrder.customer_id now requires the referenced
	// Party to hold the customer PartyRole.
	if _, err := engine.Create(ctx, defFor(t, tenantDB, "PartyRole"), map[string]any{
		"party_id": customer.ID, "role_type": "customer",
	}, actor); err != nil {
		t.Fatalf("create customer PartyRole: %v", err)
	}

	statusTypes, err := engine.ListByField(ctx, defFor(t, tenantDB, "StatusType"), "code", "customer_invoice_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list customer_invoice_status StatusType: %v", err)
	}
	statuses, err := engine.ListByField(ctx, defFor(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	statusID := map[string]string{}
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code != "" {
			statusID[code] = s.ID
		}
	}
	if statusID["draft"] == "" || statusID["issued"] == "" || statusID["paid"] == "" {
		t.Fatalf("expected draft/issued/paid Status records, got %+v", statusID)
	}

	soStatusTypes, err := engine.ListByField(ctx, defFor(t, tenantDB, "StatusType"), "code", "sales_order_status")
	if err != nil || len(soStatusTypes) == 0 {
		t.Fatalf("list sales_order_status StatusType: %v", err)
	}
	soStatuses, err := engine.ListByField(ctx, defFor(t, tenantDB, "Status"), "status_type_id", soStatusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var soDraftStatusID string
	for _, s := range soStatuses {
		if code, _ := s.Data["code"].(string); code == "draft" {
			soDraftStatusID = s.ID
		}
	}

	so, err := engine.Create(ctx, defFor(t, tenantDB, "SalesOrder"), map[string]any{
		"so_number": "SO-1", "customer_id": customer.ID, "order_date": "2026-01-01",
		"status_id": soDraftStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("create SalesOrder: %v", err)
	}

	return customerInvoiceFixture{tenantDB: tenantDB, engine: engine, soID: so.ID, customerID: customer.ID, statusID: statusID}
}

func (fx customerInvoiceFixture) createDraftInvoice(t *testing.T, total float64) (id string, fields map[string]any, version int) {
	t.Helper()
	fields = map[string]any{
		"invoice_number": "INV-1", "sales_order_id": fx.soID, "customer_id": fx.customerID,
		"invoice_date": "2026-01-15", "status_id": fx.statusID["draft"], "total": total,
	}
	rec, err := fx.engine.Create(context.Background(), defFor(t, fx.tenantDB, "CustomerInvoice"), fields, humanActor())
	if err != nil {
		t.Fatalf("create CustomerInvoice: %v", err)
	}
	return rec.ID, fields, rec.Version
}

func TestPostCustomerInvoiceToLedger_PostsOnIssuedTransition(t *testing.T) {
	fx := setUpCustomerInvoiceFixture(t)
	fx.engine.SetHook("CustomerInvoice", PostCustomerInvoiceToLedger)
	ctx := context.Background()
	actor := humanActor()

	id, fields, version := fx.createDraftInvoice(t, 250.00)

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	if list, err := entries.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("expected no journal entry for a draft invoice, got %d (err=%v)", len(list), err)
	}

	fields["status_id"] = fx.statusID["issued"]
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "CustomerInvoice"), id, fields, &version, actor); err != nil {
		t.Fatalf("update to issued: %v", err)
	}

	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry after issuing, got %d", len(list))
	}
	entry := list[0]
	if entry.SourceType != "CustomerInvoice" || entry.SourceID != id {
		t.Fatalf("unexpected source: type=%s id=%s", entry.SourceType, entry.SourceID)
	}
	wantMinor := int64(250 * 100)
	byCode := map[string]data.JournalLine{}
	for _, l := range entry.Lines {
		byCode[l.AccountCode] = l
	}
	if ar := byCode["1200"]; ar.DebitMinor != wantMinor {
		t.Fatalf("expected AR (1200) debit %d, got %+v", wantMinor, ar)
	}
	if rev := byCode["4100"]; rev.CreditMinor != wantMinor {
		t.Fatalf("expected Revenue (4100) credit %d, got %+v", wantMinor, rev)
	}
}

// TestPostCustomerInvoiceToLedger_PaidTransition_DoesNotDoublePost
// confirms issued->paid never posts a second time (this hook only
// implements the issued posting — paid's own cash/AR entry is
// deliberately deferred, see this hook's own doc comment) and, just as
// importantly, doesn't re-post the *issued* entry again either.
func TestPostCustomerInvoiceToLedger_PaidTransition_DoesNotDoublePost(t *testing.T) {
	fx := setUpCustomerInvoiceFixture(t)
	fx.engine.SetHook("CustomerInvoice", PostCustomerInvoiceToLedger)
	ctx := context.Background()
	actor := humanActor()

	id, fields, version := fx.createDraftInvoice(t, 250.00)
	fields["status_id"] = fx.statusID["issued"]
	newVersion, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "CustomerInvoice"), id, fields, &version, actor)
	if err != nil {
		t.Fatalf("update to issued: %v", err)
	}

	fields["status_id"] = fx.statusID["paid"]
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "CustomerInvoice"), id, fields, &newVersion, actor); err != nil {
		t.Fatalf("update to paid: %v", err)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry total (issued only, paid not yet implemented), got %d", len(list))
	}
}

func TestPostCustomerInvoiceToLedger_ZeroTotal_PostsNothing(t *testing.T) {
	fx := setUpCustomerInvoiceFixture(t)
	fx.engine.SetHook("CustomerInvoice", PostCustomerInvoiceToLedger)
	ctx := context.Background()
	actor := humanActor()

	id, fields, version := fx.createDraftInvoice(t, 0)
	fields["status_id"] = fx.statusID["issued"]
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "CustomerInvoice"), id, fields, &version, actor); err != nil {
		t.Fatalf("update to issued: %v", err)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no journal entry for a zero-total invoice, got %d", len(list))
	}
}

func TestPostCustomerInvoiceToLedger_CreateAction_IsNoOp(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{"status_id": "y", "total": float64(100)}}
	if err := PostCustomerInvoiceToLedger(context.Background(), nil, nil, rec, audit.ActionCreate, humanActor()); err != nil {
		t.Fatalf("expected Create action to no-op without even touching tx, got: %v", err)
	}
}

// TestPostCustomerInvoiceToLedger_CurrencyIDDoesNotChangePostedScale pins
// the deliberate non-currency-awareness this hook keeps (uc-infra#163
// investigated making the posted amount scale by the invoice's own
// currency_id, and independent review caught why that's unsafe:
// journal_lines has no per-line currency/scale column, and every other
// reader of a posted minor-unit amount — internal/kernel/saft's
// formatMinor, money.Money.Major/String — unconditionally assumes
// money.Decimals. Posting a real 0dp-scaled amount there would post a
// technically-correct minor-unit count that every one of those readers
// then silently misinterprets by up to 100x). This test sets a
// non-default-scale (0dp JPY-style) currency_id and asserts the posted
// amount is UNCHANGED by it — still money.Decimals-scaled — so a future
// change that re-introduces currency-aware posting here without also
// fixing the GL's own scale-blindness fails this test rather than
// silently reintroducing the misstatement the review caught.
func TestPostCustomerInvoiceToLedger_CurrencyIDDoesNotChangePostedScale(t *testing.T) {
	fx := setUpCustomerInvoiceFixture(t)
	fx.engine.SetHook("CustomerInvoice", PostCustomerInvoiceToLedger)
	ctx := context.Background()
	actor := humanActor()

	jpy, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "Currency"), map[string]any{
		"code": "JPY", "name": "Japanese Yen", "minor_unit": float64(0),
	}, actor)
	if err != nil {
		t.Fatalf("create JPY Currency: %v", err)
	}

	id, fields, version := fx.createDraftInvoice(t, 1000.00)
	fields["currency_id"] = jpy.ID
	fields["status_id"] = fx.statusID["issued"]
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "CustomerInvoice"), id, fields, &version, actor); err != nil {
		t.Fatalf("update to issued: %v", err)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry, got %d", len(list))
	}
	wantMinor := int64(1000 * 100) // money.Decimals scale, NOT the JPY currency's own 0dp scale
	byCode := map[string]data.JournalLine{}
	for _, l := range list[0].Lines {
		byCode[l.AccountCode] = l
	}
	if ar := byCode["1200"]; ar.DebitMinor != wantMinor {
		t.Fatalf("AR debit = %d minor units, want %d — a 0dp currency_id must NOT change the posted scale (journal_lines has no per-line scale column)", ar.DebitMinor, wantMinor)
	}
	if rev := byCode["4100"]; rev.CreditMinor != wantMinor {
		t.Fatalf("Revenue credit = %d minor units, want %d — a 0dp currency_id must NOT change the posted scale (journal_lines has no per-line scale column)", rev.CreditMinor, wantMinor)
	}
}

// TestPostCustomerInvoiceToLedger_CorruptTotal_HardFails (uc-infra#163):
// unlike the old hardcoded-2dp ledger.ToMinorUnits (which never returned
// an error), money.FromMajorUnits validates its input and can reject a
// corrupt total (NaN/±Inf/overflow). That must fail the posting outright
// rather than silently posting a zero or garbage amount — entity.
// ValidateRecord would normally reject a NaN FieldNumber before this
// hook ever runs, so this calls the hook directly to exercise the
// defensive path a hook invoked directly, bypassing that validation,
// would still need.
//
// rec.ID must be a syntactically valid UUID, NOT an arbitrary string
// like "does-not-matter": ExistsForSource's own SQL runs BEFORE the
// total conversion this test means to exercise, and journal_entries.
// source_id is a Postgres UUID column — a non-UUID id fails there first
// (independent review caught this: the first version of this test used
// a non-UUID id, which meant it never actually reached
// money.FromMajorUnits and would have passed identically with that
// whole branch deleted). The nil UUID is syntactically valid and never
// a real row's id, so ExistsForSource cleanly returns false and
// execution reaches the total conversion this test is actually for.
func TestPostCustomerInvoiceToLedger_CorruptTotal_HardFails(t *testing.T) {
	fx := setUpCustomerInvoiceFixture(t)

	tx, err := fx.tenantDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rec := data.Record{ID: "00000000-0000-0000-0000-000000000000", Data: map[string]any{
		"status_id": fx.statusID["issued"], "total": math.NaN(),
	}}
	if err := PostCustomerInvoiceToLedger(context.Background(), tx, nil, rec, audit.ActionUpdate, humanActor()); err == nil {
		t.Fatal("expected an error for a NaN total, got nil")
	}
}
