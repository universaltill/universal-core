package finance

import (
	"context"
	"database/sql"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestResolveBaseCurrency_NoIsBaseSet_FallsBackToDefault covers the
// zero-candidates case: a tenant that has never set is_base on any
// Currency gets the hardcoded DefaultGLCurrency, same as before uc-infra#120.
func TestResolveBaseCurrency_NoIsBaseSet_FallsBackToDefault(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	got, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}
	if got != DefaultGLCurrency {
		t.Fatalf("expected fallback %q with no is_base set, got %q", DefaultGLCurrency, got)
	}
}

// TestResolveBaseCurrency_OneIsBaseSet_ReturnsItsCode is the core
// acceptance case: exactly one Currency marked is_base=true resolves to
// that currency's own code, not the hardcoded fallback.
func TestResolveBaseCurrency_OneIsBaseSet_ReturnsItsCode(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	currencyDef := publishedCurrencyDef(t, tenantDB)
	engine := crud.NewEngine(tenantDB)

	if _, err := engine.Create(ctx, currencyDef, map[string]any{
		"code": "USD", "name": "US Dollar", "is_base": false,
	}, actor); err != nil {
		t.Fatalf("create USD Currency: %v", err)
	}
	if _, err := engine.Create(ctx, currencyDef, map[string]any{
		"code": "GBP", "name": "British Pound", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create GBP Currency: %v", err)
	}

	got, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}
	if got != "GBP" {
		t.Fatalf("expected the is_base=true currency's code %q, got %q", "GBP", got)
	}
}

// TestResolveBaseCurrency_MultipleIsBaseSet_FallsBackToDefault covers the
// ambiguous case — more than one Currency claiming is_base=true (the
// generic entity/crud layer has no unique-constraint concept, uc-infra#121)
// degrades to the documented fallback rather than guessing which one is
// correct, same fail-safe posture as PartyRole.own_organization.
func TestResolveBaseCurrency_MultipleIsBaseSet_FallsBackToDefault(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	currencyDef := publishedCurrencyDef(t, tenantDB)
	engine := crud.NewEngine(tenantDB)

	if _, err := engine.Create(ctx, currencyDef, map[string]any{
		"code": "GBP", "name": "British Pound", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create GBP Currency: %v", err)
	}
	if _, err := engine.Create(ctx, currencyDef, map[string]any{
		"code": "EUR", "name": "Euro", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create EUR Currency: %v", err)
	}

	got, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}
	if got != DefaultGLCurrency {
		t.Fatalf("expected fallback %q with two is_base=true currencies, got %q", DefaultGLCurrency, got)
	}
}

// TestResolveBaseCurrency_MalformedCode_FallsBackToDefault is the
// regression test for a real bug independent review found: nothing on
// the Currency Definition constrains code's shape (entity.Field has no
// length/pattern concept), so an is_base=true row with a non-ISO-4217
// code used to reach saft.Build's strict "exactly 3 letters" check
// unvalidated — turning a tenant's own bad data entry into a 500 on
// their SAF-T export instead of the graceful degradation this function
// exists to provide everywhere else. A malformed code must be treated
// as no candidate at all, same as an empty one.
func TestResolveBaseCurrency_MalformedCode_FallsBackToDefault(t *testing.T) {
	for _, badCode := range []string{"US Dollar", "QA", "TOOLONG"} {
		t.Run(badCode, func(t *testing.T) {
			tenantDB := freshTenantDB(t)
			ctx := context.Background()
			actor := humanActor()

			if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
				t.Fatalf("foundation.Publish: %v", err)
			}
			currencyDef := publishedCurrencyDef(t, tenantDB)
			engine := crud.NewEngine(tenantDB)

			// Exactly ONE is_base=true row, with a malformed code — this
			// is the case that actually exercised the bug: a single bad
			// code used to be treated as an unambiguous match (matches
			// == 1) and returned verbatim, not caught by the
			// "more than one candidate" fallback branch at all.
			if _, err := engine.Create(ctx, currencyDef, map[string]any{
				"code": badCode, "name": "Bad Currency", "is_base": true,
			}, actor); err != nil {
				t.Fatalf("create Currency with code %q: %v", badCode, err)
			}

			got, err := ResolveBaseCurrency(ctx, tenantDB)
			if err != nil {
				t.Fatalf("ResolveBaseCurrency: %v", err)
			}
			if got != DefaultGLCurrency {
				t.Fatalf("expected fallback %q for malformed is_base code %q, got %q (would have broken saft.Build's 3-letter check)", DefaultGLCurrency, badCode, got)
			}
		})
	}
}

