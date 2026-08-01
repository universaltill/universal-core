package foundation

import "github.com/universaltill/universal-core/internal/kernel/form"

// PartyForm was the first Form Definition for a foundation entity —
// Party specifically, since every module depends on it (see this
// package's own doc comment). Deliberately not published by Publish()
// above: a Form Definition is a presentation-layer choice, not the
// "always present, every module needs it to exist at all" guarantee
// ADR-0001 §8 makes for the entities themselves, so it doesn't belong in
// the same all-foundation-entities lifecycle. Every other foundation
// entity now has one too (AllForms' own doc comment below covers why
// that took until 2026-07-26 and what gap it closed) — this file no
// longer has a "not built yet" foundation entity left.
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
		// Version bumped 1->2 alongside IssueReport entity's own v1->v2
		// (foundation.go) to carry the new console_log field into the
		// triage view — a field a human can never actually see and act on
		// otherwise, since this is the only surface that renders a
		// submitted report.
		Version: 2,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "title", Label: "Title"},
					{Name: "description", Label: "Description"},
					{Name: "transcript", Label: "Voice Transcript"},
					{Name: "console_log", Label: "Console Log"},
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
//
// The remaining 12 foundation entities' forms were added 2026-07-26,
// closing a real gap this doc comment's own earlier wording ("add one
// here as each is actually needed") had left open far longer than
// intended: accessibleModules (internal/api/dashboard.go) only shows an
// entity in the module hub/menu once BOTH its entity Definition AND its
// Form Definition are published — every foundation entity without a
// form here was completely invisible in the UI (no module-menu node, no
// /forms/{entityType}/new|{id} route reachable) despite being real,
// already-seeded, already-referenced data (Currency and UnitOfMeasure in
// particular are actively created by cmd/seed-demo-data and referenced
// by Item/PurchaseOrder, yet had no way to view or edit a single one
// through the app itself). AIProviderConnection is the one deliberate,
// still-standing exception — see its own doc comment in foundation.go:
// a generic form would render its encrypted API key as a plain text
// box, so its settings page is bespoke and stays that way.
func AllForms() []*form.Definition {
	return []*form.Definition{
		PartyForm(), IssueReportForm(),
		PartyRoleForm(), PartyRelationshipForm(), AddressForm(), ContactMechanismForm(),
		AttachmentForm(), UnitOfMeasureForm(), UomConversionForm(), CurrencyForm(),
		ExchangeRateForm(), StatusTypeForm(), StatusForm(), StatusTransitionForm(),
		RoleForm(), UserRoleForm(),
		PermissionForm(), FieldPermissionForm(),
		DepartmentForm(), PositionForm(),
		DelegationForm(),
	}
}

