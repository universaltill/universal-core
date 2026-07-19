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
