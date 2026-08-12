package finance

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ResolveBaseCurrency returns the tenant's configured base/functional
// currency code (uc-infra#120): the ISO 4217 code of the
// foundation.Currency record(s) with is_base=true, if the tenant has
// exactly one DISTINCT such code. Falls back to DefaultGLCurrency when
// zero, or more than one DISTINCT code, claims is_base=true — degrading
// to the documented fallback rather than guessing which one is correct,
// the same fail-safe posture PartyRole.own_organization and
// AIProviderConnection's one-row-per-tenant conventions already
// establish for this kernel (see foundation.Currency's own doc comment).
// Distinctness is by CODE, not by row count: two is_base=true rows that
// both carry the same code (a plausible duplicate-entry state, since
// nothing makes Currency.code unique either) agree on the answer and
// are not the ambiguity this degrades for — the same reasoning
// saftCompanyProfile's own doc comment already makes for
// own_organization (multiple rows naming the same Party isn't a guess;
// multiple rows naming different Parties is).
//
// A candidate code that isn't a well-formed 3-letter ISO 4217 code
// (after trimming and uppercasing, matching saft.Build's own check) is
// treated as no code at all, not as a match — independent review found
// that nothing on the Currency Definition constrains code's shape
// (entity.Field has no length/pattern concept yet), and an
// unvalidated value reaching saft.Build's strict 3-letter check turned
// into a user-reachable 500 on the SAF-T export instead of a graceful
// fallback. Silently degrading to DefaultGLCurrency here keeps that
// failure mode impossible instead of documenting around it.
//
// A tenant that hasn't published the foundation module's Currency
// Definition at all is a real error here, not a silent DefaultGLCurrency
// — SyncGLAccounts already requires Currency to exist for its own
// currency_id resolution, and while the SAF-T export doesn't read
// Currency directly for anything else, both callers only ever run after
// foundation.Publish (which always seeds the Currency Definition), so
// surfacing this failure plainly is more honest than masking it behind
// the same-looking fallback value.
//
// Uses a raw, unguarded crud.Engine rather than an authz-guarded one
// deliberately: this is tenant-level configuration (which currency the
// whole tenant reports in, not a record whose visibility should depend
// on which actor is asking), the same posture h.tenantDisplayName
// already takes for the SAF-T export's CompanyName field, and
// SyncGLAccounts itself runs from cmd/ binaries with no HTTP
// actor/permission context to guard against in the first place.
func ResolveBaseCurrency(ctx context.Context, db *sql.DB) (string, error) {
	entityDefs := data.NewEntityDefinitionRepo(db)
	currencyDefRaw, err := entityDefs.GetPublished(ctx, "Currency")
	if err != nil {
		return "", fmt.Errorf("look up published Currency definition: %w", err)
	}
	currencyDef, err := entity.Unmarshal(currencyDefRaw.Definition)
	if err != nil {
		return "", fmt.Errorf("unmarshal Currency definition: %w", err)
	}

	engine := crud.NewEngine(db)
	currencies, err := engine.List(ctx, currencyDef)
	if err != nil {
		return "", fmt.Errorf("list Currency records: %w", err)
	}

	return pickBaseCurrencyCode(currencies), nil
}

// resolveBaseCurrencyTx is ResolveBaseCurrency against a caller-supplied
// transaction instead of db — the shape SyncGLAccountOnWrite (seed.go)
// needs, since a crud.Hook must do all of its reading and writing inside
// the write's own transaction (Hook's own doc comment). Shares
// pickBaseCurrencyCode with ResolveBaseCurrency deliberately: the two
// entry points must never disagree on which currency counts as "the"
// base one just because they took different repo methods to get there.
// Same "tenant hasn't published Currency at all is a real error" posture
// as ResolveBaseCurrency — see that function's own doc comment.
func resolveBaseCurrencyTx(ctx context.Context, tx *sql.Tx) (string, error) {
	entityDefs := data.NewEntityDefinitionRepo(nil)
	if _, err := entityDefs.GetPublishedTx(ctx, tx, "Currency"); err != nil {
		return "", fmt.Errorf("look up published Currency definition: %w", err)
	}
	currencies, err := data.NewRecordRepo(nil).ListTx(ctx, tx, "Currency")
	if err != nil {
		return "", fmt.Errorf("list Currency records: %w", err)
	}
	return pickBaseCurrencyCode(currencies), nil
}

// pickBaseCurrencyCode is ResolveBaseCurrency's/resolveBaseCurrencyTx's
// shared, pure selection rule (uc-infra#120) — see ResolveBaseCurrency's
// own doc comment for the full reasoning (distinctness by CODE not row
// count, malformed-code handling, the DefaultGLCurrency degrade). Pure
// so it needs no database at all to test the actual decision rule.
func pickBaseCurrencyCode(currencies []data.Record) string {
	distinctCodes := map[string]bool{}
	for _, c := range currencies {
		isBase, _ := c.Data["is_base"].(bool)
		if !isBase {
			continue
		}
		raw, _ := c.Data["code"].(string)
		code := strings.ToUpper(strings.TrimSpace(raw))
		if len(code) != 3 {
			continue // not a well-formed ISO 4217 code — not a valid candidate
		}
		distinctCodes[code] = true
	}
	if len(distinctCodes) != 1 {
		return DefaultGLCurrency
	}
	for code := range distinctCodes {
		return code
	}
	return DefaultGLCurrency // unreachable: the len check above guarantees exactly one entry
}
