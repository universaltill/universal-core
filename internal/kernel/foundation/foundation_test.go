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
