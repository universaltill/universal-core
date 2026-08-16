package foundation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestAllFoundationDefinitionsAreValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Fatalf("%s: expected valid definition, got %v", def.EntityType, err)
		}
	}
}

// TestAllFoundationFormsAreValid confirms every AllForms() entry is a
// structurally valid Form Definition — no test caught this generically
// before the 2026-07-26 addition of the 12 previously-missing foundation
// forms (AllForms' own doc comment), since only PartyForm/IssueReportForm
// existed and neither had a dedicated validity test either.
func TestAllFoundationFormsAreValid(t *testing.T) {
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid form definition, got %v", f.EntityType, err)
		}
	}
}

// TestAllFoundationEntitiesHaveAForm is the direct regression test for
// the gap AllForms' own doc comment describes: accessibleModules
// (internal/api/dashboard.go) only surfaces an entity in the UI once
// BOTH its entity Definition AND its Form Definition are published, so
// an entity Definition with no matching form is invisible — reachable by
// no module-menu node, no /forms/{entityType}/new|{id} route. Every
// foundation entity except AIProviderConnection and ExternalSQLSource
// (their own doc comments: deliberately bespoke, a generic form would
// render their encrypted secrets as plain text boxes) and
// ExternalIdentity (a different reason — no secrets, but it is system
// bookkeeping written by the import engine; hand-editing identity rows
// silently breaks import idempotency, see AllForms' doc comment) must
// have a form.
func TestAllFoundationEntitiesHaveAForm(t *testing.T) {
	formTypes := map[string]bool{}
	for _, f := range AllForms() {
		formTypes[f.EntityType] = true
	}
	noForm := map[string]bool{
		"AIProviderConnection": true,
		"ExternalSQLSource":    true,
		"ExternalIdentity":     true,
	}
	for _, def := range All() {
		if noForm[def.EntityType] {
			continue
		}
		if !formTypes[def.EntityType] {
			t.Errorf("%s has an entity Definition but no Form Definition — it will be invisible in the UI (see AllForms' own doc comment)", def.EntityType)
		}
	}
}

// TestAllFoundationFormFieldsExistOnTheirEntity closes a real gap found
// by this feature's own independent review: form.Definition.Validate()
// only checks a form's own internal shape (a fields section has ≥1
// field, action ops are known) — it never cross-checks a FormField.Name
// against the target entity's actual declared fields. A typo'd or
// stale field name would pass Validate() and TestAllFoundationFormsAreValid
// cleanly, then only fail at real render time
// (formrender.Render's own "form field %q has no matching field on
// entity %q" error) the first time a human actually opens that form —
// exactly the kind of gap this whole change exists to close, so it
// needs its own guard, not just "the form is shaped correctly."
func TestAllFoundationFormFieldsExistOnTheirEntity(t *testing.T) {
	entityDefs := map[string]*entity.Definition{}
	for _, def := range All() {
		entityDefs[def.EntityType] = def
	}
	for _, f := range AllForms() {
		def, ok := entityDefs[f.EntityType]
		if !ok {
			t.Errorf("form %s targets an entity type not in All()", f.EntityType)
			continue
		}
		for _, section := range f.Sections {
			for _, ff := range section.Fields {
				if _, ok := def.FieldByName(ff.Name); !ok {
					t.Errorf("%s form section %q references field %q, which doesn't exist on the %s entity", f.EntityType, section.Title, ff.Name, f.EntityType)
				}
			}
		}
	}
}

// TestPartyRole_SamePartyCanHoldMultipleRoles is the whole point of the
// Party-Role pattern (ADR-0001 §8): a single Party record can hold
// customer AND vendor roles at once, instead of the classic ERP failure
// of duplicate master records (the same real-world company existing once
// per department because each department created its own).
func TestPartyRole_SamePartyCanHoldMultipleRoles(t *testing.T) {
	roleDef := PartyRole()
	partyID := "party-123"

	vendorRole := map[string]any{"party_id": partyID, "role_type": "vendor"}
	customerRole := map[string]any{"party_id": partyID, "role_type": "customer"}

	// Both roles validate against the SAME party id — nothing about the
	// schema forces a second Party record to exist for the second role.
	if err := entity.ValidateRecord(roleDef, vendorRole); err != nil {
		t.Fatalf("vendor role should validate: %v", err)
	}
	if err := entity.ValidateRecord(roleDef, customerRole); err != nil {
		t.Fatalf("customer role should validate: %v", err)
	}
}

func TestPartyRole_RejectsUnknownRoleType(t *testing.T) {
	roleDef := PartyRole()
	data := map[string]any{"party_id": "party-123", "role_type": "landlord"}
	if err := entity.ValidateRecord(roleDef, data); err == nil {
		t.Fatal("expected error for role_type not in the declared enum")
	}
}

// TestPartyRole_OwnOrganizationIsALegalRoleType is the regression test
// for uc-infra#63's v3 enum addition: a PartyRole record marking a Party
// as the tenant's own legal entity must validate, the same as any other
// role_type value.
func TestPartyRole_OwnOrganizationIsALegalRoleType(t *testing.T) {
	roleDef := PartyRole()
	data := map[string]any{"party_id": "party-123", "role_type": "own_organization"}
	if err := entity.ValidateRecord(roleDef, data); err != nil {
		t.Fatalf("own_organization role should validate: %v", err)
	}
}

// TestPartyRole_DeclaresConditionalUniqueOnOwnOrganization confirms
// uc-infra#201/ADR-0028's UniqueWhen declaration on PartyRole v4: at most
// one live PartyRole with role_type=="own_organization" — see
// TestCurrency_DeclaresConditionalUniqueOnIsBase for the identical shape
// on Currency.is_base, and crud/unique_constraints_test.go and
// cmd/sync-tenant-modules's own ConditionalUnique tests for the generic
// mechanism's end-to-end coverage (Create/Update/Delete/backfill).
func TestPartyRole_DeclaresConditionalUniqueOnOwnOrganization(t *testing.T) {
	def := PartyRole()
	if def.Version < 4 {
		t.Errorf("PartyRole is v%d, want >= v4 — the own_organization UniqueWhen constraint (uc-infra#201)", def.Version)
	}
	if len(def.UniqueWhen) != 1 {
		t.Fatalf("PartyRole.UniqueWhen = %v, want exactly one declared entry", def.UniqueWhen)
	}
	got := def.UniqueWhen[0]
	if len(got.Fields) != 1 || got.Fields[0] != "role_type" || got.WhenField != "role_type" || got.WhenValue != "own_organization" {
		t.Fatalf("PartyRole.UniqueWhen[0] = %+v, want Fields=[role_type] WhenField=role_type WhenValue=own_organization", got)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("PartyRole must validate as a Definition: %v", err)
	}
}

// TestParty_ExistingRecordsStillValidateAfterV3 confirms uc-infra#63's
// field additions are additive-only: a Party record written before v3
// existed (no registration_number/contact_first_name/contact_last_name
// keys at all, not even blank ones) still validates against the v3
// Definition, the same guarantee every other Version bump in this
// package documents ("rows written against vN still hold legal values").
func TestParty_ExistingRecordsStillValidateAfterV3(t *testing.T) {
	partyDef := Party()
	preV3Record := map[string]any{"party_type": "organization", "name": "Demo Organization"}
	if err := entity.ValidateRecord(partyDef, preV3Record); err != nil {
		t.Fatalf("pre-v3 Party record (no statutory fields) should still validate: %v", err)
	}
}