func DepartmentForm() *form.Definition {
	return &form.Definition{
		EntityType: "Department",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "parent_department_id", Label: "Parent Department"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func PositionForm() *form.Definition {
	return &form.Definition{
		EntityType: "Position",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "title", Label: "Title"},
					{Name: "department_id", Label: "Department"},
					{Name: "reports_to_position_id", Label: "Reports To"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// DelegationForm is the generated screen for creating/revoking a
// substitute-approver delegation (Delegation, foundation.go) — plain
// user-id text fields, same shape as UserRoleForm's own user_id input,
// since there is no Core-side User/Person entity to pick from either
// (see UserRole's own doc comment).
func DelegationForm() *form.Definition {
	return &form.Definition{
		EntityType: "Delegation",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "delegator_user_id", Label: "Delegator (user id)"},
					{Name: "delegate_user_id", Label: "Delegate (user id)"},
					{Name: "ends_at", Label: "Ends At"},
					{Name: "revoked", Label: "Revoked"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func PermissionForm() *form.Definition {
	return &form.Definition{
		EntityType: "Permission",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "role_id", Label: "Role"},
					{Name: "entity_type", Label: "Entity type"},
					{Name: "can_read", Label: "Can read"},
					{Name: "can_write", Label: "Can write"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func FieldPermissionForm() *form.Definition {
	return &form.Definition{
		EntityType: "FieldPermission",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "role_id", Label: "Role"},
					{Name: "entity_type", Label: "Entity type"},
					{Name: "field_name", Label: "Field name"},
					{Name: "hidden", Label: "Hidden"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func RoleForm() *form.Definition {
	return &form.Definition{
		EntityType: "Role",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "description", Label: "Description"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// UserRoleForm deliberately does NOT surface department_id (v2 on the
// entity, see UserRole's own doc comment) yet, even though the field is
// real and already validates/persists — found by independent review:
// exposing it as an editable picker here, before
// internal/kernel/authz.loadRoles reads DepartmentID at all, would let
// a tenant admin author "Finance Manager, Sales only" and believe it's
// enforced when authorization silently ignores the scope entirely
// (worse for department-scoping a tenant_admin grant, which would still
// resolve to full unrestricted admin). Add this field to the form in
// the same change that wires DepartmentID into enforcement, not before.
func UserRoleForm() *form.Definition {
	return &form.Definition{
		EntityType: "UserRole",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "user_id", Label: "User"},
					{Name: "role_id", Label: "Role"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func PartyRoleForm() *form.Definition {
	return &form.Definition{
		EntityType: "PartyRole",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "party_id", Label: "Party"},
					{Name: "role_type", Label: "Role"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func PartyRelationshipForm() *form.Definition {
	return &form.Definition{
		EntityType: "PartyRelationship",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "party_id_from", Label: "From Party"},
					{Name: "party_id_to", Label: "To Party"},
					{Name: "relationship_type", Label: "Relationship"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func AddressForm() *form.Definition {
	return &form.Definition{
		EntityType: "Address",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "party_id", Label: "Party"},
					{Name: "address_type", Label: "Type"},
					{Name: "line1", Label: "Address Line 1"},
					{Name: "line2", Label: "Address Line 2"},
					{Name: "city", Label: "City"},
					{Name: "region", Label: "Region"},
					{Name: "postal_code", Label: "Postal Code"},
					{Name: "country_code", Label: "Country Code"},
					{Name: "is_primary", Label: "Primary"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func ContactMechanismForm() *form.Definition {
	return &form.Definition{
		EntityType: "ContactMechanism",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "party_id", Label: "Party"},
					{Name: "mechanism_type", Label: "Type"},
					{Name: "value", Label: "Value"},
					{Name: "is_primary", Label: "Primary"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func AttachmentForm() *form.Definition {
	return &form.Definition{
		EntityType: "Attachment",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "entity_type", Label: "Entity Type"},
					{Name: "record_id", Label: "Record ID"},
					{Name: "file_name", Label: "File Name"},
					{Name: "mime_type", Label: "MIME Type"},
					{Name: "size_bytes", Label: "Size (bytes)"},
					{Name: "storage_path", Label: "Storage Path"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func UnitOfMeasureForm() *form.Definition {
	return &form.Definition{
		EntityType: "UnitOfMeasure",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func UomConversionForm() *form.Definition {
	return &form.Definition{
		EntityType: "UomConversion",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "from_uom_id", Label: "From Unit"},
					{Name: "to_uom_id", Label: "To Unit"},
					{Name: "factor", Label: "Factor"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func CurrencyForm() *form.Definition {
	return &form.Definition{
		EntityType: "Currency",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "minor_unit", Label: "Minor Unit"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func ExchangeRateForm() *form.Definition {
	return &form.Definition{
		EntityType: "ExchangeRate",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "from_currency_id", Label: "From Currency"},
					{Name: "to_currency_id", Label: "To Currency"},
					{Name: "effective_date", Label: "Effective Date"},
					{Name: "rate", Label: "Rate"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func StatusTypeForm() *form.Definition {
	return &form.Definition{
		EntityType: "StatusType",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "entity_type", Label: "Entity Type"},
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func StatusForm() *form.Definition {
	return &form.Definition{
		EntityType: "Status",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "status_type_id", Label: "Status Type"},
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "sequence", Label: "Sequence"},
					{Name: "is_initial", Label: "Initial"},
					{Name: "is_terminal", Label: "Terminal"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func StatusTransitionForm() *form.Definition {
	return &form.Definition{
		EntityType: "StatusTransition",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "status_type_id", Label: "Status Type"},
					{Name: "from_status_id", Label: "From Status"},
					{Name: "to_status_id", Label: "To Status"},
					{Name: "requires_workflow", Label: "Requires Workflow"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}
