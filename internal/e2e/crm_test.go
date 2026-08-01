package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestCRM_CaseListAndForm is written WITH the module rather than after
// an independent review demanded it — the previous three cards each
// shipped a rendered surface with no browser test, and each time that
// is exactly where the blocker hid, because every test written asserted
// definition shape and the shape was always fine.
//
// It checks the three things a rendered-string test cannot: that the
// list resolves its reference columns to labels rather than UUIDs, that
// the filter finds a case by the customer name a user can actually see
// in the cell (the #17 fix, which this module inherits for free), and
// that the form renders its three sections with a working priority
// select.
func TestCRM_CaseListAndForm(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"foundation", func() error { return foundation.Publish(ctx, tenantDB, actor) }},
		{"foundation forms", func() error { return foundation.PublishForms(ctx, tenantDB, actor) }},
		{"crm", func() error { return crm.Publish(ctx, tenantDB, actor) }},
		{"crm forms", func() error { return crm.PublishForms(ctx, tenantDB, actor) }},
		{"crm statuses", func() error { return crm.PublishStatuses(ctx, tenantDB, actor) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)
	def := func(entityType string) *entity.Definition {
		t.Helper()
		v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	statusID := func(code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, def("StatusType"), "code", "case_status")
		rows, _ := engine.ListByField(ctx, def("Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no case_status/%s", code)
		return ""
	}

	caseDef := def("Case")
	var firstCase string
	for _, c := range []struct{ customer, number, subject string }{
		{"Northwind Retail", "CASE-1001", "Unit fails self-test"},
		{"Southbrook Supplies", "CASE-1002", "Invoice address wrong"},
	} {
		party, err := engine.Create(ctx, def("Party"), map[string]any{
			"party_type": "organization", "name": c.customer, "status": "active",
		}, actor)
		if err != nil {
			t.Fatalf("create Party %s: %v", c.customer, err)
		}
		rec, err := engine.Create(ctx, caseDef, map[string]any{
			"case_number": c.number, "subject": c.subject, "customer_id": party.ID,
			"priority": "high", "opened_date": "2026-07-29", "status_id": statusID("new"),
		}, actor)
		if err != nil {
			t.Fatalf("create Case %s: %v", c.number, err)
		}
		if firstCase == "" {
			firstCase = rec.ID
		}
	}

	browser := browserCtx(t, tenantID)

	// The list resolves the customer reference to a name, not a UUID.
	var listText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/Case"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render case list: %v", err)
	}
	if !strings.Contains(listText, "Northwind Retail") || !strings.Contains(listText, "Southbrook Supplies") {
		t.Errorf("case list does not resolve its customer references:\n%s", listText)
	}

	// Filtering by the customer name shown in the cell narrows to that
	// customer — the reference-aware filter from #17, inherited here.
	var filtered string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/Case?filter=customer_id&q=Northwind"),
		chromedp.WaitVisible(`main`, chromedp.ByQuery),
		chromedp.Text(`main`, &filtered, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render filtered case list: %v", err)
	}
	if !strings.Contains(filtered, "CASE-1001") {
		t.Errorf("filtering by a customer name found nothing — that name is what the cell shows:\n%s", filtered)
	}
	if strings.Contains(filtered, "CASE-1002") {
		t.Errorf("the filter did not exclude the other customer's case:\n%s", filtered)
	}

	// The form renders all three sections, with priority as a real
	// select carrying the stored value.
	var sections int
	var priority string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Case/"+firstCase),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('form.uc-form section.uc-section').length`, &sections),
		chromedp.Value(`select[name="priority"]`, &priority, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render case form: %v", err)
	}
	if sections != 3 {
		t.Errorf("case form rendered %d sections, want 3", sections)
	}
	if priority != "high" {
		t.Errorf("priority select = %q, want the stored value high", priority)
	}
}
