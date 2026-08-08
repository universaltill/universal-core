package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
)

// TestHR_EmployeeIsLabelledNotAUUID is the browser test board #16
// shipped without — and, exactly as with #18, the reason its blocker
// survived the whole dev cycle: every other test asserted structure,
// and the structure was fine. The CONTENT was a raw UUID.
//
// ADR-0013 deliberately gives Employee no name of its own (the person's
// name is Party's), which meant the kernel's name/title label
// convention found nothing and fell back to the record id — in every
// picker, list cell and export. Worse, the reference picker can only
// sort/filter by a field it knows to be the label, so it silently
// degraded to an unsearchable capped listing: past that cap the right
// employee is not merely hard to find, it is unreachable from the form.
func TestHR_EmployeeIsLabelledNotAUUID(t *testing.T) {
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
		"employee_number": "EMP-4242", "party_id": person.ID,
		"hire_date": "2024-01-08", "status_id": status("employee_status", "active"),
	}, actor)
	if err != nil {
		t.Fatalf("create Employee: %v", err)
	}
	if _, err := engine.Create(ctx, def("LeaveRequest"), map[string]any{
		"request_number": "LV-9001", "employee_id": employee.ID, "leave_type": "annual",
		"start_date": "2026-08-10", "end_date": "2026-08-14", "days": 5.0,
		"status_id": status("leave_request_status", "submitted"),
	}, actor); err != nil {
		t.Fatalf("create LeaveRequest: %v", err)
	}

	// 1. The picker endpoint the New-Leave-Request form calls.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/references/Employee", nil)
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "e2e")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/references/Employee: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var envelope struct {
		Data []struct{ ID, Label string } `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode reference response: %v (%s)", err, body)
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("expected one employee option, got %s", body)
	}
	if got := envelope.Data[0]; got.Label == got.ID {
		t.Errorf("the employee picker labels the record with its own UUID — unusable and unsearchable: %s", body)
	} else if got.Label != "EMP-4242" {
		t.Errorf("picker label = %q, want the employee number", got.Label)
	}

	// 2. The rendered list page must show the same, not a UUID.
	browser := browserCtx(t, tenantID)
	var listText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/LeaveRequest"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &listText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render LeaveRequest list: %v", err)
	}
	if strings.Contains(listText, employee.ID) {
		t.Errorf("the leave-request list shows the employee's raw UUID:\n%s", listText)
	}
	if !strings.Contains(listText, "EMP-4242") {
		t.Errorf("the leave-request list does not resolve its employee reference:\n%s", listText)
	}

	// 3. The Employee form's own Leave History related list renders its
	//    rows server-side (board #20's fix) rather than a JSON dump.
	var historyText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Employee/"+employee.ID),
		chromedp.WaitVisible(`.uc-related-list`, chromedp.ByQuery),
		chromedp.Text(`.uc-related-list`, &historyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render Employee form: %v", err)
	}
	if !strings.Contains(historyText, "LV-9001") {
		t.Errorf("the employee's leave history is missing their own request:\n%s", historyText)
	}
	if strings.Contains(historyText, `"data":`) || strings.Contains(historyText, "map[") {
		t.Errorf("leave history rendered raw JSON or a Go map:\n%s", historyText)
	}
}

// TestHR_AttendanceListIsFilterableByEmployee is the browser test board
// #17 shipped without — the third card running to omit one, and again
// the layer that would have caught the blocker.
//
// AttendanceRecord is deliberately NOT a related list on Employee, and
// the justification in the module's own comment was that attendance is
// "reachable from its own list page filtered by employee". That was
// empirically false: the list filter does a substring match against the
// STORED value, which for a reference column is a UUID, so typing the
// employee number a user can plainly see in the cell returned nothing.
// With 200 employees × 250 working days there is no way to find one
// person's attendance at all. The filter is now reference-aware
// generically; this asserts it in a real browser rather than trusting
// the SQL.
func TestHR_AttendanceListIsFilterableByEmployee(t *testing.T) {
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
	activeStatus := func() string {
		t.Helper()
		types, _ := engine.ListByField(ctx, def("StatusType"), "code", "employee_status")
		rows, _ := engine.ListByField(ctx, def("Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == "active" {
				return r.ID
			}
		}
		t.Fatal("no active employee status")
		return ""
	}()

	empDef := def("Employee")
	attDef := def("AttendanceRecord")
	for _, e := range []struct{ number, personName, date string }{
		{"EMP-0001", "Demo Alice", "2026-07-27"},
		{"EMP-0002", "Demo Bob", "2026-07-28"},
	} {
		person, err := engine.Create(ctx, def("Party"), map[string]any{
			"party_type": "person", "name": e.personName, "status": "active",
		}, actor)
		if err != nil {
			t.Fatalf("create Party %s: %v", e.personName, err)
		}
		emp, err := engine.Create(ctx, empDef, map[string]any{
			"employee_number": e.number, "party_id": person.ID,
			"hire_date": "2024-01-08", "status_id": activeStatus,
		}, actor)
		if err != nil {
			t.Fatalf("create Employee %s: %v", e.number, err)
		}
		if _, err := engine.Create(ctx, attDef, map[string]any{
			"employee_id": emp.ID, "entry_date": e.date,
			"hours_worked": 8.0, "source": "clock",
		}, actor); err != nil {
			t.Fatalf("create AttendanceRecord for %s: %v", e.number, err)
		}
	}

	browser := browserCtx(t, tenantID)

	// Unfiltered: both rows, and the employee column resolves to
	// employee numbers rather than the UUIDs it stores.
	var all string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/AttendanceRecord"),
		chromedp.WaitVisible(`table`, chromedp.ByQuery),
		chromedp.Text(`table`, &all, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render attendance list: %v", err)
	}
	if !strings.Contains(all, "EMP-0001") || !strings.Contains(all, "EMP-0002") {
		t.Fatalf("attendance list does not resolve its employee references:\n%s", all)
	}

	// Filtered by what the user can actually see in the cell.
	var filtered string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/AttendanceRecord?filter=employee_id&q=EMP-0001"),
		chromedp.WaitVisible(`main`, chromedp.ByQuery),
		chromedp.Text(`main`, &filtered, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render filtered attendance list: %v", err)
	}
	if !strings.Contains(filtered, "EMP-0001") {
		t.Errorf("filtering by an employee number found nothing — the number is what the cell shows:\n%s", filtered)
	}
	if strings.Contains(filtered, "EMP-0002") {
		t.Errorf("the filter did not exclude the other employee's attendance:\n%s", filtered)
	}

	// A term that matches no employee must return no rows, not silently
	// drop the filter and show everything.
	var none string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/records/AttendanceRecord?filter=employee_id&q=EMP-NOBODY"),
		chromedp.WaitVisible(`main`, chromedp.ByQuery),
		chromedp.Text(`main`, &none, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render no-match attendance list: %v", err)
	}
	if strings.Contains(none, "EMP-0001") || strings.Contains(none, "EMP-0002") {
		t.Errorf("a filter matching no employee must return no rows, got:\n%s", none)
	}
}

// TestHR_EmployeeCostRateRendersAsMoneyDecimal_RealBrowser (uc-infra#134,
// independent review finding) is the real-browser counterpart the
// "Compensation" section's cost_rate FieldMoney input didn't have: a
// rendered-HTML-string test proves the input is present in the markup,
// never that the browser actually shows the right DECIMAL VALUE — the
// same distinction CLAUDE.md draws for CSS/script behavior, applied
// here to formrender's minor-to-major money conversion (render.go's
// FieldMoney case: `fv.Value = m.String()`). This is also the first e2e
// coverage anywhere in this repo for a plain, non-roll-up FieldMoney
// input's decimal rendering — every existing money e2e test
// (TestPurchaseOrderForm_RollUpTotalRendersAsMoneyDecimal_RealBrowser)
// covers a roll-up TOTAL cell, not an ordinary editable money field like
// unit_price or this one.
func TestHR_EmployeeCostRateRendersAsMoneyDecimal_RealBrowser(t *testing.T) {
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
		"party_type": "person", "name": "Rate Test Employee", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	// cost_rate stored in MINOR units (6550 = $65.50/hr) — the field this
	// whole test exists to check renders correctly back out in MAJOR
	// units, not the raw stored integer.
	employee, err := engine.Create(ctx, def("Employee"), map[string]any{
		"employee_number": "EMP-RATE-1", "party_id": person.ID,
		"hire_date": "2024-01-08", "status_id": status("employee_status", "active"),
		"cost_rate": float64(6550),
	}, actor)
	if err != nil {
		t.Fatalf("create Employee: %v", err)
	}

	browser := browserCtx(t, tenantID)
	var sectionText, inputValue, stepAttr string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Employee/"+employee.ID),
		chromedp.WaitVisible(`input[name="cost_rate"]`, chromedp.ByQuery),
		chromedp.Text(`body`, &sectionText, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="cost_rate"]').value`, &inputValue),
		chromedp.Evaluate(`document.querySelector('input[name="cost_rate"]').step`, &stepAttr),
	); err != nil {
		t.Fatalf("render Employee form: %v", err)
	}
	if !strings.Contains(sectionText, "Compensation") {
		t.Errorf("the Compensation section title is missing from the rendered form:\n%s", sectionText)
	}
	if inputValue != "65.50" {
		t.Errorf("cost_rate input's real DOM value = %q, want \"65.50\" — 6550 minor units rendered as major-unit decimal (a markup-only test would have missed a 100x-off or unconverted-integer regression here)", inputValue)
	}
	if stepAttr != "0.01" {
		t.Errorf("cost_rate input's step = %q, want \"0.01\" (a 2-decimal money input)", stepAttr)
	}

	// A fresh (no cost_rate) Employee's create form must show an EMPTY
	// input, not "0.00" — render.go's own doc comment on this exact
	// point: coercing an absent optional money value to a displayed zero
	// would contradict the whole "absent means unknown, not $0" design
	// this field exists for (hr.go's own field doc comment).
	var newFormValue string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Employee/new"),
		chromedp.WaitVisible(`input[name="cost_rate"]`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="cost_rate"]').value`, &newFormValue),
	); err != nil {
		t.Fatalf("render Employee new form: %v", err)
	}
	if newFormValue != "" {
		t.Errorf("a new Employee's cost_rate input value = %q, want empty — an unset optional money field must not render as a displayed zero", newFormValue)
	}
}