// TestParty_AcceptsStatutoryProfileFields is the happy path for the
// fields internal/api/saftexport.go's saftCompanyProfile reads off the
// Party holding the own_organization PartyRole (uc-infra#63).
func TestParty_AcceptsStatutoryProfileFields(t *testing.T) {
	partyDef := Party()
	record := map[string]any{
		"party_type":          "organization",
		"name":                "Demo Organization",
		"tax_id":              "TAX-98765",
		"registration_number": "REG-12345",
		"contact_first_name":  "Jane",
		"contact_last_name":   "Doe",
	}
	if err := entity.ValidateRecord(partyDef, record); err != nil {
		t.Fatalf("Party record with statutory profile fields should validate: %v", err)
	}
}

func TestCurrency_DefaultMinorUnit(t *testing.T) {
	def := Currency()
	f, ok := def.FieldByName("minor_unit")
	if !ok {
		t.Fatal("expected a minor_unit field")
	}
	if f.Default != float64(2) {
		t.Fatalf("expected default minor_unit of 2, got %v", f.Default)
	}
}

// TestCurrency_IsBaseFieldDefaultsFalse is uc-infra#120's Definition-level
// acceptance case: is_base exists, is a bool, and defaults false so
// existing v3 rows (which never set it) are still legal without a data
// migration.
func TestCurrency_IsBaseFieldDefaultsFalse(t *testing.T) {
	def := Currency()
	f, ok := def.FieldByName("is_base")
	if !ok {
		t.Fatal("expected an is_base field")
	}
	if f.Type != entity.FieldBool {
		t.Fatalf("expected is_base to be FieldBool, got %v", f.Type)
	}
	if f.Default != false {
		t.Fatalf("expected is_base to default false, got %v", f.Default)
	}

	// A record that never sets is_base at all (the pre-v4 shape) must
	// still validate — additive field, no data migration.
	record := map[string]any{"code": "USD", "name": "US Dollar"}
	if err := entity.ValidateRecord(def, record); err != nil {
		t.Fatalf("a Currency record without is_base set should still validate: %v", err)
	}

	explicit := map[string]any{"code": "QAR", "name": "Qatari Riyal", "is_base": true}
	if err := entity.ValidateRecord(def, explicit); err != nil {
		t.Fatalf("a Currency record with is_base=true should validate: %v", err)
	}
}

// TestCurrency_UniqueOnCode confirms uc-infra#181's Unique declaration:
// one live Currency per code — the Definition-level half of a duplicate-
// code rejection, which relies on crud.WriteUniqueConstraintKeys
// rejecting a concurrent second CREATE for the same code (see
// TestCurrency_DuplicateCodeRejected in seed_test.go for the real-
// Postgres end-to-end case).
func TestCurrency_UniqueOnCode(t *testing.T) {
	def := Currency()
	if def.Version < 5 {
		t.Errorf("Currency is v%d, want >= v5 — the code Unique constraint (uc-infra#181)", def.Version)
	}
	if len(def.Unique) != 1 {
		t.Fatalf("Currency.Unique = %v, want exactly one declared set", def.Unique)
	}
	want := []string{"code"}
	got := append([]string(nil), def.Unique[0]...)
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Currency.Unique[0] = %v, want %v", got, want)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("Currency must validate as a Definition: %v", err)
	}
}

// TestCurrency_DeclaresConditionalUniqueOnIsBase confirms uc-infra#201/
// ADR-0028's UniqueWhen declaration on Currency v6: at most one live
// Currency with is_base=true — see TestPartyRole_DeclaresConditionalUniqueOnOwnOrganization
// for the identical shape on PartyRole, and crud/unique_constraints_test.go
// for the generic mechanism's own end-to-end coverage.
func TestCurrency_DeclaresConditionalUniqueOnIsBase(t *testing.T) {
	def := Currency()
	if def.Version < 6 {
		t.Errorf("Currency is v%d, want >= v6 — the is_base UniqueWhen constraint (uc-infra#201)", def.Version)
	}
	if len(def.UniqueWhen) != 1 {
		t.Fatalf("Currency.UniqueWhen = %v, want exactly one declared entry", def.UniqueWhen)
	}
	got := def.UniqueWhen[0]
	if len(got.Fields) != 1 || got.Fields[0] != "is_base" || got.WhenField != "is_base" || got.WhenValue != "true" {
		t.Fatalf("Currency.UniqueWhen[0] = %+v, want Fields=[is_base] WhenField=is_base WhenValue=true", got)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("Currency must validate as a Definition: %v", err)
	}
}

// TestAddress_TypedAndMultiplePerParty is the point of Address being its
// own entity rather than fields on Party: the same party_id can carry a
// billing address and a shipping address as two independent records.
func TestAddress_TypedAndMultiplePerParty(t *testing.T) {
	def := Address()
	partyID := "party-123"

	billing := map[string]any{
		"party_id": partyID, "address_type": "billing",
		"line1": "1 Finance Way", "city": "Doha", "country_code": "QA",
	}
	shipping := map[string]any{
		"party_id": partyID, "address_type": "shipping",
		"line1": "2 Warehouse Rd", "city": "Manama", "country_code": "BH",
	}
	if err := entity.ValidateRecord(def, billing); err != nil {
		t.Fatalf("billing address should validate: %v", err)
	}
	if err := entity.ValidateRecord(def, shipping); err != nil {
		t.Fatalf("shipping address should validate: %v", err)
	}
}

func TestAddress_RejectsUnknownAddressType(t *testing.T) {
	def := Address()
	data := map[string]any{
		"party_id": "party-123", "address_type": "summer_home",
		"line1": "1 Finance Way", "city": "Doha", "country_code": "QA",
	}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for address_type not in the declared enum")
	}
}

func TestAddress_MissingRequiredLine1(t *testing.T) {
	def := Address()
	data := map[string]any{
		"party_id": "party-123", "address_type": "billing",
		"city": "Doha", "country_code": "QA",
	}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required line1")
	}
}

func TestAddress_IsPrimaryDefaultsFalse(t *testing.T) {
	def := Address()
	f, ok := def.FieldByName("is_primary")
	if !ok {
		t.Fatal("expected an is_primary field")
	}
	if f.Default != false {
		t.Fatalf("expected default is_primary of false, got %v", f.Default)
	}
}

// TestContactMechanism_TypedAndMultiplePerParty mirrors Address: one
// party_id can carry both a phone and an email as independent records,
// which fixed phone/email columns on Party couldn't represent (e.g. two
// phone numbers, or a fax-only vendor).
func TestContactMechanism_TypedAndMultiplePerParty(t *testing.T) {
	def := ContactMechanism()
	partyID := "party-123"

	phone := map[string]any{"party_id": partyID, "mechanism_type": "phone", "value": "+974-4444-1234"}
	email := map[string]any{"party_id": partyID, "mechanism_type": "email", "value": "ap@example.com"}
	if err := entity.ValidateRecord(def, phone); err != nil {
		t.Fatalf("phone contact should validate: %v", err)
	}
	if err := entity.ValidateRecord(def, email); err != nil {
		t.Fatalf("email contact should validate: %v", err)
	}
}

