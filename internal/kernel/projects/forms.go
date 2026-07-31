package projects

import "github.com/universaltill/universal-core/internal/kernel/form"

// ProjectForm edits its task list in place (master-detail), with no
// roll-up target: estimated hours sum to a number worth seeing, but
// storing that sum on Project would create a second source of truth
// that silently disagrees with the tasks the moment one is edited
// outside the form. Budget is what the project actually commits to.
func ProjectForm() *form.Definition {
	return &form.Definition{
		EntityType: "Project",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Project",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "project_code", Label: "Project Code"},
					{Name: "name", Label: "Name"},
					{Name: "customer_id", Label: "Customer"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Schedule and Budget",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "start_date", Label: "Start Date"},
					{Name: "end_date", Label: "End Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "budget", Label: "Budget"},
				},
			},
			{
				Title:     "Tasks",
				Component: form.ComponentMasterDetail,
				Target:    "Task",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// TaskForm shows a task's own logged time as a read-only related list —
// the hours belong to the people who logged them, not to the task's
// edit form.
func TaskForm() *form.Definition {
	return &form.Definition{
		EntityType: "Task",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Task",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "project_id", Label: "Project"},
					{Name: "parent_task_id", Label: "Parent Task"},
					{Name: "title", Label: "Title"},
					{Name: "assignee_id", Label: "Assignee"},
					{Name: "estimated_hours", Label: "Estimated Hours"},
					{Name: "due_date", Label: "Due Date"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Logged Time",
				Component: form.ComponentRelatedList,
				Target:    "TimeEntry",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func TimeEntryForm() *form.Definition {
	return &form.Definition{
		EntityType: "TimeEntry",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Time Entry",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "task_id", Label: "Task"},
					{Name: "employee_id", Label: "Employee"},
					{Name: "entry_date", Label: "Date"},
					{Name: "hours", Label: "Hours"},
					{Name: "billable", Label: "Billable"},
					{Name: "notes", Label: "Notes"},
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
		ProjectForm(),
		TaskForm(),
		TimeEntryForm(),
	}
}
