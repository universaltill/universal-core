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
