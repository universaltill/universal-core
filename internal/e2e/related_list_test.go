package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
)

// TestRelatedList_ShowsOnlyThisRecordsChildren is the browser-level
// regression test for board #20's second blocker, and the layer whose
// absence let it through in the first place: every other test asserted
// markup structure, and the section's markup was structurally fine —
// it was the CONTENT, arriving later via htmx, that was wrong.
//
// The section used to render empty with hx-trigger="load" pointing at
// an endpoint that ignored its ref filter, so on load the browser
// replaced the section with a JSON dump of every record of the target
// type in the tenant. A parent's "history" therefore listed other
// parents' children. Only a real browser settles this: it needs the
// load trigger to actually fire (or provably not exist) and the final
// DOM to be inspected after it would have.
func TestRelatedList_ShowsOnlyThisRecordsChildren(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	machineDef := &entity.Definition{
		EntityType: "RelMachine",
		Version:    1,
		Fields:     []entity.Field{{Name: "code", Type: entity.FieldString, Required: true}},
		Relationships: []entity.Relationship{
			{Name: "jobs", Kind: entity.RelationRelatedList, Target: "RelJob", ParentField: "machine_id"},
		},
	}
	machineForm := &form.Definition{
		EntityType: "RelMachine",
		Version:    1,
		Sections: []form.Section{
			{Title: "Machine", Component: form.ComponentFields,
				Fields: []form.FormField{{Name: "code", Label: "Code"}}},
			{Title: "Job History", Component: form.ComponentRelatedList, Target: "RelJob"},
		},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	jobDef := &entity.Definition{
		EntityType: "RelJob",
		Version:    1,
		Fields: []entity.Field{
			{Name: "machine_id", Type: entity.FieldReference, Required: true, Target: "RelMachine"},
			{Name: "note", Type: entity.FieldString, Required: true},
		},
	}
	jobForm := &form.Definition{
		EntityType: "RelJob",
		Version:    1,
		Sections: []form.Section{{Title: "Job", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "machine_id"}, {Name: "note"}}}},
		Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
	}
	publishDef(t, tenantDB, machineDef, machineForm)
	publishDef(t, tenantDB, jobDef, jobForm)

	ctx := context.Background()
	engine := crud.NewEngine(tenantDB)
	actor := humanActor()
	mineID := ""
	for _, m := range []struct{ code, note string }{
		{"MACHINE-MINE", "JOB-BELONGS-TO-MINE"},
		{"MACHINE-OTHER", "JOB-BELONGS-TO-OTHER"},
	} {
		machine, err := engine.Create(ctx, machineDef, map[string]any{"code": m.code}, actor)
		if err != nil {
			t.Fatalf("create %s: %v", m.code, err)
		}
		if m.code == "MACHINE-MINE" {
			mineID = machine.ID
		}
		if _, err := engine.Create(ctx, jobDef, map[string]any{
			"machine_id": machine.ID, "note": m.note,
		}, actor); err != nil {
			t.Fatalf("create job for %s: %v", m.code, err)
		}
	}

	browser := browserCtx(t, tenantID)
	var sectionText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/RelMachine/"+mineID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		// Settle: if a load-triggered fetch existed it would have fired
		// and swapped the section by now.
		chromedp.WaitVisible(`.uc-related-list`, chromedp.ByQuery),
		chromedp.Text(`.uc-related-list`, &sectionText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render machine form: %v", err)
	}

	if !strings.Contains(sectionText, "JOB-BELONGS-TO-MINE") {
		t.Errorf("related list is missing this record's own child:\n%s", sectionText)
	}
	if strings.Contains(sectionText, "JOB-BELONGS-TO-OTHER") {
		t.Errorf("related list leaked another record's child — this is the bug:\n%s", sectionText)
	}
	// The old failure mode rendered raw JSON into the section.
	if strings.Contains(sectionText, `"data":`) || strings.Contains(sectionText, `"error":`) {
		t.Errorf("related list contains a raw JSON envelope rather than rendered rows:\n%s", sectionText)
	}

	// And no fetch is armed on the section at all.
	var hxGet, hxTrigger string
	var ok bool
	if err := chromedp.Run(browser,
		chromedp.AttributeValue(`.uc-related-list`, "hx-get", &hxGet, &ok, chromedp.ByQuery),
		chromedp.AttributeValue(`.uc-related-list`, "hx-trigger", &hxTrigger, &ok, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read related-list attributes: %v", err)
	}
	if hxGet != "" || hxTrigger != "" {
		t.Errorf("related list should not lazy-load (hx-get=%q hx-trigger=%q)", hxGet, hxTrigger)
	}
}

