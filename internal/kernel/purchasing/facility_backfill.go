package purchasing

import (
	"context"
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// GetOrCreateFacility resolves a Facility by code, creating it if this
// tenant has none — the shared get-or-create every facility-backfill
// command needs (cmd/backfill-inventory-facility, uc-infra#54's
// cmd/backfill-goods-receipt-facility). Exported and generalized here on
// the second real example, the same discipline
// internal/kernel/recordmigrate itself was generalized under
// (cmd/backfill-purchase-order-status's own doc comment: "if a second
// entity ever needs the same kind of fix, generalize then, against two
// real examples instead of one imagined one"). Deliberately NOT part of
// recordmigrate itself — that package's own doc comment says it never
// names an entity type, and this helper is Facility-specific by design
// (the "code" field, the warehouse default facility_type), so it lives
// here in the module that owns Facility instead.
//
// A dry run must not write, so when the facility does not exist yet it
// returns a synthetic all-zero id purely so the caller's preview
// validates rows against a facility-shaped value. entity.ValidateRecord
// treats a present, non-nil string as satisfying Required regardless of
// content, so this placeholder is not load-bearing for validation — it
// exists only so a dry-run log line can say which facility a row would
// point at, and it is never written: callers must not invoke this with
// dryRun and then persist the returned id.
func GetOrCreateFacility(
	ctx context.Context,
	engine *crud.Engine,
	def *entity.Definition,
	code, name string,
	dryRun bool,
	actor audit.Actor,
) (id string, created bool, err error) {
	existing, err := engine.ListByField(ctx, def, "code", code)
	if err != nil {
		return "", false, fmt.Errorf("list Facility by code: %w", err)
	}
	if len(existing) > 0 {
		return existing[0].ID, false, nil
	}
	if dryRun {
		return "00000000-0000-0000-0000-000000000000", true, nil
	}
	rec, err := engine.Create(ctx, def, map[string]any{
		"code": code, "name": name, "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		return "", false, fmt.Errorf("create Facility: %w", err)
	}
	return rec.ID, true, nil
}
