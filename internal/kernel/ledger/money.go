package ledger

import "math"

// ToMinorUnits converts a currency amount stored as this codebase's plain
// FieldNumber float (e.g. 10.5 meaning $10.50 — CLAUDE.md's own
// "money via a money.Money-equivalent integer-minor-units type" API
// convention isn't actually implemented as a real type anywhere in this
// kernel yet) into the integer minor units Line.DebitMinor/CreditMinor
// require.
//
// Known, documented simplification: always assumes 2 decimal places
// (matches foundation.Currency's own Default: float64(2) minor_unit),
// not per-currency-aware. A real 0-decimal (JPY-style) or 3-decimal
// (KWD/BHD-style) currency would round wrong here. Revisit once a
// currency-aware money type actually exists — tracked in
// erp/BACKLOG-TASKS.md alongside finance.DefaultGLCurrency, the same
// category of known gap.
func ToMinorUnits(amount float64) int64 {
	return int64(math.Round(amount * 100))
}
