package formrender

import (
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/money"
)

// computeRollUp sums a numeric field across a master_detail section's
// child records — the roll-up form.Section already declares (RollUp /
// RollUpTarget) but that nothing evaluates until the renderer.
//
// isMoney selects the summation strategy: when the RollUpTarget field
// (and, by construction, the child RollUp field it's summed from) is
// entity.FieldMoney, each child's value is parsed via money.FromAny and
// accumulated as an exact money.Money (int64 minor units) — never a
// float64, for the identical reason money.Money's own doc comment gives
// for why summing prices must never touch a float in the hot path
// (uc-infra#136, following up uc-infra#68's own footer-total fix in
// internal/api/rfq_report.go). Before this, every roll-up sum was plain
// float64 += regardless of field type — harmless for a genuinely
// fractional FieldNumber (PurchaseOrderForm's own "Lines" section was
// the first Definition to declare a FieldMoney RollUpTarget, so the gap
// was latent, not yet reachable — see this package's own money_test.go
// history), but the wrong tool for a field the entity Definition itself
// declares must never carry a fractional minor-unit amount.
//
// Returns the total as a plain float64 either way, matching every
// existing caller's expectation (it round-trips back into a record's
// map[string]any field, the same shape entity.ValidateRecord and
// money.FromAny already accept) — a money.Money total's int64 value
// converts to float64 exactly for any realistic amount (float64
// represents every integer up to 2^53 exactly), so nothing is lost
// converting back at this boundary.
//
// The isMoney branch SKIPS a child whose value doesn't parse as money
// (independent review of uc-infra#136's first pass) rather than erroring
// the whole render, the opposite of the plain-FieldNumber branch below.
// A non-numeric FieldNumber value is genuine data corruption — rare
// enough that failing loud (internal/api/handlers.go's own render path
// is documented to only ever fail on "a schema-drift/malformed-
// expression bug in the Definitions themselves … never on attacker-
// controlled record data") is the right call. A FRACTIONAL FieldMoney
// value is not corruption — it's the ORDINARY, EXPECTED shape of a
// legacy row written before a FieldNumber->FieldMoney Version bump,
// still waiting on its own cmd/backfill-* command (the same "one bad
// row's problem, not the whole page's" resolution internal/data/
// reporting.go's moneyMinorUnitsPattern guard and rfq_reporting.go's own
// precedent already apply to exactly this shape of legacy data). Erroring
// here would turn GET /forms/PurchaseOrder/{id} into a permanent 500 for
// any tenant with an un-backfilled POLine — including one
// cmd/backfill-poline-money's own all-or-nothing row policy can't
// convert without the dangerous -include-whole-numbers flag — with no
// way to view or fix the record through the one screen that could.
func computeRollUp(children []map[string]any, field string, isMoney bool) (float64, error) {
	if isMoney {
		var total money.Money
		for _, child := range children {
			v, ok := child[field]
			if !ok {
				continue
			}
			m, err := money.FromAny(v)
			if err != nil {
				// Skipped, not fatal — see this function's own doc
				// comment. The child ROW itself is still rendered (every
				// OTHER cell in it), avoiding a permanent 500 on the
				// record's only editable screen — but this specific cell
				// does NOT stay visible: childCellValue's own FieldMoney
				// case (render.go) returns nil, not a raw-value fallback,
				// for the identical money.FromAny failure, so the
				// un-coercible amount renders blank (independent review
				// of uc-infra#166; this comment previously claimed
				// otherwise).
				continue
			}
			total += m
		}
		return float64(total), nil
	}
	var total float64
	for i, child := range children {
		v, ok := child[field]
		if !ok {
			continue
		}
		n, ok := v.(float64)
		if !ok {
			return 0, fmt.Errorf("roll_up field %q on child %d is not numeric (got %T)", field, i, v)
		}
		total += n
	}
	return total, nil
}
