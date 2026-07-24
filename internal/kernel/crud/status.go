package crud

import (
	"context"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ErrInvalidTransition is returned by ValidateStatusTransition when a
// status_id value would either put a new record in a non-initial status,
// or move an existing record across an edge its StatusType's
// StatusTransition rows don't declare (foundation.go).
var ErrInvalidTransition = errors.New("crud: invalid status transition")

// ValidateStatusTransition enforces def's StatusType/Status/
// StatusTransition graph against a create or update's incoming fields —
// a no-op for any def with no StatusTypeCode (most entities, still) or
// any call that doesn't touch status_id (an update to other fields on a
// status-managed entity shouldn't have to re-assert its current status).
//
// Deliberately a separate method callers invoke alongside Create/Update
// rather than folded into them: unlike the Required-field/enum shape
// checks entity.ValidateRecord always applies, this needs the record's
// *current* status_id to check against on an update, which Create has no
// use for (isCreate=true skips that read — id is ignored in that case,
// since Create generates the record's id itself) and every Update would
// otherwise pay an extra read for even when def has no managed lifecycle
// at all — the common case today (see foundation.go's StatusType doc
// comment: no existing entity has opted in yet). internal/api's create/
// updateRecord call this explicitly, the same shape as extractVersion
// already being a step distinct from Update itself.
func (e *Engine) ValidateStatusTransition(ctx context.Context, def *entity.Definition, tenantID, id string, fields map[string]any, isCreate bool) error {
	if def.StatusTypeCode == "" {
		return nil
	}
	newStatusID, touched := fields["status_id"].(string)
	if !touched || newStatusID == "" {
		if isCreate {
			return fmt.Errorf("%s requires status_id to create (status_type_code %q)", def.EntityType, def.StatusTypeCode)
		}
		return nil
	}

	statusTypeID, err := e.statusTypeIDByCode(ctx, tenantID, def.StatusTypeCode)
	if err != nil {
		return err
	}
	newStatus, err := e.records.Get(ctx, tenantID, "Status", newStatusID)
	if err != nil {
		if errors.Is(err, data.ErrNotFound) {
			return fmt.Errorf("%w: status_id %q does not exist", ErrInvalidTransition, newStatusID)
		}
		return fmt.Errorf("look up status %s: %w", newStatusID, err)
	}
	if got, _ := newStatus.Data["status_type_id"].(string); got != statusTypeID {
		return fmt.Errorf("%w: status_id %q does not belong to status type %q", ErrInvalidTransition, newStatusID, def.StatusTypeCode)
	}

	var currentStatusID string
	if !isCreate {
		rec, err := e.records.Get(ctx, tenantID, def.EntityType, id)
		if err != nil {
			return fmt.Errorf("look up current %s %s: %w", def.EntityType, id, err)
		}
		currentStatusID, _ = rec.Data["status_id"].(string)
	}

	if currentStatusID == "" {
		// No prior status (a genuine create, or an update on a record
		// that predates this entity opting into StatusTypeCode) — the
		// only legal destination is a Status flagged is_initial.
		isInitial, _ := newStatus.Data["is_initial"].(bool)
		if !isInitial {
			return fmt.Errorf("%w: %s must start in an initial status, %q is not", ErrInvalidTransition, def.EntityType, newStatusID)
		}
		return nil
	}
	if currentStatusID == newStatusID {
		return nil // setting to the same status is always a no-op, not a transition
	}

	transitions, err := e.records.ListByField(ctx, tenantID, "StatusTransition", "status_type_id", statusTypeID)
	if err != nil {
		return fmt.Errorf("list transitions for status type %s: %w", def.StatusTypeCode, err)
	}
	for _, t := range transitions {
		from, _ := t.Data["from_status_id"].(string)
		to, _ := t.Data["to_status_id"].(string)
		if from == currentStatusID && to == newStatusID {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s is not a declared transition for %s", ErrInvalidTransition, currentStatusID, newStatusID, def.StatusTypeCode)
}

// statusTypeIDByCode resolves the entity.Definition.StatusTypeCode a
// caller declared to the actual StatusType record's id — code is the
// human-authored, stable identifier; the record id is what Status/
// StatusTransition rows actually reference (foundation.go's StatusType
// doc comment).
func (e *Engine) statusTypeIDByCode(ctx context.Context, tenantID, code string) (string, error) {
	matches, err := e.records.ListByField(ctx, tenantID, "StatusType", "code", code)
	if err != nil {
		return "", fmt.Errorf("look up status type %q: %w", code, err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: status type %q is not published for this tenant", ErrInvalidTransition, code)
	}
	return matches[0].ID, nil
}
