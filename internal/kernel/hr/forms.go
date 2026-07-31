package hr

import "github.com/universaltill/universal-core/internal/kernel/form"

// EmployeeForm separates who the person is (a Party reference — the
// person's own details are edited on Party, not here) from what the
// employment is. The leave history is a read-only related list for the
// same reason it is a related list on the entity: it records decisions
// that outlive the employment.
func EmployeeForm() *form.Definition {
	return &form.Definition{
		EntityType: "Employee",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Employment",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "employee_number", Label: "Employee Number"},
					{Name: "party_id", Label: "Person"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Role and Dates",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "position_id", Label: "Position"},
					{Name: "department_id", Label: "Department"},
					{Name: "hire_date", Label: "Hire Date"},
					{Name: "end_date", Label: "End Date"},
				},
			},
			{
				Title:     "Leave History",
				Component: form.ComponentRelatedList,
				Target:    "LeaveRequest",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func LeaveRequestForm() *form.Definition {
	return &form.Definition{
		EntityType: "LeaveRequest",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Request",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "request_number", Label: "Request Number"},
					{Name: "employee_id", Label: "Employee"},
					{Name: "leave_type", Label: "Leave Type"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Dates",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "start_date", Label: "Start Date"},
					{Name: "end_date", Label: "End Date"},
					{Name: "days", Label: "Days"},
					{Name: "reason", Label: "Reason"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// AllForms returns every Form Definition this module adds.
func AllForms() []*form.Definition {
	return []*form.Definition{
		EmployeeForm(),
		LeaveRequestForm(),
	}
}
