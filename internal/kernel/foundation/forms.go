package foundation

import "github.com/universaltill/universal-core/internal/kernel/form"

// PartyForm is the first Form Definition for a foundation entity —
// Party specifically, since every module depends on it (see this
// package's own doc comment). Deliberately not published by Publish()
// above: a Form Definition is a presentation-layer choice, not the
// "always present, every module needs it to exist at all" guarantee
// ADR-0001 §8 makes for the entities themselves, so it doesn't belong in
// the same all-foundation-entities lifecycle. Other foundation entities
// don't have a form yet; add one here as each is actually needed by a
// real screen, rather than building all of them speculatively now.
func PartyForm() *form.Definition {
	return &form.Definition{
		EntityType: "Party",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "party_type", Label: "Type"},
					{Name: "name", Label: "Name"},
					{Name: "tax_id", Label: "Tax ID"},
					{Name: "status", Label: "Status"},
					{Name: "preferred_language", Label: "Preferred Language"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// IssueReportForm is the admin-facing triage view of a submitted
// IssueReport — not the submission UI itself (that's the bespoke
// capture page, internal/api/issuereport.go, same "a feature that
// doesn't fit the generic field-based form model gets its own page"
// precedent the CSV import wizard already established). This is what
// GET /forms/IssueReport/{id} and the generic /records/IssueReport list
// page (reachable from the module menu) actually render — reusing the
// generic form renderer for the read/triage side is exactly right,
// since "show a record's fields, let someone edit status" is precisely
// what that renderer is for.
func IssueReportForm() *form.Definition {
	return &form.Definition{
		EntityType: "IssueReport",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "title", Label: "Title"},
					{Name: "description", Label: "Description"},
					{Name: "transcript", Label: "Voice Transcript"},
					{Name: "page_url", Label: "Page"},
					{Name: "user_agent", Label: "Browser"},
					{Name: "status", Label: "Status"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// AllForms returns every Form Definition this package defines — the
// source of truth seed.go's PublishForms iterates, so a newly added form
// constructor is published automatically instead of needing a second,
// separately-maintained list a caller could forget to update (exactly
// the gap that left every form unpublished in production until
// PublishForms existed at all).
func AllForms() []*form.Definition {
	return []*form.Definition{PartyForm(), IssueReportForm()}
}