// TestContactMechanism_MobileAndFax rounds out the enum's other two
// values — TypedAndMultiplePerParty above only exercises phone/email.
func TestContactMechanism_MobileAndFax(t *testing.T) {
	def := ContactMechanism()
	mobile := map[string]any{"party_id": "party-123", "mechanism_type": "mobile", "value": "+974-5555-1234"}
	fax := map[string]any{"party_id": "party-123", "mechanism_type": "fax", "value": "+974-4444-9999"}
	if err := entity.ValidateRecord(def, mobile); err != nil {
		t.Fatalf("mobile contact should validate: %v", err)
	}
	if err := entity.ValidateRecord(def, fax); err != nil {
		t.Fatalf("fax contact should validate: %v", err)
	}
}

func TestContactMechanism_RejectsUnknownMechanismType(t *testing.T) {
	def := ContactMechanism()
	data := map[string]any{"party_id": "party-123", "mechanism_type": "carrier_pigeon", "value": "loft-7"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for mechanism_type not in the declared enum")
	}
}

// TestUomConversion_ReferencesBothUnits is the worked example from
// reference-data-model.md §0: 1 box = 12 each.
func TestUomConversion_ReferencesBothUnits(t *testing.T) {
	def := UomConversion()
	data := map[string]any{"from_uom_id": "uom-box", "to_uom_id": "uom-each", "factor": float64(12)}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("box->each conversion should validate: %v", err)
	}
}

func TestUomConversion_MissingFactor(t *testing.T) {
	def := UomConversion()
	data := map[string]any{"from_uom_id": "uom-box", "to_uom_id": "uom-each"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required factor")
	}
}

// TestExchangeRate_IsDateEffective checks ExchangeRate carries its own
// effective_date rather than assuming one rate per currency pair forever
// — the whole reason it's a separate entity from Currency.
func TestExchangeRate_IsDateEffective(t *testing.T) {
	def := ExchangeRate()
	f, ok := def.FieldByName("effective_date")
	if !ok {
		t.Fatal("expected an effective_date field")
	}
	if f.Type != entity.FieldDate {
		t.Fatalf("expected effective_date to be a date field, got %s", f.Type)
	}
	if !f.Required {
		t.Fatal("expected effective_date to be required — an exchange rate without a date isn't date-effective")
	}
}

// TestAttachment_UsableFromAnyEntityType is the point of Attachment being
// generic (reference-data-model.md §0: "usable from any entity"): the
// same Definition validates a file attached to a PurchaseOrder and one
// attached to a Vendor, because entity_type is data, not a fixed schema
// choice — a FieldReference with one Target couldn't do this (see
// Attachment's doc comment).
func TestAttachment_UsableFromAnyEntityType(t *testing.T) {
	def := Attachment()

	onPurchaseOrder := map[string]any{
		"entity_type": "PurchaseOrder", "record_id": "po-1",
		"file_name": "quote.pdf", "mime_type": "application/pdf",
		"size_bytes": float64(48213), "storage_path": "attachments/po-1/quote.pdf",
	}
	onVendor := map[string]any{
		"entity_type": "Vendor", "record_id": "vendor-9",
		"file_name": "w9.pdf", "mime_type": "application/pdf",
		"size_bytes": float64(12000), "storage_path": "attachments/vendor-9/w9.pdf",
	}
	if err := entity.ValidateRecord(def, onPurchaseOrder); err != nil {
		t.Fatalf("attachment on a PurchaseOrder should validate: %v", err)
	}
	if err := entity.ValidateRecord(def, onVendor); err != nil {
		t.Fatalf("attachment on a Vendor should validate: %v", err)
	}
}

func TestAttachment_MissingRequiredField(t *testing.T) {
	def := Attachment()
	data := map[string]any{
		"entity_type": "PurchaseOrder", "record_id": "po-1",
		"file_name": "quote.pdf", "mime_type": "application/pdf",
		// size_bytes and storage_path omitted
	}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required size_bytes/storage_path")
	}
}

// TestAttachment_HasNoFixedTargetField confirms entity_type/record_id are
// plain strings, not a FieldReference — a FieldReference always names one
// fixed Target (see entity.Field.Target), which would defeat the point of
// a generic, any-entity attachment.
func TestAttachment_HasNoFixedTargetField(t *testing.T) {
	def := Attachment()
	f, ok := def.FieldByName("entity_type")
	if !ok {
		t.Fatal("expected an entity_type field")
	}
	if f.Type != entity.FieldString {
		t.Fatalf("expected entity_type to be a plain string field, not %s (a fixed Target would defeat genericity)", f.Type)
	}
}

// TestStatus_ValidatesAgainstItsStatusType exercises the shape of the
// three new Definitions together: a Status record names the StatusType
// it belongs to, and a StatusTransition names the StatusType plus the two
// Status ids it connects — none of this is enforced by entity.ValidateRecord
// itself (that only checks field shape, not that the referenced ids
// actually belong to the same StatusType — see
// crud.Engine.ValidateStatusTransition for the enforcement that does).
func TestStatus_ValidatesAgainstItsStatusType(t *testing.T) {
	statusTypeDef, statusDef, transitionDef := StatusType(), Status(), StatusTransition()

	st := map[string]any{"entity_type": "PurchaseOrder", "code": "purchase_order_status", "name": "Purchase Order Status"}
	if err := entity.ValidateRecord(statusTypeDef, st); err != nil {
		t.Fatalf("status type should validate: %v", err)
	}

	draft := map[string]any{"status_type_id": "st-1", "code": "draft", "name": map[string]any{"en": "Draft"}, "is_initial": true}
	if err := entity.ValidateRecord(statusDef, draft); err != nil {
		t.Fatalf("draft status should validate: %v", err)
	}

	transition := map[string]any{"status_type_id": "st-1", "from_status_id": "status-draft", "to_status_id": "status-submitted"}
	if err := entity.ValidateRecord(transitionDef, transition); err != nil {
		t.Fatalf("transition should validate: %v", err)
	}
}

func TestStatus_IsInitialDefaultsFalse(t *testing.T) {
	def := Status()
	f, ok := def.FieldByName("is_initial")
	if !ok {
		t.Fatal("expected an is_initial field")
	}
	if f.Default != false {
		t.Fatalf("expected is_initial to default to false, got %v", f.Default)
	}
}

func TestStatusTransition_MissingToStatus(t *testing.T) {
	def := StatusTransition()
	data := map[string]any{"status_type_id": "st-1", "from_status_id": "status-draft"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required to_status_id")
	}
}