// TestResolveBaseCurrency_WhitespaceOnlyCode_FallsBackToDefault is the
// same regression as the malformed-code test above, for a code that is
// whitespace-only rather than shape-wrong.
//
// uc-infra#105 made entity.ValidateRecord's Required check reject a
// whitespace-only string the same as "", so engine.Create can no longer
// create a NEW Currency with a whitespace-only code — that write-path
// guard is entity.TestValidateRecord's job now, not this function's
// (same split #86 established for the plain "" case — see
// internal/api/saftexport_test.go's identical reasoning). But the guard
// is write-path only: it does not touch rows written before it landed,
// and this function's own TrimSpace (basecurrency.go) exists precisely
// so an already-stored whitespace-only code still degrades gracefully
// through ResolveBaseCurrency into saft.Build's strict 3-letter check
// instead of 500ing a tenant's SAF-T export — the exact bug this whole
// test function guards. Seeded directly through the repository (no
// validation layer), because that is how such a row can still exist.
func TestResolveBaseCurrency_WhitespaceOnlyCode_FallsBackToDefault(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishedCurrencyDef(t, tenantDB)

	if _, err := data.NewRecordRepo(tenantDB).Create(ctx, "Currency", map[string]any{
		"code": "  ", "name": "Bad Currency", "is_base": true,
	}); err != nil {
		t.Fatalf("seed a legacy whitespace-only-code Currency directly: %v", err)
	}

	got, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}
	if got != DefaultGLCurrency {
		t.Fatalf("expected fallback %q for whitespace-only is_base code, got %q (would have broken saft.Build's 3-letter check)", DefaultGLCurrency, got)
	}
}

// TestResolveBaseCurrency_DuplicateRowsSameCode_ResolvesNotFallback is
// the regression test for independent review's "DISTINCT" finding: two
// is_base=true rows that agree on the same code are not the ambiguity
// this function degrades for (same reasoning saftCompanyProfile's own
// doc comment already makes for own_organization) — counting ROWS
// instead of distinct CODES used to fall back to DefaultGLCurrency here
// even though both rows named the same, unambiguous answer.
func TestResolveBaseCurrency_DuplicateRowsSameCode_ResolvesNotFallback(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	currencyDef := publishedCurrencyDef(t, tenantDB)
	engine := crud.NewEngine(tenantDB)

	for i := 0; i < 2; i++ {
		if _, err := engine.Create(ctx, currencyDef, map[string]any{
			"code": "GBP", "name": "British Pound", "is_base": true,
		}, actor); err != nil {
			t.Fatalf("create duplicate GBP Currency %d: %v", i, err)
		}
	}

	got, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}
	if got != "GBP" {
		t.Fatalf("expected %q (two rows agreeing on the same code is not ambiguous), got %q", "GBP", got)
	}
}

// TestResolveBaseCurrency_NormalizesCaseAndWhitespace confirms a code
// entered with stray whitespace or lowercase letters — legal today since
// entity.Field has no shape constraint — still resolves to the clean,
// uppercased ISO 4217 form saft.Build requires, rather than passing the
// raw value through and failing that check downstream.
func TestResolveBaseCurrency_NormalizesCaseAndWhitespace(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	currencyDef := publishedCurrencyDef(t, tenantDB)
	engine := crud.NewEngine(tenantDB)

	if _, err := engine.Create(ctx, currencyDef, map[string]any{
		"code": " gbp ", "name": "British Pound", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create Currency: %v", err)
	}

	got, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}
	if got != "GBP" {
		t.Fatalf("expected the normalized code %q, got %q", "GBP", got)
	}
}

// TestResolveBaseCurrency_UnpublishedCurrency_ReturnsError confirms a
// tenant that hasn't published the foundation module's Currency
// Definition gets a real error, not a silently-masked DefaultGLCurrency —
// both SyncGLAccounts and the SAF-T export already require Currency to
// exist for other reasons, so this failure should surface plainly.
func TestResolveBaseCurrency_UnpublishedCurrency_ReturnsError(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if _, err := ResolveBaseCurrency(ctx, tenantDB); err == nil {
		t.Fatal("expected an error when Currency has never been published, got nil")
	}
}

