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