// TestStatusTransition_FromStatusMustShareStatusType and its to_status_id
// twin below (uc-infra#252) mirror
// projects.TestTask_ParentTaskMustShareProject's own shape-level proof
// for the same mechanism: from_status_id/to_status_id declare
// MustMatchParentField: "status_type_id" — crud.Engine (proven generically
// against throwaway Definitions by
// internal/kernel/crud/target_constraints_test.go) is what actually
// enforces it at write time; this only proves StatusTransition's own
// Definition wires the constraint on, closing the gap uc-infra#252
// itself reported: a transition could point at a Status belonging to an
// entirely different StatusType with no error, since nothing before this
// declared the constraint (StatusTransition's own doc comment never
// claimed otherwise — it only ever called out requires_workflow as
// unenforced).
func TestStatusTransition_FromStatusMustShareStatusType(t *testing.T) {
	f, ok := StatusTransition().FieldByName("from_status_id")
	if !ok {
		t.Fatal("expected a from_status_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Status" {
		t.Fatalf("expected from_status_id to be a FieldReference targeting Status, got type=%s target=%s", f.Type, f.Target)
	}
	if f.MustMatchParentField != "status_type_id" {
		t.Fatalf("expected must_match_parent_field %q, got %q", "status_type_id", f.MustMatchParentField)
	}
}

func TestStatusTransition_ToStatusMustShareStatusType(t *testing.T) {
	f, ok := StatusTransition().FieldByName("to_status_id")
	if !ok {
		t.Fatal("expected a to_status_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Status" {
		t.Fatalf("expected to_status_id to be a FieldReference targeting Status, got type=%s target=%s", f.Type, f.Target)
	}
	if f.MustMatchParentField != "status_type_id" {
		t.Fatalf("expected must_match_parent_field %q, got %q", "status_type_id", f.MustMatchParentField)
	}
}

// TestStatusTransition_RejectsCrossStatusTypeFromAndTo (uc-infra#252) is
// the real-Postgres end-to-end proof that the two shape tests above
// actually do something at write time: crud.Engine.Create must reject a
// StatusTransition whose from_status_id or to_status_id names a Status
// belonging to a DIFFERENT StatusType than the transition row's own
// status_type_id, and must still accept the legitimate same-StatusType
// case. The generic MustMatchParentField mechanism itself is already
// proven against throwaway Definitions
// (internal/kernel/crud/target_constraints_test.go); this exercises the
// real StatusTransition Definition specifically, the same "prove the real
// Definition wires the generic mechanism correctly" layer
// TestSystemOfRecord_DuplicateEntityTypeAndSourceRejected above
// establishes for a different constraint.
func TestStatusTransition_RejectsCrossStatusTypeFromAndTo(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	engine := crud.NewEngine(tenantDB)

	poType, err := engine.Create(ctx, StatusType(), map[string]any{
		"entity_type": "PurchaseOrder", "code": "purchase_order_status", "name": "Purchase Order Status",
	}, actor)
	if err != nil {
		t.Fatalf("create purchase_order_status StatusType: %v", err)
	}
	soType, err := engine.Create(ctx, StatusType(), map[string]any{
		"entity_type": "SalesOrder", "code": "sales_order_status", "name": "Sales Order Status",
	}, actor)
	if err != nil {
		t.Fatalf("create sales_order_status StatusType: %v", err)
	}

	poDraft, err := engine.Create(ctx, Status(), map[string]any{
		"status_type_id": poType.ID, "code": "draft", "name": map[string]any{"en": "Draft"}, "is_initial": true,
	}, actor)
	if err != nil {
		t.Fatalf("create purchase_order_status draft Status: %v", err)
	}
	poSubmitted, err := engine.Create(ctx, Status(), map[string]any{
		"status_type_id": poType.ID, "code": "submitted", "name": map[string]any{"en": "Submitted"},
	}, actor)
	if err != nil {
		t.Fatalf("create purchase_order_status submitted Status: %v", err)
	}
	soDraft, err := engine.Create(ctx, Status(), map[string]any{
		"status_type_id": soType.ID, "code": "draft", "name": map[string]any{"en": "Draft"}, "is_initial": true,
	}, actor)
	if err != nil {
		t.Fatalf("create sales_order_status draft Status: %v", err)
	}

	// Legitimate same-StatusType transition still succeeds.
	if _, err := engine.Create(ctx, StatusTransition(), map[string]any{
		"status_type_id": poType.ID, "from_status_id": poDraft.ID, "to_status_id": poSubmitted.ID,
	}, actor); err != nil {
		t.Fatalf("expected a same-StatusType transition to succeed: %v", err)
	}

	// from_status_id pointing at the WRONG StatusType is rejected.
	if _, err := engine.Create(ctx, StatusTransition(), map[string]any{
		"status_type_id": poType.ID, "from_status_id": soDraft.ID, "to_status_id": poSubmitted.ID,
	}, actor); !errors.Is(err, crud.ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation for a cross-StatusType from_status_id, got %v", err)
	}

	// to_status_id pointing at the WRONG StatusType is rejected.
	if _, err := engine.Create(ctx, StatusTransition(), map[string]any{
		"status_type_id": poType.ID, "from_status_id": poDraft.ID, "to_status_id": soDraft.ID,
	}, actor); !errors.Is(err, crud.ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation for a cross-StatusType to_status_id, got %v", err)
	}
}

func TestIssueReport_DefaultsToNewStatus(t *testing.T) {
	def := IssueReport()
	f, ok := def.FieldByName("status")
	if !ok {
		t.Fatal("expected a status field")
	}
	if f.Default != "new" {
		t.Fatalf("expected status to default to \"new\", got %v", f.Default)
	}
}

func TestIssueReport_MissingRequiredDescription(t *testing.T) {
	def := IssueReport()
	data := map[string]any{"title": "Something's broken"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required description")
	}
}

// TestIssueReport_TranscriptIsOptional confirms a typed-only report
// (no voice note recorded at all) is still a valid submission — voice
// capture is one input among several the issue logger offers, not a
// requirement to file a report at all. status is supplied explicitly
// (Required, and — same as every other Required field in this kernel,
// see entity.Definition.Validate's own discipline — Default is a
// form-rendering hint only, entity.ValidateRecord never auto-fills a
// missing Required field from it) the same way the real submit handler
// always will.
func TestIssueReport_TranscriptIsOptional(t *testing.T) {
	def := IssueReport()
	data := map[string]any{"title": "Something's broken", "description": "The button does nothing when clicked.", "status": "new"}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected a typed-only report (no transcript) to be valid, got %v", err)
	}
}

// TestIssueReport_ConsoleLogIsOptional is console_log's own version of
// TestIssueReport_TranscriptIsOptional immediately above: a browser that
// captured no console/error activity (or an old client predating this
// field, universaltill/uc-infra#46) must still be a valid submission —
// console_log is captured evidence, not a requirement to file a report.
func TestIssueReport_ConsoleLogIsOptional(t *testing.T) {
	def := IssueReport()
	data := map[string]any{"title": "Something's broken", "description": "The button does nothing when clicked.", "status": "new"}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected a report with no console_log to be valid, got %v", err)
	}
}

// TestIssueReport_ConsoleLogAccepted confirms a report that does carry a
// captured console_log validates cleanly, same shape as any other typed
// FieldString value on this entity.
func TestIssueReport_ConsoleLogAccepted(t *testing.T) {
	def := IssueReport()
	data := map[string]any{
		"title":       "Something's broken",
		"description": "The button does nothing when clicked.",
		"console_log": "[error] TypeError: cannot read properties of undefined",
		"status":      "new",
	}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected a report with console_log to be valid, got %v", err)
	}
}

func TestIssueReport_RejectsUnknownStatus(t *testing.T) {
	def := IssueReport()
	data := map[string]any{"title": "x", "description": "y", "status": "resolved-by-magic"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for a status value outside the declared enum")
	}
}

// TestIssueReport_DescriptionHasMaxLength (uc-infra#174): description had
// no length bound at all before this — a plain FieldString riding along
// in the same multipart request as a screen-recording upload inherited
// that upload's own much larger http.MaxBytesReader byte cap
// (issuereport.go's maxIssueReportSubmitBytes, 61 MiB) instead of any
// bound of its own. This is the regression test at the entity-Definition
// level (issue_report_test.go / e2e cover the HTTP-handler level).
func TestIssueReport_DescriptionHasMaxLength(t *testing.T) {
	def := IssueReport()
	f, ok := def.FieldByName("description")
	if !ok {
		t.Fatal("expected a description field")
	}
	if f.MaxLength == nil {
		t.Fatal("expected description to declare a MaxLength")
	}
	data := map[string]any{
		"title":       "Something's broken",
		"description": strings.Repeat("x", *f.MaxLength+1),
		"status":      "new",
	}
	err := entity.ValidateRecord(def, data)
	if err == nil {
		t.Fatalf("expected error for description exceeding its MaxLength (%d)", *f.MaxLength)
	}
	var verr *entity.ValidationError
	if !errors.As(err, &verr) || verr.Kind != entity.KindTooLong {
		t.Fatalf("expected a KindTooLong ValidationError, got %v", err)
	}
	// At the exact bound: still valid (inclusive), same as Min/Max.
	data["description"] = strings.Repeat("x", *f.MaxLength)
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("unexpected error at the inclusive MaxLength: %v", err)
	}
}

