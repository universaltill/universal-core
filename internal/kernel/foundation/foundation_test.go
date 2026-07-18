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
