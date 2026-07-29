package foundation

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
)

// RoleCodesForUser resolves which Role.code values userID (the Zitadel
// OIDC "sub" — the same identifier this kernel already uses as
// audit.Actor.ID, see UserRole's own doc comment and ADR-0005) currently
// holds in this tenant. Read-only.
//
// The UserRole lookup uses RecordRepo.ListByField (WHERE data->>'user_id'
// = $1, pushed into SQL) — not a full-tenant scan filtered in Go —
// because, unlike ledger.checkPeriodOpen/purchasing.
// receivedValueForPurchaseOrder (which run inside an existing *sql.Tx
// with no ListByFieldTx equivalent), this runs on a plain *sql.DB with
// no such constraint, and it's meant to become the building block a
// future per-request permission check calls (ADR-0005) — scanning every
// UserRole grant in the tenant on every call would be a real, needless
// scalability cost once that happens. Found by independent review: an
// earlier version of this function did exactly that full scan, on a
// stale "no field-level query exists yet" premise that ListByField's
// own existence (used elsewhere in this same codebase, e.g.
// cmd/seed-demo-data's seedVendorInvoices) already contradicted.
//
// The second Role lookup stays a full List + Go-side filter — the
// resolved role_id set from the first pass is already small (however
// many roles this one user holds), so pushing a second filter into SQL
// wouldn't meaningfully help.
//
// Deliberately not wired into any permission check yet (ADR-0005's own
// scoping note, uc-infra/docs/adr/0005-role-permission-model-core-
// owned.md) — this is the building block a future enforcement mechanism
// calls, not the mechanism itself. Returns nil, not an error, for a user
// with zero role grants — a real, valid state (they're a tenant member
// via Zitadel but haven't been assigned any Core-side Role yet), not a
// failure.
func RoleCodesForUser(ctx context.Context, db *sql.DB, userID string) ([]string, error) {
	records := data.NewRecordRepo(db)

	userRoles, err := records.ListByField(ctx, "UserRole", "user_id", userID)
	if err != nil {
		return nil, fmt.Errorf("list UserRole by user_id: %w", err)
	}
	roleIDs := make(map[string]bool)
	for _, ur := range userRoles {
		if rid, _ := ur.Data["role_id"].(string); rid != "" {
			roleIDs[rid] = true
		}
	}
	if len(roleIDs) == 0 {
		return nil, nil
	}

	roles, err := records.List(ctx, "Role")
	if err != nil {
		return nil, fmt.Errorf("list Role: %w", err)
	}
	var codes []string
	for _, r := range roles {
		if !roleIDs[r.ID] {
			continue
		}
		if code, _ := r.Data["code"].(string); code != "" {
			codes = append(codes, code)
		}
	}
	return codes, nil
}