// TestIssueReport_ConsoleLogHasMaxLength is console_log's own version of
// TestIssueReport_DescriptionHasMaxLength immediately above — the other
// field the originating review found unbounded.
func TestIssueReport_ConsoleLogHasMaxLength(t *testing.T) {
	def := IssueReport()
	f, ok := def.FieldByName("console_log")
	if !ok {
		t.Fatal("expected a console_log field")
	}
	if f.MaxLength == nil {
		t.Fatal("expected console_log to declare a MaxLength")
	}
	data := map[string]any{
		"title":       "Something's broken",
		"description": "The button does nothing when clicked.",
		"console_log": strings.Repeat("x", *f.MaxLength+1),
		"status":      "new",
	}
	err := entity.ValidateRecord(def, data)
	if err == nil {
		t.Fatalf("expected error for console_log exceeding its MaxLength (%d)", *f.MaxLength)
	}
	var verr *entity.ValidationError
	if !errors.As(err, &verr) || verr.Kind != entity.KindTooLong {
		t.Fatalf("expected a KindTooLong ValidationError, got %v", err)
	}
}

// TestIssueReport_TitleHasMaxLength and TestIssueReport_TranscriptHasMaxLength
// (uc-infra#174, independent review) close the rest of the same gap the two
// tests above cover: the original fix only bounded description/console_log,
// but title and transcript ride in the exact same /issue-report/submit
// request, with the exact same entity.FieldString-had-no-length-concept
// hole, and were just as reachable by a caller posting directly to the
// endpoint (no browser, no maxlength attribute in the way).
func TestIssueReport_TitleHasMaxLength(t *testing.T) {
	def := IssueReport()
	f, ok := def.FieldByName("title")
	if !ok {
		t.Fatal("expected a title field")
	}
	if f.MaxLength == nil {
		t.Fatal("expected title to declare a MaxLength")
	}
	data := map[string]any{
		"title":       strings.Repeat("x", *f.MaxLength+1),
		"description": "Fine.",
		"status":      "new",
	}
	err := entity.ValidateRecord(def, data)
	if err == nil {
		t.Fatalf("expected error for title exceeding its MaxLength (%d)", *f.MaxLength)
	}
	var verr *entity.ValidationError
	if !errors.As(err, &verr) || verr.Kind != entity.KindTooLong {
		t.Fatalf("expected a KindTooLong ValidationError, got %v", err)
	}
}

func TestIssueReport_TranscriptHasMaxLength(t *testing.T) {
	def := IssueReport()
	f, ok := def.FieldByName("transcript")
	if !ok {
		t.Fatal("expected a transcript field")
	}
	if f.MaxLength == nil {
		t.Fatal("expected transcript to declare a MaxLength")
	}
	data := map[string]any{
		"title":       "Something's broken",
		"description": "Fine.",
		"transcript":  strings.Repeat("x", *f.MaxLength+1),
		"status":      "new",
	}
	err := entity.ValidateRecord(def, data)
	if err == nil {
		t.Fatalf("expected error for transcript exceeding its MaxLength (%d)", *f.MaxLength)
	}
	var verr *entity.ValidationError
	if !errors.As(err, &verr) || verr.Kind != entity.KindTooLong {
		t.Fatalf("expected a KindTooLong ValidationError, got %v", err)
	}
}

// TestIssueReport_TranscriptMaxLengthExceedsDescriptions pins the
// deliberate asymmetry foundation.IssueReport's own doc comment
// documents: transcript's bound must stay larger than description's, or
// the capture page's append-transcript-into-description flow would have
// nothing meaningful left to clamp against (a transcript legitimately
// longer than description's own cap must still be storable in full,
// verbatim, in its own field).
func TestIssueReport_TranscriptMaxLengthExceedsDescriptionMaxLength(t *testing.T) {
	def := IssueReport()
	desc, ok := def.FieldByName("description")
	if !ok || desc.MaxLength == nil {
		t.Fatal("expected description to declare a MaxLength")
	}
	transcript, ok := def.FieldByName("transcript")
	if !ok || transcript.MaxLength == nil {
		t.Fatal("expected transcript to declare a MaxLength")
	}
	if *transcript.MaxLength <= *desc.MaxLength {
		t.Fatalf("expected transcript's MaxLength (%d) to exceed description's (%d)", *transcript.MaxLength, *desc.MaxLength)
	}
}

func TestAIProviderConnection_RequiresProvider(t *testing.T) {
	def := AIProviderConnection()
	data := map[string]any{"model": "llama3.2:3b"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required provider")
	}
}

func TestAIProviderConnection_RejectsUnknownProvider(t *testing.T) {
	def := AIProviderConnection()
	data := map[string]any{"provider": "gemini"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for a provider value outside the declared enum (ollama/anthropic/openai)")
	}
}

// TestAIProviderConnection_APIKeyEncryptedIsNotRequiredAtTheEntityLevel
// confirms an Ollama-only connection (base_url, no api_key_encrypted at
// all) is a valid record on its own — the "Anthropic/OpenAI need a key,
// Ollama doesn't" rule is provider-conditional business logic that
// belongs in internal/api's settings handler, not something
// entity.Field can express (see AIProviderConnection's own doc comment
// on why api_key_encrypted is deliberately not Required here).
func TestAIProviderConnection_APIKeyEncryptedIsNotRequiredAtTheEntityLevel(t *testing.T) {
	def := AIProviderConnection()
	data := map[string]any{"provider": "ollama", "base_url": "http://localhost:11434", "model": "llama3.2:3b", "singleton_key": "singleton"}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected an Ollama connection with no api_key_encrypted to be valid at the entity level, got %v", err)
	}
}