// TestRelatedList_ColumnHeadersAreLocalized_RealBrowser (uc-infra#103):
// the Leave History related_list's column headers are real, rendered,
// translated text in a real browser — a rendered-HTML-string test (the
// formrender unit tests covering the same fix) proves the markup shape
// but, per #99's own tests, can't prove a browser actually shows the
// resolved text rather than raw Go template output some intermediate
// layer mangled. Uses the real Employee/LeaveRequest production forms
// (hr.EmployeeForm's "Leave History" section), not a synthetic form, so
// this is a proof the production wiring resolves.
//
// Driven in TURKISH (?lang=tr): LeaveRequest's first declared field,
// request_number, isn't an English word a coincidental match could
// produce, but asserting the real Turkish catalog text is still the
// only way to prove buildChildRows' related_list branch is actually
// resolved through field.{EntityType}.{FieldName} rather than left
// unrendered (the bug this card fixes) or left as the raw field name.
func TestRelatedList_ColumnHeadersAreLocalized_RealBrowser(t *testing.T) {
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
		{"hr", func() error { return hr.Publish(ctx, tenantDB, actor) }},
		{"hr forms", func() error { return hr.PublishForms(ctx, tenantDB, actor) }},
		{"hr statuses", func() error { return hr.PublishStatuses(ctx, tenantDB, actor) }},
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
	status := func(typeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, def("StatusType"), "code", typeCode)
		if len(types) == 0 {
			t.Fatalf("no StatusType %s", typeCode)
		}
		rows, _ := engine.ListByField(ctx, def("Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s/%s", typeCode, code)
		return ""
	}

	person, err := engine.Create(ctx, def("Party"), map[string]any{
		"party_type": "person", "name": "Demo Employee", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	employee, err := engine.Create(ctx, def("Employee"), map[string]any{
		"employee_number": "EMP-5150", "party_id": person.ID,
		"hire_date": "2024-01-08", "status_id": status("employee_status", "active"),
	}, actor)
	if err != nil {
		t.Fatalf("create Employee: %v", err)
	}
	if _, err := engine.Create(ctx, def("LeaveRequest"), map[string]any{
		"request_number": "LV-9002", "employee_id": employee.ID, "leave_type": "annual",
		"start_date": "2026-08-10", "end_date": "2026-08-14", "days": 5.0,
		"status_id": status("leave_request_status", "submitted"),
	}, actor); err != nil {
		t.Fatalf("create LeaveRequest: %v", err)
	}

	browser := browserCtx(t, tenantID)
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Employee/"+employee.ID+"?lang=tr"),
		chromedp.WaitVisible(`.uc-related-header`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /forms/Employee/%s?lang=tr: %v", employee.ID, err)
	}

	// First declared LeaveRequest field is request_number -> "Talep No";
	// the raw field name stays in data-field so a stable selector still
	// works regardless of locale, same convention as master_detail's
	// <th data-field="...">.
	var firstHeader string
	if err := chromedp.Run(browser, chromedp.Text(`.uc-related-header-cell[data-field="request_number"]`, &firstHeader, chromedp.ByQuery)); err != nil {
		t.Fatalf("read first column header: %v", err)
	}
	if got := strings.TrimSpace(firstHeader); got != "Talep No" {
		t.Fatalf("expected the Turkish translation of the request_number column header, got %q", got)
	}

	var rowText string
	if err := chromedp.Run(browser, chromedp.Text(`.uc-related-list`, &rowText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read related list content: %v", err)
	}
	if !strings.Contains(rowText, "LV-9002") {
		t.Errorf("expected the leave request's own row to still render alongside its header:\n%s", rowText)
	}
}
