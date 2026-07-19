package form

import "testing"

// purchaseOrderForm is the worked master-detail example from ADR-0017 §6:
// a header with a conditional LC-reference field, and a line-items grid
// that's a composition (master-detail), not a plain reference or a
// read-only related list.
func purchaseOrderForm() *Definition {
	return &Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []Section{
			{
				Title:     "Header",
				Component: ComponentFields,
				Fields: []FormField{
					{Name: "vendor_id"},
					{Name: "order_date"},
					{Name: "payment_method"},
					{Name: "lc_reference", VisibleIf: "payment_method == 'LC'"},
				},
			},
			{
				Title:        "Lines",
				Component:    ComponentMasterDetail,
				Target:       "POLine",
				RollUp:       "line_total",
				RollUpTarget: "total",
			},
		},
		Actions: []Action{
			{Label: "Save", Op: OpSave},
			{Label: "Submit for Approval", Op: OpWorkflowStart, Workflow: "po_approval"},
			{Label: "Print", Op: OpReportRender, Report: "po_print"},
		},
	}
}

func TestDefinitionValidate_ValidMasterDetailForm(t *testing.T) {
	d := purchaseOrderForm()
	if err := d.Validate(); err != nil {
		t.Fatalf("expected valid form to pass, got %v", err)
	}
}

func TestDefinitionValidate_MissingEntityType(t *testing.T) {
	d := &Definition{Sections: []Section{{Component: ComponentFields, Fields: []FormField{{Name: "x"}}}}}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for missing entity_type")
	}
}

func TestDefinitionValidate_FieldsComponentWithNoFields(t *testing.T) {
	d := &Definition{
		EntityType: "Vendor",
		Sections:   []Section{{Title: "Empty", Component: ComponentFields}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for fields component with no fields")
	}
}

func TestDefinitionValidate_MasterDetailWithoutTarget(t *testing.T) {
	d := &Definition{
		EntityType: "PurchaseOrder",
		Sections:   []Section{{Title: "Lines", Component: ComponentMasterDetail}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for master_detail section without a target")
	}
}

func TestDefinitionValidate_RelatedListWithoutTarget(t *testing.T) {
	d := &Definition{
		EntityType: "Customer",
		Sections:   []Section{{Title: "Past Orders", Component: ComponentRelatedList}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for related_list section without a target")
	}
}

func TestDefinitionValidate_UnknownComponent(t *testing.T) {
	d := &Definition{
		EntityType: "Vendor",
		Sections:   []Section{{Title: "Weird", Component: "chart"}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for unknown component")
	}
}

func TestDefinitionValidate_WorkflowActionWithoutWorkflowName(t *testing.T) {
	d := &Definition{
		EntityType: "PurchaseOrder",
		Sections:   []Section{{Title: "H", Component: ComponentFields, Fields: []FormField{{Name: "x"}}}},
		Actions:    []Action{{Label: "Submit", Op: OpWorkflowStart}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for workflow.start action with no workflow name")
	}
}

func TestDefinitionValidate_NavigateActionWithoutRoute(t *testing.T) {
	d := &Definition{
		EntityType: "PurchaseOrder",
		Sections:   []Section{{Title: "H", Component: ComponentFields, Fields: []FormField{{Name: "x"}}}},
		Actions:    []Action{{Label: "Back", Op: OpNavigate}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for navigate action with no route")
	}
}

func TestDefinitionValidate_ReportActionWithoutReportName(t *testing.T) {
	d := &Definition{
		EntityType: "PurchaseOrder",
		Sections:   []Section{{Title: "H", Component: ComponentFields, Fields: []FormField{{Name: "x"}}}},
		Actions:    []Action{{Label: "Print", Op: OpReportRender}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for report.render action with no report name")
	}
}

// TestDefinitionValidate_RejectsArbitraryOp is the regression test for the
// ADR-0017 §6 guardrail: actions must stay a closed, declarative set. If
// this test ever needs to be relaxed to let a tenant or an AI draft invent
// a new op freely, that's a sign the form schema has started growing into
// a scripting language and should be stopped.
func TestDefinitionValidate_RejectsArbitraryOp(t *testing.T) {
	d := &Definition{
		EntityType: "Vendor",
		Sections:   []Section{{Title: "H", Component: ComponentFields, Fields: []FormField{{Name: "x"}}}},
		Actions:    []Action{{Label: "Do Something", Op: "run_script"}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: arbitrary action ops must be rejected")
	}
}