// TestAIProviderConnection_SingletonKeyIsRequired confirms singleton_key
// (uc-infra#180) is enforced like any other Required field at the entity
// level — a record missing it (the settings handler always sets it, but
// entity.ValidateRecord has no way to know that) is invalid, the same
// belt-and-braces reasoning this field's own doc comment gives for
// declaring it Required at all.
func TestAIProviderConnection_SingletonKeyIsRequired(t *testing.T) {
	def := AIProviderConnection()
	data := map[string]any{"provider": "ollama", "base_url": "http://localhost:11434", "model": "llama3.2:3b"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required singleton_key")
	}
}

// TestAIProviderConnection_DeclaresUniqueOnSingletonKey confirms the
// Definition itself declares the Unique constraint the marker-field
// mechanism relies on (uc-infra#180) — a static, Version-controlled
// shape check independent of crud.Engine's own database-aware
// enforcement (covered separately, integration-level, in
// internal/kernel/crud).
func TestAIProviderConnection_DeclaresUniqueOnSingletonKey(t *testing.T) {
	def := AIProviderConnection()
	if len(def.Unique) != 1 || len(def.Unique[0]) != 1 || def.Unique[0][0] != "singleton_key" {
		t.Fatalf("expected Unique == [][]string{{\"singleton_key\"}}, got %v", def.Unique)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("expected AIProviderConnection() to be a valid Definition, got %v", err)
	}
}

func TestRole_RequiresCodeAndName(t *testing.T) {
	def := Role()
	if err := entity.ValidateRecord(def, map[string]any{"name": "Finance Manager"}); err == nil {
		t.Fatal("expected an error for a Role missing its required code")
	}
	if err := entity.ValidateRecord(def, map[string]any{"code": "finance_manager"}); err == nil {
		t.Fatal("expected an error for a Role missing its required name")
	}
	if err := entity.ValidateRecord(def, map[string]any{
		"code": "finance_manager", "name": "Finance Manager", "description": "Approves invoices over threshold",
	}); err != nil {
		t.Fatalf("expected a fully-populated Role to validate, got %v", err)
	}
}

// TestUserRole_SameUserCanHoldMultipleRoles mirrors
// TestPartyRole_SamePartyCanHoldMultipleRoles's own reasoning — the
// same real reason UserRole is many-to-many (ADR-0005: "a user can hold
// more than one Role").
func TestUserRole_SameUserCanHoldMultipleRoles(t *testing.T) {
	def := UserRole()
	userID := "zitadel-sub-123"

	if err := entity.ValidateRecord(def, map[string]any{"user_id": userID, "role_id": "role-finance"}); err != nil {
		t.Fatalf("first UserRole should validate: %v", err)
	}
	if err := entity.ValidateRecord(def, map[string]any{"user_id": userID, "role_id": "role-warehouse"}); err != nil {
		t.Fatalf("second UserRole for the same user should validate: %v", err)
	}
}

func TestUserRole_MissingRequiredUserID(t *testing.T) {
	def := UserRole()
	if err := entity.ValidateRecord(def, map[string]any{"role_id": "role-finance"}); err == nil {
		t.Fatal("expected an error for a UserRole missing its required user_id")
	}
}

func TestUserRole_MissingRequiredRoleID(t *testing.T) {
	def := UserRole()
	if err := entity.ValidateRecord(def, map[string]any{"user_id": "zitadel-sub-123"}); err == nil {
		t.Fatal("expected an error for a UserRole missing its required role_id")
	}
}

// TestUserRole_DepartmentIsOptional confirms a v2 UserRole with no
// department_id (a global grant, per its own doc comment) still
// validates — the same reversible-optional-field precedent as
// Position.department_id, not a required field a pre-v2 UserRole row
// would suddenly fail to satisfy.
func TestUserRole_DepartmentIsOptional(t *testing.T) {
	def := UserRole()
	if err := entity.ValidateRecord(def, map[string]any{
		"user_id": "zitadel-sub-123", "role_id": "role-finance",
	}); err != nil {
		t.Fatalf("expected a UserRole with no department_id (global grant) to validate, got %v", err)
	}
}

func TestUserRole_DepartmentScopedGrantValidates(t *testing.T) {
	def := UserRole()
	if err := entity.ValidateRecord(def, map[string]any{
		"user_id": "zitadel-sub-123", "role_id": "role-finance", "department_id": "dept-sales",
	}); err != nil {
		t.Fatalf("expected a department-scoped UserRole to validate, got %v", err)
	}
}

func TestDepartment_RequiresCodeAndName(t *testing.T) {
	def := Department()
	if err := entity.ValidateRecord(def, map[string]any{"name": "Finance"}); err == nil {
		t.Fatal("expected an error for a Department missing its required code")
	}
	if err := entity.ValidateRecord(def, map[string]any{"code": "fin"}); err == nil {
		t.Fatal("expected an error for a Department missing its required name")
	}
	if err := entity.ValidateRecord(def, map[string]any{"code": "fin", "name": "Finance"}); err != nil {
		t.Fatalf("expected a top-level Department (no parent) to validate, got %v", err)
	}
}

// TestDepartment_ParentIsOptional confirms a Department can sit at the
// top of the org chart with no parent_department_id — the field-level
// mirror of TestAccount_HierarchyResolvesEndToEnd's real-DB proof that a
// root node needs no parent value.
func TestDepartment_ParentIsOptional(t *testing.T) {
	def := Department()
	if err := entity.ValidateRecord(def, map[string]any{
		"code": "eng", "name": "Engineering", "parent_department_id": "dept-root",
	}); err != nil {
		t.Fatalf("expected a Department with a parent to validate, got %v", err)
	}
}

func TestPosition_RequiresTitle(t *testing.T) {
	def := Position()
	if err := entity.ValidateRecord(def, map[string]any{"department_id": "dept-1"}); err == nil {
		t.Fatal("expected an error for a Position missing its required title")
	}
	if err := entity.ValidateRecord(def, map[string]any{
		"title": "Finance Manager", "department_id": "dept-1",
	}); err != nil {
		t.Fatalf("expected a top-of-chart Position (no reports_to) to validate, got %v", err)
	}
}

// TestPosition_DepartmentIsOptional confirms a company-level/matrix
// Position (e.g. CFO, reporting to no single department) can be created
// with no department_id at all — the design point independent review
// caught: a required department_id would have forced a synthetic root
// Department onto every such Position.
func TestPosition_DepartmentIsOptional(t *testing.T) {
	def := Position()
	if err := entity.ValidateRecord(def, map[string]any{"title": "CFO"}); err != nil {
		t.Fatalf("expected a Position with no department_id to validate, got %v", err)
	}
}

func TestPosition_ReportsToIsOptional(t *testing.T) {
	def := Position()
	if err := entity.ValidateRecord(def, map[string]any{
		"title": "Accountant", "department_id": "dept-1", "reports_to_position_id": "pos-manager",
	}); err != nil {
		t.Fatalf("expected a Position with reports_to_position_id to validate, got %v", err)
	}
}

