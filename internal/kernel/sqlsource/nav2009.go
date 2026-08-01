// The NAV 2009 mapping template — pure data, per ADR-0019's "everything
// vendor-specific is data" rule. Dynamics NAV 2009 exposes each
// company's data as SQL Server tables named "<Company>$<Table>" (the
// company name with dots/quotes flattened to underscores, then a `$`,
// then the object name — "CRONUS International Ltd_$Item"), which is why
// template matching is a suffix match on "$<Table>" rather than an exact
// relation name: the company half varies per installation.
//
// The column names below are NAV 2009 base-application standard (the
// shipped W1 objects), which is what "supporting NAV 2009" honestly
// means ahead of a real engagement: a customer's Z-fields/customized
// objects are exactly the discovery problem BACKLOG.md R7 owns, not
// something a static template should pretend to cover.
//
// Two limitations these templates surfaced deliberately, both recorded
// as R4 evidence in ADR-0019 and the card's close-out:
//   - Required enum fields (Item.item_type, Party.party_type) have no
//     NAV column carrying their values — hence Constants, not a column
//     mapping.
//   - NAV's record keys ("No_") mostly have no target field on the
//     canonical entities (Party has no external-code field), so the
//     legacy key originally wasn't preserved at all. That half is now
//     closed by uc-infra#101's cross-system external-ID mechanism:
//     KeyColumn names No_ and CommitRowsUpserting (upsert.go) stores it
//     per record as foundation.ExternalIdentity.external_key — off the
//     record itself, so the canonical entities still carry no legacy
//     field.
package sqlsource

import (
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/csvimport"
)

// TemplateEntity maps one legacy relation shape onto one target entity
// type.
type TemplateEntity struct {
	// RelationSuffix matches a discovered relation by name suffix,
	// case-sensitively — "$Item" matches any NAV company's Item table.
	RelationSuffix string
	EntityType     string
	Mapping        csvimport.ColumnMapping // source column -> target field
	Constants      map[string]string       // target field -> fixed raw value
	// KeyColumn names the source column carrying the legacy system's own
	// record key — what CommitRowsUpserting (upsert.go) stores as each
	// imported record's ExternalIdentity.external_key so a re-import
	// updates instead of duplicating. Empty means the template declares
	// no key and the source can only be imported create-only.
	KeyColumn string
}

// Template is one vendor's set of relation mappings.
type Template struct {
	Key      string // stable identifier, e.g. "nav2009"
	Vendor   string // human-readable, e.g. "Microsoft Dynamics NAV 2009"
	Entities []TemplateEntity
}

// Templates returns the built-in vendor templates. When the plugin
// runtime (#57) exists these become plugin-shippable data; the built-in
// set stays the fallback.
func Templates() []Template {
	return []Template{nav2009Template()}
}

func nav2009Template() Template {
	return Template{
		Key:    "nav2009",
		Vendor: "Microsoft Dynamics NAV 2009",
		Entities: []TemplateEntity{
			{
				RelationSuffix: "$Item",
				EntityType:     "Item",
				// No_ is NAV's primary key on every master table — the
				// natural external_key for upserting re-imports.
				KeyColumn: "No_",
				Mapping: csvimport.ColumnMapping{
					"No_":         "sku",
					"Description": "name",
				},
				Constants: map[string]string{
					// NAV's Item.Type is an integer option (0=Inventory,
					// 1=Service...) — no value transformation exists yet
					// (R4 evidence), so the template pins the common case
					// and a human adjusts service items after import.
					"item_type": "stock",
				},
			},
			{
				RelationSuffix: "$Customer",
				EntityType:     "Party",
				KeyColumn:      "No_", // NAV's primary No_ field, same as $Item
				Mapping: csvimport.ColumnMapping{
					"Name":                 "name",
					"VAT Registration No_": "tax_id",
				},
				Constants: map[string]string{
					"party_type": "organization",
				},
			},
			{
				RelationSuffix: "$Vendor",
				EntityType:     "Party",
				KeyColumn:      "No_", // NAV's primary No_ field, same as $Item
				Mapping: csvimport.ColumnMapping{
					"Name":                 "name",
					"VAT Registration No_": "tax_id",
				},
				Constants: map[string]string{
					"party_type": "organization",
				},
			},
		},
	}
}

// MatchTemplate finds the first template entity whose RelationSuffix
// matches relationName, across all built-in templates. ok is false when
// nothing matches — the caller falls back to plain
// csvimport.SuggestMapping over the relation's real columns.
func MatchTemplate(relationName string) (Template, TemplateEntity, bool) {
	for _, t := range Templates() {
		for _, te := range t.Entities {
			if strings.HasSuffix(relationName, te.RelationSuffix) {
				return t, te, true
			}
		}
	}
	return Template{}, TemplateEntity{}, false
}
