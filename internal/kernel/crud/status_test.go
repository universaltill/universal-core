package crud

import (
	"context"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// These fixtures deliberately don't import internal/kernel/foundation —
// same reasoning vendorDef() in crud_test.go doesn't import a real
// module: crud's own tests exercise the generic mechanism against
// minimal ad hoc Definitions, not a specific module's real ones.

func statusTypeDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "StatusType",
		Version:    1,
		Fields: []entity.Field{
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

func statusDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Status",
		Version:    1,
		Fields: []entity.Field{
			{Name: "status_type_id", Type: entity.FieldReference, Required: true, Target: "StatusType"},
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "is_initial", Type: entity.FieldBool, Default: false},
		},
	}
}

func statusTransitionDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "StatusTransition",
		Version:    1,
		Fields: []entity.Field{
			{Name: "status_type_id", Type: entity.FieldReference, Required: true, Target: "StatusType"},
			{Name: "from_status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "to_status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// purchaseOrderDef is a stand-in for a real status-managed business
// entity — StatusTypeCode set, status_id a Status reference, matching
// what entity.Definition.Validate requires of any Definition that opts
// into the pattern.
func purchaseOrderDef() *entity.Definition {
	return &entity.Definition{
		EntityType:     "PurchaseOrder",
		Version:        1,
		StatusTypeCode: "purchase_order_status",
		Fields: []entity.Field{
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// statusFixture seeds one StatusType ("purchase_order_status") with two
// statuses (draft, flagged is_initial, and submitted, not) and a single
// declared transition draft -> submitted — the minimal graph the tests
// below exercise every branch of ValidateStatusTransition against.
type statusFixture struct {
	draftID     string
	submittedID string
}

func seedStatusFixture(t *testing.T, ctx context.Context, engine *Engine, tenantID string) statusFixture {
	t.Helper()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	st, err := engine.Create(ctx, statusTypeDef(), tenantID, map[string]any{
		"entity_type": "PurchaseOrder", "code": "purchase_order_status", "name": "Purchase Order Status",
	}, actor)
	if err != nil {
		t.Fatalf("seed status type: %v", err)
	}

	draft, err := engine.Create(ctx, statusDef(), tenantID, map[string]any{
		"status_type_id": st.ID, "code": "draft", "name": "Draft", "is_initial": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed draft status: %v", err)
	}

	submitted, err := engine.Create(ctx, statusDef(), tenantID, map[string]any{
		"status_type_id": st.ID, "code": "submitted", "name": "Submitted", "is_initial": false,
	}, actor)
	if err != nil {
		t.Fatalf("seed submitted status: %v", err)
	}

	if _, err := engine.Create(ctx, statusTransitionDef(), tenantID, map[string]any{
		"status_type_id": st.ID, "from_status_id": draft.ID, "to_status_id": submitted.ID,
	}, actor); err != nil {
		t.Fatalf("seed draft->submitted transition: %v", err)
	}

	return statusFixture{draftID: draft.ID, submittedID: submitted.ID}
}

func TestValidateStatusTransition_NoOpWithoutStatusTypeCode(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)

	err := engine.ValidateStatusTransition(ctx, vendorDef(), tenantID, "", map[string]any{"name": "Acme"}, true)
	if err != nil {
		t.Fatalf("expected no-op for a Definition with no StatusTypeCode, got %v", err)
	}
}

func TestValidateStatusTransition_CreateRequiresStatusID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	seedStatusFixture(t, ctx, engine, tenantID)

	err := engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, "", map[string]any{}, true)
	if err == nil {
		t.Fatal("expected an error creating a status-managed entity with no status_id")
	}
}

func TestValidateStatusTransition_CreateRejectsNonInitialStatus(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	fx := seedStatusFixture(t, ctx, engine, tenantID)

	err := engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, "",
		map[string]any{"status_id": fx.submittedID}, true)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition creating in a non-initial status, got %v", err)
	}
}

func TestValidateStatusTransition_CreateAllowsInitialStatus(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	fx := seedStatusFixture(t, ctx, engine, tenantID)

	err := engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, "",
		map[string]any{"status_id": fx.draftID}, true)
	if err != nil {
		t.Fatalf("expected creating in the initial status to be allowed, got %v", err)
	}
}

func TestValidateStatusTransition_UpdateNotTouchingStatusIsNoOp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	fx := seedStatusFixture(t, ctx, engine, tenantID)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	po, err := engine.Create(ctx, purchaseOrderDef(), tenantID, map[string]any{"status_id": fx.draftID}, actor)
	if err != nil {
		t.Fatalf("seed purchase order: %v", err)
	}

	err = engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, po.ID, map[string]any{}, false)
	if err != nil {
		t.Fatalf("expected an update not touching status_id to be a no-op, got %v", err)
	}
}

func TestValidateStatusTransition_UpdateToSameStatusIsNoOp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	fx := seedStatusFixture(t, ctx, engine, tenantID)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	po, err := engine.Create(ctx, purchaseOrderDef(), tenantID, map[string]any{"status_id": fx.draftID}, actor)
	if err != nil {
		t.Fatalf("seed purchase order: %v", err)
	}

	err = engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, po.ID,
		map[string]any{"status_id": fx.draftID}, false)
	if err != nil {
		t.Fatalf("expected setting the same status to be a no-op, got %v", err)
	}
}

func TestValidateStatusTransition_UpdateAllowsDeclaredTransition(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	fx := seedStatusFixture(t, ctx, engine, tenantID)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	po, err := engine.Create(ctx, purchaseOrderDef(), tenantID, map[string]any{"status_id": fx.draftID}, actor)
	if err != nil {
		t.Fatalf("seed purchase order: %v", err)
	}

	err = engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, po.ID,
		map[string]any{"status_id": fx.submittedID}, false)
	if err != nil {
		t.Fatalf("expected the declared draft->submitted transition to be allowed, got %v", err)
	}
}

func TestValidateStatusTransition_UpdateRejectsUndeclaredTransition(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	fx := seedStatusFixture(t, ctx, engine, tenantID)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// Starts submitted, with no submitted -> draft transition declared.
	po, err := engine.Create(ctx, purchaseOrderDef(), tenantID, map[string]any{"status_id": fx.submittedID}, actor)
	if err != nil {
		t.Fatalf("seed purchase order: %v", err)
	}

	err = engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, po.ID,
		map[string]any{"status_id": fx.draftID}, false)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for an undeclared submitted->draft move, got %v", err)
	}
}

func TestValidateStatusTransition_RejectsStatusFromAnotherStatusType(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	seedStatusFixture(t, ctx, engine, tenantID)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A second, unrelated StatusType/Status pair for a different entity.
	otherType, err := engine.Create(ctx, statusTypeDef(), tenantID, map[string]any{
		"entity_type": "Party", "code": "party_status", "name": "Party Status",
	}, actor)
	if err != nil {
		t.Fatalf("seed other status type: %v", err)
	}
	otherStatus, err := engine.Create(ctx, statusDef(), tenantID, map[string]any{
		"status_type_id": otherType.ID, "code": "active", "name": "Active", "is_initial": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed other status: %v", err)
	}

	err = engine.ValidateStatusTransition(ctx, purchaseOrderDef(), tenantID, "",
		map[string]any{"status_id": otherStatus.ID}, true)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for a status_id from a different StatusType, got %v", err)
	}
}