func TestExternalSQLSource_RequiresNameDriverHostDatabase(t *testing.T) {
	def := ExternalSQLSource()
	valid := map[string]any{
		"name": "Legacy NAV", "driver": "mssql",
		"host": "nav.internal", "database": "NAVDB",
	}
	if err := entity.ValidateRecord(def, valid); err != nil {
		t.Fatalf("expected a minimal source (name/driver/host/database) to be valid, got %v", err)
	}
	for _, missing := range []string{"name", "driver", "host", "database"} {
		data := map[string]any{}
		for k, v := range valid {
			if k != missing {
				data[k] = v
			}
		}
		if err := entity.ValidateRecord(def, data); err == nil {
			t.Errorf("expected an error for a source missing required %q", missing)
		}
	}
}

func TestExternalSQLSource_RejectsUnknownDriver(t *testing.T) {
	def := ExternalSQLSource()
	data := map[string]any{
		"name": "x", "driver": "oracle", "host": "h", "database": "d",
	}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for a driver outside the declared enum (mssql/postgres)")
	}
}

// TestExternalSQLSource_PasswordEncryptedIsNotRequiredAtTheEntityLevel —
// same reasoning as AIProviderConnection's api_key_encrypted: a source
// may legitimately have no password, and Required here would force the
// settings handler to round-trip the secret on every edit instead of
// treating blank-on-edit as "unchanged" (see the entity's doc comment).
func TestExternalSQLSource_PasswordEncryptedIsNotRequiredAtTheEntityLevel(t *testing.T) {
	def := ExternalSQLSource()
	data := map[string]any{
		"name": "Dev box", "driver": "postgres", "host": "localhost",
		"database": "navmirror", "username": "readonly",
	}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected a passwordless source to be valid at the entity level, got %v", err)
	}
}

// TestExternalIdentity_RequiresAllFields — every field on
// ExternalIdentity is Required by design: an identity row missing any of
// source/relation/entity-type/record/key answers no "have I imported
// this legacy record before?" question and would only poison future
// upsert lookups. source_relation is required like the rest — it is part
// of the identity scope, not decoration (NAV $Customer/$Vendor both land
// in Party with overlapping number series; see the entity's doc comment).
func TestExternalIdentity_RequiresAllFields(t *testing.T) {
	def := ExternalIdentity()
	valid := map[string]any{
		"source_id": "src-1", "source_relation": "dbo.A$Item", "entity_type": "Item",
		"record_id": "rec-1", "external_key": "1000",
	}
	if err := entity.ValidateRecord(def, valid); err != nil {
		t.Fatalf("expected a complete identity row to be valid, got %v", err)
	}
	for _, missing := range []string{"source_id", "source_relation", "entity_type", "record_id", "external_key"} {
		data := map[string]any{}
		for k, v := range valid {
			if k != missing {
				data[k] = v
			}
		}
		if err := entity.ValidateRecord(def, data); err == nil {
			t.Errorf("expected an error for an identity row missing required %q", missing)
		}
	}
}

// TestSystemOfRecord_RequiresEntityTypeAndMode — entity_type and mode are
// the two halves of an ownership declaration ("Items are read-only
// here"); a row missing either declares nothing.
func TestSystemOfRecord_RequiresEntityTypeAndMode(t *testing.T) {
	def := SystemOfRecord()
	valid := map[string]any{
		"entity_type": "Item", "source_id": "src-1", "mode": "read_only",
	}
	if err := entity.ValidateRecord(def, valid); err != nil {
		t.Fatalf("expected a complete SystemOfRecord row to be valid, got %v", err)
	}
	for _, missing := range []string{"entity_type", "mode"} {
		data := map[string]any{}
		for k, v := range valid {
			if k != missing {
				data[k] = v
			}
		}
		if err := entity.ValidateRecord(def, data); err == nil {
			t.Errorf("expected an error for a SystemOfRecord row missing required %q", missing)
		}
	}
}

// TestSystemOfRecord_SourceIsOptional — source_id is meaningless for
// platform_owned (no external party to point at), so it is not Required
// at the entity level; the entity's doc comment covers why the
// read_only-needs-a-source expectation can't be expressed here either.
func TestSystemOfRecord_SourceIsOptional(t *testing.T) {
	def := SystemOfRecord()
	if err := entity.ValidateRecord(def, map[string]any{
		"entity_type": "Item", "mode": "platform_owned",
	}); err != nil {
		t.Fatalf("expected a sourceless platform_owned row to be valid, got %v", err)
	}
}

// TestSystemOfRecord_ModeEnum — all three declared values validate,
// including the reserved "bidirectional": the ENTITY accepts it (removing
// it later would be a breaking enum change; reserving it is additive) and
// the guarded engine, not this layer, is what refuses to save it
// (authz.ErrSystemOfRecordModeReserved — tested in internal/kernel/authz).
func TestSystemOfRecord_ModeEnum(t *testing.T) {
	def := SystemOfRecord()
	for _, mode := range []string{"read_only", "bidirectional", "platform_owned"} {
		if err := entity.ValidateRecord(def, map[string]any{
			"entity_type": "Item", "mode": mode,
		}); err != nil {
			t.Errorf("expected declared mode %q to validate at the entity level, got %v", mode, err)
		}
	}
	if err := entity.ValidateRecord(def, map[string]any{
		"entity_type": "Item", "mode": "write_back_someday",
	}); err == nil {
		t.Error("expected an error for an unknown SystemOfRecord mode")
	}
}

