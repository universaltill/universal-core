package ledger

import "math"

// ToMinorUnits converts a currency amount stored as this codebase's plain
// FieldNumber float (e.g. 10.5 meaning $10.50) into the integer minor
// units Line.DebitMinor/CreditMinor require. This is the transitional
// helper for call sites still handed a FieldNumber amount — CLAUDE.md's
// "money via a money.Money-equivalent integer-minor-units type" API
// convention now has a real implementation, internal/kernel/money's
// Money type and entity.FieldMoney (ADR-0021, uc-infra#68); a field
// migrated to FieldMoney carries its value as minor units already and
// never needs this conversion at all.
//
// Known, documented simplification: always assumes 2 decimal places
// (matches foundation.Currency's own Default: float64(2) minor_unit,
// and money.Decimals), not per-currency-aware. A real 0-decimal
// (JPY-style) or 3-decimal (KWD/BHD-style) currency would round wrong
// here. Revisit once a currency-aware money type actually exists — same
// tracked gap ADR-0021's own "Alternatives rejected" section notes
// (uc-infra#163; not the same gap as finance.DefaultGLCurrency/
// ResolveBaseCurrency, uc-infra#120, which is about which currency is
// the tenant's base, not decimal-place correctness).
func ToMinorUnits(amount float64) int64 {
	return int64(math.Round(amount * 100))
}
