// Package form implements the Form Definition schema (ADR-0017 §6): a
// versioned, declarative layout — sections, fields, actions, navigation —
// rendered by a generic engine. Action verbs stay a small closed set
// (never a scripting language embedded in metadata); real logic belongs
// in a named workflow, referenced by an action, not written here.
package form

import "fmt"

// ActionOp is the closed set of verbs a button may invoke (ADR-0017 §6's
// guardrail). Adding a new op is a deliberate decision, not something a
// tenant or an AI draft can invent freely.
type ActionOp string

const (
	OpSave          ActionOp = "save"
	OpWorkflowStart ActionOp = "workflow.start"
	OpReportRender  ActionOp = "report.render"
	OpNavigate      ActionOp = "navigate"
)

// Component distinguishes a plain field group from an embedded
// master-detail grid or a read-only related list — these are visually
// similar but semantically distinct (ADR-0017 §6's three-way split).
type Component string

const (
	ComponentFields       Component = "fields"        // a group of Field entries
	ComponentMasterDetail Component = "master_detail" // composition relationship, atomic save + roll-up
	ComponentRelatedList  Component = "related_list"  // read-only, independent records
)

// FormField is one field's presentation on a form — distinct from
// entity.Field, which defines the field's data shape. A form can choose
// to omit, reorder, or add a visibility condition to an entity field
// without changing the entity itself.
type FormField struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	VisibleIf string `json:"visible_if,omitempty"`
}

// Section is one grouping/tab on a form.
type Section struct {
	Title     string      `json:"title"`
	Component Component   `json:"component"`
	Fields    []FormField `json:"fields,omitempty"`
	// Target is the child/related entity type, required when Component is
	// ComponentMasterDetail or ComponentRelatedList.
	Target string `json:"target,omitempty"`
	// RollUp names a numeric field on Target to sum into a header field
	// when Component is ComponentMasterDetail (e.g. line totals -> header
	// total). Empty means no roll-up for this section.
	RollUp       string `json:"roll_up,omitempty"`
	RollUpTarget string `json:"roll_up_target,omitempty"`
}

// Action is one button.
type Action struct {
	Label    string   `json:"label"`
	Op       ActionOp `json:"op"`
	Workflow string   `json:"workflow,omitempty"` // required when Op == OpWorkflowStart
	Report   string   `json:"report,omitempty"`   // required when Op == OpReportRender
}

// Definition is one version of a form's layout for an entity type.
type Definition struct {
	EntityType string    `json:"entity_type"`
	Version    int       `json:"version"`
	Sections   []Section `json:"sections"`
	Actions    []Action  `json:"actions,omitempty"`
}

// Validate checks internal consistency — what a human reviews before
// approving an AI-drafted form layout (ADR-0017 §14).
func (d *Definition) Validate() error {
	if d.EntityType == "" {
		return fmt.Errorf("entity_type is required")
	}
	for i, s := range d.Sections {
		switch s.Component {
		case ComponentFields:
			if len(s.Fields) == 0 {
				return fmt.Errorf("section %d (%q): fields component has no fields", i, s.Title)
			}
		case ComponentMasterDetail, ComponentRelatedList:
			if s.Target == "" {
				return fmt.Errorf("section %d (%q): %s component requires a target", i, s.Title, s.Component)
			}
		default:
			return fmt.Errorf("section %d (%q): unknown component %q", i, s.Title, s.Component)
		}
	}
	for i, a := range d.Actions {
		switch a.Op {
		case OpSave, OpNavigate:
			// no extra requirement
		case OpWorkflowStart:
			if a.Workflow == "" {
				return fmt.Errorf("action %d (%q): workflow.start requires a workflow name", i, a.Label)
			}
		case OpReportRender:
			if a.Report == "" {
				return fmt.Errorf("action %d (%q): report.render requires a report name", i, a.Label)
			}
		default:
			return fmt.Errorf("action %d (%q): unknown op %q — actions stay a closed declarative set", i, a.Label, a.Op)
		}
	}
	return nil
}
