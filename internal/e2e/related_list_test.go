package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
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