// TestSystemOfRecord_UniqueOnEntityTypeAndSource confirms uc-infra#121's
// Unique declaration: one live SystemOfRecord row per (entity_type,
// source_id) pair — the Definition-level half of a duplicate-pair
// rejection (see TestSystemOfRecord_DuplicateEntityTypeAndSourceRejected
// for the real-Postgres end-to-end case). Deliberately NOT entity_type
// alone: an independent review of this same card caught that
// authz.checkSoRReadOnly is written to accept several rows per
// entity_type, one per external source (readOnlySources is a set) — a
// tenant mirroring Party from two different sources legitimately holds
// two SystemOfRecord rows naming "Party," and an entity_type-only
// constraint would have silently made that configuration impossible.
// The authz "most restrictive wins" resolution stays in place regardless
// — it is not just a pre-migration fallback, it is what this multi-source
// configuration actually relies on (see SystemOfRecord's own doc comment).
func TestSystemOfRecord_UniqueOnEntityTypeAndSource(t *testing.T) {
	def := SystemOfRecord()
	if def.Version != 2 {
		t.Errorf("SystemOfRecord is v%d, want v2 — the (entity_type, source_id) Unique constraint (uc-infra#121)", def.Version)
	}
	if len(def.Unique) != 1 {
		t.Fatalf("SystemOfRecord.Unique = %v, want exactly one declared set", def.Unique)
	}
	want := []string{"entity_type", "source_id"}
	got := append([]string(nil), def.Unique[0]...)
	if len(got) != len(want) {
		t.Fatalf("SystemOfRecord.Unique[0] = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f] = true
	}
	for _, f := range want {
		if !seen[f] {
			t.Errorf("SystemOfRecord.Unique[0] = %v, missing %q", got, f)
		}
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("SystemOfRecord must validate as a Definition: %v", err)
	}
}

// TestExternalIdentity_UniqueOnCompositeKey confirms uc-infra#121's Unique
// declaration on (source_id, source_relation, entity_type, external_key)
// — the Definition-level half (see
// TestExternalIdentity_DuplicateCompositeKeyRejected for the real-Postgres
// end-to-end case).
func TestExternalIdentity_UniqueOnCompositeKey(t *testing.T) {
	def := ExternalIdentity()
	if def.Version != 2 {
		t.Errorf("ExternalIdentity is v%d, want v2 — the composite Unique constraint (uc-infra#121)", def.Version)
	}
	if len(def.Unique) != 1 {
		t.Fatalf("ExternalIdentity.Unique = %v, want exactly one declared set", def.Unique)
	}
	want := []string{"source_id", "source_relation", "entity_type", "external_key"}
	got := append([]string(nil), def.Unique[0]...)
	if len(got) != len(want) {
		t.Fatalf("ExternalIdentity.Unique[0] = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f] = true
	}
	for _, f := range want {
		if !seen[f] {
			t.Errorf("ExternalIdentity.Unique[0] = %v, missing %q", got, f)
		}
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("ExternalIdentity must validate as a Definition: %v", err)
	}
}

// TestSystemOfRecord_DuplicateEntityTypeAndSourceRejected is the
// real-Postgres end-to-end case: a second SystemOfRecord row repeating
// the same (entity_type, source_id) pair must be rejected by crud.Engine
// itself (record_unique_keys' real Postgres UNIQUE index, ADR-0018
// §3(c)) — but a SECOND source declaring the SAME entity_type must still
// succeed, since that is the legitimate multi-source configuration
// authz.checkSoRReadOnly is written to support (see
// TestSystemOfRecord_UniqueOnEntityTypeAndSource's own doc comment).
func TestSystemOfRecord_DuplicateEntityTypeAndSourceRejected(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	engine := crud.NewEngine(tenantDB)
	def := SystemOfRecord()

	sourceA, err := engine.Create(ctx, ExternalSQLSource(), map[string]any{
		"name": "NAV mirror", "driver": "postgres", "host": "h", "database": "nav",
	}, actor)
	if err != nil {
		t.Fatalf("create ExternalSQLSource A: %v", err)
	}
	sourceB, err := engine.Create(ctx, ExternalSQLSource(), map[string]any{
		"name": "old CRM", "driver": "postgres", "host": "h", "database": "crm",
	}, actor)
	if err != nil {
		t.Fatalf("create ExternalSQLSource B: %v", err)
	}

	if _, err := engine.Create(ctx, def, map[string]any{
		"entity_type": "Item", "source_id": sourceA.ID, "mode": "read_only",
	}, actor); err != nil {
		t.Fatalf("create first SystemOfRecord: %v", err)
	}

	// Same entity_type, SAME source: a genuinely redundant/contradictory
	// re-declaration — rejected.
	_, err = engine.Create(ctx, def, map[string]any{
		"entity_type": "Item", "source_id": sourceA.ID, "mode": "platform_owned",
	}, actor)
	if err == nil {
		t.Fatal("expected a second SystemOfRecord row for the same (entity_type, source_id) to be rejected")
	}
	var uniqueErr *crud.UniqueConstraintError
	if !errors.As(err, &uniqueErr) {
		t.Fatalf("expected a *crud.UniqueConstraintError, got %T: %v", err, err)
	}
	if uniqueErr.EntityType != "SystemOfRecord" {
		t.Errorf("UniqueConstraintError.EntityType = %q, want %q", uniqueErr.EntityType, "SystemOfRecord")
	}

	// Same entity_type, a DIFFERENT source: the legitimate multi-source
	// configuration — must still succeed.
	if _, err := engine.Create(ctx, def, map[string]any{
		"entity_type": "Item", "source_id": sourceB.ID, "mode": "read_only",
	}, actor); err != nil {
		t.Fatalf("expected a second source's SystemOfRecord row for the same entity_type to succeed: %v", err)
	}

	// A DIFFERENT entity_type must also still succeed.
	if _, err := engine.Create(ctx, def, map[string]any{
		"entity_type": "Party", "source_id": sourceA.ID, "mode": "platform_owned",
	}, actor); err != nil {
		t.Fatalf("expected a SystemOfRecord row for a distinct entity_type to succeed: %v", err)
	}
}

// TestExternalIdentity_DuplicateCompositeKeyRejected is the real-Postgres
// end-to-end case: a second ExternalIdentity row repeating the same
// (source_id, source_relation, entity_type, external_key) must be
// rejected by crud.Engine itself, not merely surface later as a per-row
// "ambiguous identity" import error.
func TestExternalIdentity_DuplicateCompositeKeyRejected(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	engine := crud.NewEngine(tenantDB)

	source, err := engine.Create(ctx, ExternalSQLSource(), map[string]any{
		"name": "NAV mirror", "driver": "postgres",
		"host": "legacy.example.internal", "database": "navmirror",
	}, actor)
	if err != nil {
		t.Fatalf("create ExternalSQLSource: %v", err)
	}
	party1, err := engine.Create(ctx, Party(), map[string]any{
		"name": "Vendor One", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create first Party: %v", err)
	}
	party2, err := engine.Create(ctx, Party(), map[string]any{
		"name": "Vendor Two", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create second Party: %v", err)
	}

	def := ExternalIdentity()
	if _, err := engine.Create(ctx, def, map[string]any{
		"source_id": source.ID, "source_relation": "dbo.CRONUS$Vendor",
		"entity_type": "Party", "record_id": party1.ID, "external_key": "V-1000",
	}, actor); err != nil {
		t.Fatalf("create first ExternalIdentity: %v", err)
	}

	// Same source/relation/entity_type/external_key, different record_id
	// (record_id is not part of the Unique set — it's the identity's
	// payload, not its key) — must still collide.
	_, err = engine.Create(ctx, def, map[string]any{
		"source_id": source.ID, "source_relation": "dbo.CRONUS$Vendor",
		"entity_type": "Party", "record_id": party2.ID, "external_key": "V-1000",
	}, actor)
	if err == nil {
		t.Fatal("expected a second ExternalIdentity row with the same composite key to be rejected")
	}
	var uniqueErr *crud.UniqueConstraintError
	if !errors.As(err, &uniqueErr) {
		t.Fatalf("expected a *crud.UniqueConstraintError, got %T: %v", err, err)
	}
	if uniqueErr.EntityType != "ExternalIdentity" {
		t.Errorf("UniqueConstraintError.EntityType = %q, want %q", uniqueErr.EntityType, "ExternalIdentity")
	}

	// A different source_relation (uc-infra#101's $Customer/$Vendor
	// overlap scenario) with the SAME external_key must still succeed —
	// source_relation is part of the scope, not decoration.
	if _, err := engine.Create(ctx, def, map[string]any{
		"source_id": source.ID, "source_relation": "dbo.CRONUS$Customer",
		"entity_type": "Party", "record_id": party2.ID, "external_key": "V-1000",
	}, actor); err != nil {
		t.Fatalf("expected an ExternalIdentity row with a distinct source_relation to succeed: %v", err)
	}
}
