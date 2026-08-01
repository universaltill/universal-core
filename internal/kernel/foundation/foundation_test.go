package foundation

import (
	"testing"

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
// render their encrypted secrets as plain text boxes) must have a form.
func TestAllFoundationEntitiesHaveAForm(t *testing.T) {
	formTypes := map[string]bool{}
	for _, f := range AllForms() {
		formTypes[f.EntityType] = true
	}
	for _, def := range All() {
		if def.EntityType == "AIProviderConnection" || def.EntityType == "ExternalSQLSource" {
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

	draft := map[string]any{"status_type_id": "st-1", "code": "draft", "name": "Draft", "is_initial": true}
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

func TestIssueReport_RejectsUnknownStatus(t *testing.T) {
	def := IssueReport()
	data := map[string]any{"title": "x", "description": "y", "status": "resolved-by-magic"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for a status value outside the declared enum")
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
	data := map[string]any{"provider": "ollama", "base_url": "http://localhost:11434", "model": "llama3.2:3b"}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected an Ollama connection with no api_key_encrypted to be valid at the entity level, got %v", err)
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
