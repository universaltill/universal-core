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

// TestCRM_PipelineListsAndCampaignRelatedList covers the pipeline half
// of the module in a real browser.
//
// The related list is the reason this test exists rather than a
// rendered-string one. That mechanism has been broken twice in this
// repo in ways markup assertions passed straight through: once it
// rendered an empty table and then lazy-loaded an unfiltered dump of
// every record of the target type, and once it printed raw Go maps for
// i18n fields. So the assertion here is not "a related list exists" —
// it is that the campaign's own lead is in it and the other campaign's
// lead is not.
func TestCRM_PipelineListsAndCampaignRelatedList(t *testing.T) {
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
	statusID := func(statusTypeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, def("StatusType"), "code", statusTypeCode)
		if len(types) == 0 {
			t.Fatalf("no StatusType %s", statusTypeCode)
		}
		rows, _ := engine.ListByField(ctx, def("Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s/%s", statusTypeCode, code)
		return ""
	}

	campaign := func(name, channel string) string {
		t.Helper()
		rec, err := engine.Create(ctx, def("Campaign"), map[string]any{
			"name": name, "channel": channel, "budget": 5000.0,
			"start_date": "2026-09-01", "end_date": "2026-09-30",
			"status_id": statusID("campaign_status", "active"),
		}, actor)
		if err != nil {
			t.Fatalf("create Campaign %s: %v", name, err)
		}
		return rec.ID
	}
	expoID := campaign("Autumn Expo", "event")
	mailerID := campaign("Quarterly Mailer", "email")

	mkLead := func(name, campaignID string) {
		t.Helper()
		if _, err := engine.Create(ctx, def("Lead"), map[string]any{
			"name": name, "company_name": name + " Ltd", "source": "event",
			"campaign_id": campaignID, "status_id": statusID("lead_status", "new"),
		}, actor); err != nil {
			t.Fatalf("create Lead %s: %v", name, err)
		}
	}
	mkLead("Expo Visitor", expoID)
	mkLead("Mailer Respondent", mailerID)

	customer, err := engine.Create(ctx, def("Party"), map[string]any{
		"party_type": "organization", "name": "Westgate Wholesale", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create customer Party: %v", err)
	}
	opp, err := engine.Create(ctx, def("Opportunity"), map[string]any{
		"name": "Westgate rollout", "customer_id": customer.ID, "amount": 82000.0,
		"probability": 40.0, "expected_close_date": "2026-11-30",
		"status_id": statusID("opportunity_stage", "proposal"),
	}, actor)
	if err != nil {
		t.Fatalf("create Opportunity: %v", err)
	}

	browser := browserCtx(t, tenantID)

	// The campaign form's related list shows this campaign's lead and
	// not the other one's.
	var relatedText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Campaign/"+expoID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Text(`form.uc-form`, &relatedText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render campaign form: %v", err)
	}
	if !strings.Contains(relatedText, "Expo Visitor") {
		t.Errorf("the campaign's own lead is missing from its related list:\n%s", relatedText)
	}
	if strings.Contains(relatedText, "Mailer Respondent") {
		t.Errorf("the related list leaked another campaign's lead — it is not filtered by campaign_id:\n%s", relatedText)
	}

	// The Lead list resolves campaign_id to the campaign's name. Lead
	// and Campaign both label by `name` through the kernel's default
	// convention rather than a declared LabelField; this is that
	// convention actually working in a browser, not in a unit assertion.
	var leadList string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/Lead"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &leadList, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render lead list: %v", err)
	}
	if !strings.Contains(leadList, "Autumn Expo") {
		t.Errorf("the lead list shows a raw campaign id instead of the campaign name:\n%s", leadList)
	}

	// The opportunity form renders its three sections, and its customer
	// picker shows the party's NAME as the selected option. Reading the
	// option label rather than the select's value is the whole point:
	// the value is a UUID either way, so a test that checked it would
	// pass just as happily against the raw-id rendering #16 shipped.
	var sections int
	var customerLabel, stageLabel string
	// A reference renders as .uc-ref: a hidden input carrying the UUID
	// plus a .uc-ref-search text box whose value is the human-readable
	// label. Reading the search box is deliberate — the hidden input is
	// a UUID even when everything works, so asserting on it would pass
	// against the raw-id rendering #16 shipped.
	pickerLabel := func(field string) string {
		return `(() => {
			const wrap = document.querySelector('form.uc-form .uc-ref[data-field="` + field + `"]');
			if (!wrap) return '<no ` + field + ` picker>';
			const box = wrap.querySelector('.uc-ref-search');
			return box ? box.value : '<no search box>';
		})()`
	}
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Opportunity/"+opp.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('form.uc-form section.uc-section').length`, &sections),
		chromedp.Evaluate(pickerLabel("customer_id"), &customerLabel),
		chromedp.Evaluate(pickerLabel("status_id"), &stageLabel),
	); err != nil {
		t.Fatalf("render opportunity form: %v", err)
	}
	if sections != 3 {
		t.Errorf("opportunity form rendered %d sections, want 3", sections)
	}
	if customerLabel != "Westgate Wholesale" {
		t.Errorf("customer picker shows %q, want the party name — a UUID here is the #16 regression", customerLabel)
	}
	if stageLabel != "Proposal" {
		t.Errorf("stage picker shows %q, want the status name Proposal", stageLabel)
	}
}