// TestPickBaseCurrencyCode_TableDriven exercises pickBaseCurrencyCode's
// selection rule directly, with no database at all — the pure core both
// ResolveBaseCurrency (above) and resolveBaseCurrencyTx (used by
// SyncGLAccountOnWrite, hooks_test.go) share, so the two entry points
// can never drift on the answer. The *sql.DB-backed tests above already
// cover each of these cases end to end through ResolveBaseCurrency;
// this is the fast, dependency-free version of the same rule.
func TestPickBaseCurrencyCode_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		recs []data.Record
		want string
	}{
		{"no records", nil, DefaultGLCurrency},
		{"none is_base", []data.Record{
			{Data: map[string]any{"code": "USD", "is_base": false}},
		}, DefaultGLCurrency},
		{"one is_base", []data.Record{
			{Data: map[string]any{"code": "GBP", "is_base": true}},
		}, "GBP"},
		{"two distinct is_base codes", []data.Record{
			{Data: map[string]any{"code": "GBP", "is_base": true}},
			{Data: map[string]any{"code": "EUR", "is_base": true}},
		}, DefaultGLCurrency},
		{"two rows, same is_base code", []data.Record{
			{Data: map[string]any{"code": "GBP", "is_base": true}},
			{Data: map[string]any{"code": "GBP", "is_base": true}},
		}, "GBP"},
		{"malformed code", []data.Record{
			{Data: map[string]any{"code": "TOOLONG", "is_base": true}},
		}, DefaultGLCurrency},
		{"whitespace/case normalized", []data.Record{
			{Data: map[string]any{"code": " gbp ", "is_base": true}},
		}, "GBP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickBaseCurrencyCode(c.recs); got != c.want {
				t.Fatalf("pickBaseCurrencyCode(%v) = %q, want %q", c.recs, got, c.want)
			}
		})
	}
}

// TestResolveBaseCurrencyTx_MatchesResolveBaseCurrency confirms the
// tx-scoped path SyncGLAccountOnWrite uses agrees with the *sql.DB path
// ResolveBaseCurrency uses against the same data — the actual risk of
// having two entry points into the same selection rule.
func TestResolveBaseCurrencyTx_MatchesResolveBaseCurrency(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	currencyDef := publishedCurrencyDef(t, tenantDB)
	engine := crud.NewEngine(tenantDB)
	if _, err := engine.Create(ctx, currencyDef, map[string]any{
		"code": "GBP", "name": "British Pound", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create GBP Currency: %v", err)
	}

	wantCode, err := ResolveBaseCurrency(ctx, tenantDB)
	if err != nil {
		t.Fatalf("ResolveBaseCurrency: %v", err)
	}

	tx, err := tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()
	gotCode, err := resolveBaseCurrencyTx(ctx, tx)
	if err != nil {
		t.Fatalf("resolveBaseCurrencyTx: %v", err)
	}
	if gotCode != wantCode {
		t.Fatalf("resolveBaseCurrencyTx = %q, ResolveBaseCurrency = %q — the two must agree", gotCode, wantCode)
	}
	if gotCode != "GBP" {
		t.Fatalf("expected %q, got %q", "GBP", gotCode)
	}
}

// TestResolveBaseCurrencyTx_UnpublishedCurrency_ReturnsError mirrors
// TestResolveBaseCurrency_UnpublishedCurrency_ReturnsError for the tx
// path — the case SyncGLAccountOnWrite's own
// TestSyncGLAccountOnWrite_CurrencyNotPublished_RollsBackAccountCreate
// (hooks_test.go) exercises end to end through the hook.
func TestResolveBaseCurrencyTx_UnpublishedCurrency_ReturnsError(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	tx, err := tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := resolveBaseCurrencyTx(ctx, tx); err == nil {
		t.Fatal("expected an error when Currency has never been published, got nil")
	}
}

// publishedCurrencyDef looks up the published Currency Definition — the
// same lookup+unmarshal ResolveBaseCurrency and SyncGLAccounts's own
// tests already perform, factored out for these tests' own use.
func publishedCurrencyDef(t *testing.T, tenantDB *sql.DB) *entity.Definition {
	t.Helper()
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	raw, err := entityDefs.GetPublished(context.Background(), "Currency")
	if err != nil {
		t.Fatalf("GetPublished(Currency): %v", err)
	}
	def, err := entity.Unmarshal(raw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Currency: %v", err)
	}
	return def
}
