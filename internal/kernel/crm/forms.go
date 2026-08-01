package crm

import "github.com/universaltill/universal-core/internal/kernel/form"

// CaseForm separates the complaint from its context: what the problem
// is, then which customer/product/order it concerns, then how urgently
// it must be answered.
func CaseForm() *form.Definition {
	return &form.Definition{
		EntityType: "Case",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Case",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "case_number", Label: "Case Number"},
					{Name: "subject", Label: "Subject"},
					{Name: "description", Label: "Description"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Context",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "customer_id", Label: "Customer"},
					{Name: "item_id", Label: "Product"},
					{Name: "sales_order_id", Label: "Sales Order"},
				},
			},
			{
				Title:     "Handling",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "priority", Label: "Priority"},
					{Name: "assignee_id", Label: "Assignee"},
					{Name: "opened_date", Label: "Opened"},
					{Name: "sla_due_at", Label: "SLA Due"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// CampaignForm puts the campaign itself first, then what it costs, then
// its related leads — a read-only related list, because a lead belongs
// to the pipeline rather than to the campaign that happened to source
// it (see Campaign's Relationship comment).
func CampaignForm() *form.Definition {
	return &form.Definition{
		EntityType: "Campaign",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Campaign",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "name", Label: "Name"},
					{Name: "channel", Label: "Channel"},
					{Name: "description", Label: "Description"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Run",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "start_date", Label: "Starts"},
					{Name: "end_date", Label: "Ends"},
					{Name: "budget", Label: "Budget"},
					{Name: "currency_id", Label: "Currency"},
				},
			},
			{
				Title:     "Leads",
				Component: form.ComponentRelatedList,
				Target:    "Lead",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// LeadForm separates who the lead is from where they came from, because
// those are filled in by different people at different times: the
// contact details arrive with the enquiry, the attribution is set by
// whoever triages it.
func LeadForm() *form.Definition {
	return &form.Definition{
		EntityType: "Lead",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Lead",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "name", Label: "Name"},
					{Name: "company_name", Label: "Company"},
					{Name: "email", Label: "Email"},
					{Name: "phone", Label: "Phone"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Attribution",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "source", Label: "Source"},
					{Name: "campaign_id", Label: "Campaign"},
					{Name: "owner_id", Label: "Owner"},
				},
			},
			{
				Title:     "Outcome",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "converted_party_id", Label: "Converted To"},
					{Name: "notes", Label: "Notes"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// OpportunityForm leads with the deal and its stage, then the numbers a
// pipeline report is built from, then provenance.
func OpportunityForm() *form.Definition {
	return &form.Definition{
		EntityType: "Opportunity",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Opportunity",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "name", Label: "Name"},
					{Name: "customer_id", Label: "Customer"},
					{Name: "status_id", Label: "Stage"},
					{Name: "description", Label: "Description"},
				},
			},
			{
				Title:     "Forecast",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "amount", Label: "Amount"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "probability", Label: "Probability %"},
					{Name: "expected_close_date", Label: "Expected Close"},
				},
			},
			{
				Title:     "Ownership",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "owner_id", Label: "Owner"},
					{Name: "lead_id", Label: "Originating Lead"},
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
		CaseForm(),
		CampaignForm(),
		LeadForm(),
		OpportunityForm(),
	}
}
