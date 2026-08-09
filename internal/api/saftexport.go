// The SAF-T Financial export endpoint (universaltill/uc-infra#28) — the
// first statutory-format export, produced by internal/kernel/saft (see
// that package's doc comment for the schema choice and the plugin-first
// placement note). Two routes: GET /export/saft streams the XML file for
// a from/to date range, GET /export/saft/form is the small page the
// Finance module menu links to (date pickers + download button).
//
// Unlike the generic CSV export (export.go), which reads one entity type
// through the guarded engine and inherits its redaction, this export
// reads the ledger's own typed tables (gl_accounts, journal_entries) —
// tables no entity Definition governs — plus Party/PartyRole/TaxCode
// records through the guarded engine. Access is therefore gated
// explicitly up front on read permission for every entity type whose
// data the file discloses (saftEntityTypes), the same whole-report gate
// reporting.go's requireReportRead applies: the file is one unit, so a
// missing read grant refuses the whole export, not a redacted section.
// A field the file discloses that is individually hidden (FieldPermission,
// not just entity-level read) gets the same refusal (saftFieldReads) —
// the guarded engine would otherwise silently strip it and the
// serializer would render a schema-valid file with a false empty value
// (uc-infra#67, same hole #27's independent review found on the UBL
// export).
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/ids"
	"github.com/universaltill/universal-core/internal/kernel/saft"
)

// saftEntityTypes is every entity type whose data a SAF-T file
// discloses: Account (the chart + balances mirror gl_accounts, which
// finance.SyncGLAccounts projects from Account records), TaxCode (the
// tax table), and Party/PartyRole (customers and suppliers — PartyRole
// is the join that decides who appears, so it gates too, same
// join-target reasoning as reporting.go's purchasingReportEntityTypes).
// Journal entries themselves have no entity Definition (ADR-0004's
// dedicated typed tables) — Account read access is the honest proxy:
// the entries are unreadable without the chart they post to.
var saftEntityTypes = []string{"Account", "TaxCode", "Party", "PartyRole"}

// saftFieldReads is every field ts.crud actually reads for the entity
// types in saftEntityTypes — the field-level half of the whole-file gate
// (uc-infra#67, same hole #27's independent review found on the UBL
// export). The correct test for membership is "does the guarded engine's
// redact() delete this field when hidden", NOT "is this field literally
// serialized into the file" — hiding a field the code merely branches or
// joins on is exactly as silent a failure as hiding a rendered one, just
// with a different symptom. Two ways a deleted field surfaces:
//   - rendered as ""/0 in a schema-valid file (Party.name/tax_id,
//     TaxCode's own fields): the #27-shaped case this fix was written for.
//     Party.registration_number/contact_first_name/contact_last_name
//     (uc-infra#63) are the same shape — a hidden statutory field must
//     refuse the export, not silently fall back to the file's own "NA"
//     marker as if no profile were configured at all.
//   - silently dropped from the file instead: saftParties keys its lookup
//     on PartyRole's own party_id and switches on role_type, so hiding
//     EITHER makes every role fail to resolve or classify — the file
//     would ship with empty Customers/Suppliers master files, a false
//     "this tenant has no trading partners" statutory claim, which is
//     worse than a rendered blank (independent review finding, this
//     card's own fix pass).
//
// Account gets no entry: its fields come from gl_accounts (a typed table
// no FieldPermission governs — see the package doc comment), never
// through ts.crud, so nothing here is ever silently redacted.
var saftFieldReads = map[string][]string{
	"Party":     {"name", "tax_id", "registration_number", "contact_first_name", "contact_last_name"},
	"PartyRole": {"party_id", "role_type"},
	"TaxCode":   {"code", "name", "jurisdiction", "rate"},
}

// saftAccessDenial runs the whole-file gate shared by the export, its
// landing page, and the Finance module menu's link to that page
// (modulemenu.go's moduleReportLinks.ExtraDenied): read access to every
// entity type in saftEntityTypes, plus — for the subset with a
// saftFieldReads entry — that no field the file actually reads is
// hidden for this actor. Returns ("", nil) when access is fine, or a
// message that should reach the actor (never wrapped, since it is not a
// mistake) once it is not. Not a Handler method: nothing here reads
// handler state, only the tenant scope every caller already has.
func saftAccessDenial(ctx context.Context, ts tenantScope) (string, error) {
	for _, entityType := range saftEntityTypes {
		allowed, err := ts.crud.CanRead(ctx, entityType)
		if err != nil {
			return "", fmt.Errorf("check %s read permission for SAF-T export: %w", entityType, err)
		}
		if !allowed {
			return "SAF-T export requires read access to " + entityType, nil
		}
		fields, ok := saftFieldReads[entityType]
		if !ok {
			continue
		}
		hidden, err := ts.crud.HiddenFields(ctx, entityType)
		if err != nil {
			return "", fmt.Errorf("resolve hidden %s fields for SAF-T export: %w", entityType, err)
		}
		for _, f := range fields {
			if hidden[f] {
				return "SAF-T export requires unredacted access to " + entityType + "." + f, nil
			}
		}
	}
	return "", nil
}

// exportSAFT streams the tenant's SAF-T Financial file for the
// inclusive ?from=&to= date range. The file is fully assembled in
// memory before the first response byte: unlike the CSV export's
// streaming tradeoff, an audit file's GeneralLedgerEntries totals
// precede its entries, so the whole document must exist before anything
// can be sent anyway — and buffering means a failure anywhere still
// gets a clean error response, never a truncated file that looks
// complete. Ledger volumes are modest at this product's scale; revisit
// alongside List's own no-pagination note if that changes.
func (h *Handler) exportSAFT(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	if msg, err := saftAccessDenial(r.Context(), ts); err != nil {
		writeInternalError(w, "check SAF-T export access", err)
		return
	} else if msg != "" {
		httpx.WriteError(w, http.StatusForbidden, msg)
		return
	}

	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	for name, v := range map[string]string{"from": from, "to": to} {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, name+" must be an ISO-8601 date (YYYY-MM-DD)")
			return
		}
	}
	if from > to {
		httpx.WriteError(w, http.StatusBadRequest, "from must not be after to")
		return
	}

	input, err := h.buildSAFTInput(r, rc, ts, from, to)
	if err != nil {
		writeInternalError(w, "assemble SAF-T input", err)
		return
	}
	file, err := saft.Build(*input)
	if err != nil {
		writeInternalError(w, "build SAF-T file", err)
		return
	}
	payload, err := saft.Marshal(file)
	if err != nil {
		writeInternalError(w, "marshal SAF-T file", err)
		return
	}

	// The audit row lands before the response is written: a statutory
	// export that was generated but failed mid-send is still a
	// disclosure worth recording, while the reverse (a recorded export
	// that never produced a byte) cannot happen past this point except
	// for network failure the server can't see anyway. Same
	// actor-accountability rules as every mutation (ADR-0001 §14) —
	// this session's own runs record as ai_agent via rc.Actor.
	entry, err := audit.New("SAFTExport", "", audit.ActionExport, rc.Actor, map[string]any{
		"format":  "saft-financial-1.30",
		"from":    from,
		"to":      to,
		"entries": len(input.Entries),
	})
	if err != nil {
		writeInternalError(w, "build SAF-T audit entry", err)
		return
	}
	if err := data.NewAuditRepo(ts.db).Insert(r.Context(), ts.db, entry); err != nil {
		writeInternalError(w, "record SAF-T export audit entry", err)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	// from/to already validated as ISO dates above, so this filename
	// needs no further sanitizing (digits and hyphens only).
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="saft_%s_%s.xml"`, from, to))
	w.Write(payload)
}

// buildSAFTInput assembles the serializer's Input: ledger data from the
// dedicated typed tables (the only SQL is in internal/data), master
// data through the guarded CRUD engine so RBAC redaction/read semantics
// stay exactly what every other reader gets.
func (h *Handler) buildSAFTInput(r *http.Request, rc httpx.RequestContext, ts tenantScope, from, to string) (*saft.Input, error) {
	ctx := r.Context()
	glRepo := data.NewGLAccountRepo(ts.db)
	balances, err := glRepo.BalancesForRange(ctx, from, to)
	if err != nil {
		return nil, err
	}
	currencies, err := glRepo.DistinctCurrencies(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := data.NewJournalEntryRepo(ts.db).ListRange(ctx, from, to)
	if err != nil {
		return nil, err
	}
	// uc-infra#120: the fallback below used to be the hardcoded
	// DefaultGLCurrency constant; it's now the tenant's actual configured
	// base currency when one is set (ResolveBaseCurrency degrades to
	// DefaultGLCurrency itself when it isn't).
	baseCurrency, err := finance.ResolveBaseCurrency(ctx, ts.db)
	if err != nil {
		return nil, err
	}

	input := &saft.Input{
		Created:         time.Now().UTC().Format("2006-01-02"),
		From:            from,
		To:              to,
		SoftwareVersion: buildVersion(),
		CompanyName:     h.tenantDisplayName(r, rc),
		DefaultCurrency: baseCurrency,
	}
	// Single-currency ledger → that currency names the file's amounts.
	// Anything else (empty chart, or the mixed-currency state the
	// ledger's own known-gap notes cover) falls back to the tenant's
	// configured base currency (or the hardcoded default, see above).
	if len(currencies) == 1 {
		input.DefaultCurrency = currencies[0]
	}

	for _, b := range balances {
		input.Accounts = append(input.Accounts, saft.Account{
			Code:         b.Code,
			Name:         b.Name,
			Type:         b.AccountType,
			OpeningMinor: b.OpeningMinor,
			ClosingMinor: b.ClosingMinor,
		})
	}
	saft.SortAccounts(input.Accounts)

	for _, e := range entries {
		entry := saft.Entry{
			ID:          e.ID,
			EntryDate:   e.EntryDate,
			PostedDate:  e.PostedAt,
			Description: e.Description,
			SourceType:  e.SourceType,
		}
		for _, l := range e.Lines {
			entry.Lines = append(entry.Lines, saft.Line{
				AccountCode: l.AccountCode,
				DebitMinor:  l.DebitMinor,
				CreditMinor: l.CreditMinor,
			})
		}
		input.Entries = append(input.Entries, entry)
	}

	customers, suppliers, err := h.saftParties(r, ts)
	if err != nil {
		return nil, err
	}
	input.Customers, input.Suppliers = customers, suppliers

	regNum, taxRegNum, contactFirst, contactLast, err := h.saftCompanyProfile(r, ts)
	if err != nil {
		return nil, err
	}
	input.RegistrationNumber, input.TaxRegistrationNumber = regNum, taxRegNum
	input.ContactFirstName, input.ContactLastName = contactFirst, contactLast

	taxDef, err := ts.entityDef(ctx, "TaxCode")
	if err != nil {
		return nil, fmt.Errorf("look up TaxCode definition: %w", err)
	}
	taxRecs, err := ts.crud.List(ctx, taxDef)
	if err != nil {
		return nil, fmt.Errorf("list TaxCode records: %w", err)
	}
	for _, rec := range taxRecs {
		tc := saft.TaxCode{
			Code:    stringField(rec.Data, "code"),
			Name:    stringField(rec.Data, "name"),
			Country: stringField(rec.Data, "jurisdiction"),
		}
		if rate, ok := rec.Data["rate"].(float64); ok {
			tc.Rate, tc.HasRate = rate, true
		}
		input.TaxCodes = append(input.TaxCodes, tc)
	}
	return input, nil
}

// saftParties reads Party + PartyRole through the guarded engine and
// splits parties into customers (role_type "customer") and suppliers
// (role_type "vendor"). A party holding both roles appears in both
// sections — that is what the SAF-T master files mean, not a duplicate.
// Each section is deduplicated by party id: the XSD keys customers and
// suppliers uniquely, so a party with two customer PartyRole rows must
// still appear once.
func (h *Handler) saftParties(r *http.Request, ts tenantScope) (customers, suppliers []saft.Party, err error) {
	ctx := r.Context()
	partyDef, err := ts.entityDef(ctx, "Party")
	if err != nil {
		return nil, nil, fmt.Errorf("look up Party definition: %w", err)
	}
	partyRecs, err := ts.crud.List(ctx, partyDef)
	if err != nil {
		return nil, nil, fmt.Errorf("list Party records: %w", err)
	}
	parties := make(map[string]saft.Party, len(partyRecs))
	for _, rec := range partyRecs {
		parties[rec.ID] = saft.Party{
			ID:                 rec.ID,
			Name:               stringField(rec.Data, "name"),
			RegistrationNumber: stringField(rec.Data, "tax_id"),
		}
	}

	roleDef, err := ts.entityDef(ctx, "PartyRole")
	if err != nil {
		return nil, nil, fmt.Errorf("look up PartyRole definition: %w", err)
	}
	roleRecs, err := ts.crud.List(ctx, roleDef)
	if err != nil {
		return nil, nil, fmt.Errorf("list PartyRole records: %w", err)
	}
	seenCustomer, seenSupplier := map[string]bool{}, map[string]bool{}
	for _, rec := range roleRecs {
		p, ok := parties[stringField(rec.Data, "party_id")]
		if !ok {
			// A role pointing at a missing/hidden party: nothing to
			// export for it (the guarded engine already withheld
			// anything this actor may not see).
			continue
		}
		switch stringField(rec.Data, "role_type") {
		case "customer":
			if !seenCustomer[p.ID] {
				seenCustomer[p.ID] = true
				customers = append(customers, p)
			}
		case "vendor":
			if !seenSupplier[p.ID] {
				seenSupplier[p.ID] = true
				suppliers = append(suppliers, p)
			}
		}
	}
	return customers, suppliers, nil
}

// saftCompanyProfile reads the Party holding the "own_organization"
// PartyRole (uc-infra#63) — the tenant's own statutory identity — through
// ownOrganizationParty, which goes through the same guarded engine
// saftParties already uses for customers/suppliers. Zero such Party (no
// statutory profile configured
// yet — today's status quo for every existing tenant), more than one
// DISTINCT such Party, and a role row whose party_id resolves to nothing
// all degrade identically to all-empty: saft.Build already falls back to
// the spec's NA markers for empty Input fields, the same fail-safe
// "don't guess, degrade" posture foundation.SystemOfRecord's own doc
// comment argues for a structurally similar one-row-per-tenant
// convention.
func (h *Handler) saftCompanyProfile(r *http.Request, ts tenantScope) (regNum, taxRegNum, contactFirst, contactLast string, err error) {
	org, ok, err := h.ownOrganizationParty(r.Context(), ts)
	if err != nil || !ok {
		return "", "", "", "", err
	}
	return stringField(org.Data, "registration_number"),
		stringField(org.Data, "tax_id"),
		stringField(org.Data, "contact_first_name"),
		stringField(org.Data, "contact_last_name"),
		nil
}

// ownOrganizationParty resolves the Party holding the "own_organization"
// PartyRole (uc-infra#63) — the tenant's own statutory identity — through
// the guarded engine. ok=false covers every degrade case a caller must
// treat identically to "no statutory profile configured": zero matching
// role rows, more than one DISTINCT resolved Party (an unresolvable
// data-quality ambiguity no caller has business guessing at), and a role
// row whose party_id resolves to nothing (deleted, blank, or malformed).
// Shared by saftCompanyProfile and the UBL export's own tenant-party
// resolution (uc-infra#119) so this dedupe/guard logic — the exact two
// bugs #63's independent review found and fixed — lives in exactly one
// place instead of drifting across two copies.
func (h *Handler) ownOrganizationParty(ctx context.Context, ts tenantScope) (org data.Record, ok bool, err error) {
	roleDef, err := ts.entityDef(ctx, "PartyRole")
	if err != nil {
		// secondaryDefLookupError, not a plain fmt.Errorf-wrapped one:
		// this helper is shared with UBL export (see doc comment above),
		// whose writeUBLError must not mistake "PartyRole has no
		// published Definition" for "the requested document doesn't
		// exist" (uc-infra#179). SAF-T's own caller never maps errors to
		// a 404, so this makes no difference on that path.
		return data.Record{}, false, secondaryDefLookupError{entityType: "PartyRole", err: err}
	}
	roleRecs, err := ts.crud.ListByField(ctx, roleDef, "role_type", "own_organization")
	if err != nil {
		return data.Record{}, false, fmt.Errorf("list own_organization PartyRole records: %w", err)
	}
	// Resolved by DISTINCT party id, not by row count: nothing in the
	// generic entity/crud layer makes PartyRole rows unique, so the same
	// Party can hold the role twice (independent review). Two rows naming
	// ONE Party is not the ambiguity this degrades for — every row agrees
	// which Party is the tenant, so resolving it is not a guess; counting
	// rows would instead swap a configured profile silently back to NA.
	// Same dedupe-by-party-id reasoning saftParties applies to its own
	// role→party join. Two rows naming DIFFERENT Parties still degrades.
	//
	// ids.Canonical, not a bare string compare or a canonical-form regex:
	// it accepts every spelling Postgres's own uuid_in does, so two
	// spellings of one id compare as the same Party rather than as an
	// ambiguity (uc-infra#107's review — that distinction is load-bearing,
	// not cosmetic). ok=false is a value that can never be a records.id:
	// blank, or malformed. That case is reachable, not theoretical —
	// PartyRole.party_id declares no TargetFilter/MustMatchParentField, so
	// crud's reference-target check skips it and entity.ValidateRecord
	// only asserts "non-nil string" — and without this guard, records.id
	// being a uuid column means the Get below returns a raw Postgres
	// "invalid input syntax for type uuid" driver error rather than
	// data.ErrNotFound, so one junk role row 500s the whole caller.
	// Treated as the tolerated dangling reference it is, exactly like the
	// ErrNotFound case below.
	partyID := ""
	for i, rec := range roleRecs {
		id, idOK := ids.Canonical(stringField(rec.Data, "party_id"))
		if !idOK || (i > 0 && id != partyID) {
			return data.Record{}, false, nil
		}
		partyID = id
	}
	if partyID == "" {
		// No own_organization role at all — every tenant's status quo
		// before a statutory profile is configured.
		return data.Record{}, false, nil
	}

	partyDef, err := ts.entityDef(ctx, "Party")
	if err != nil {
		// Same reasoning as the PartyRole lookup above.
		return data.Record{}, false, secondaryDefLookupError{entityType: "Party", err: err}
	}
	org, err = ts.crud.Get(ctx, partyDef, partyID)
	if errors.Is(err, data.ErrNotFound) {
		// The role points at a Party that no longer exists — degrade
		// the same as the zero-match case above, not a caller failure.
		return data.Record{}, false, nil
	}
	if err != nil {
		return data.Record{}, false, fmt.Errorf("get own_organization Party: %w", err)
	}
	return org, true, nil
}

// tenantDisplayName resolves the exporting tenant's human name for the
// file's Company element: the control-plane tenant record when the
// member-management wiring provided a TenantRepo, else the session's
// own tenant list (the same source the nav's switcher shows), else
// empty — saft.Build substitutes the spec's NA marker.
func (h *Handler) tenantDisplayName(r *http.Request, rc httpx.RequestContext) string {
	if h.tenants != nil {
		if names, err := h.tenants.NamesByIDs(r.Context(), []string{rc.TenantID}); err == nil && names[rc.TenantID] != "" {
			return names[rc.TenantID]
		}
	}
	if h.auth.Enabled() {
		for _, o := range h.auth.SessionTenants(r) {
			if o.ID == rc.TenantID {
				return o.Name
			}
		}
	}
	return ""
}

// buildVersion is the running binary's module version — what the file's
// SoftwareVersion element carries. "dev" when built without module
// version stamping (go run, local builds).
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// stringField reads a string-typed field from a record's data map,
// tolerating absence and non-string junk the same way listview.go's
// cell rendering does — an export must not 500 over one malformed row.
func stringField(data map[string]any, field string) string {
	v, _ := data[field].(string)
	return v
}

// ---- the form page -----------------------------------------------------

// saftExportPage renders the small page the Finance module menu links
// to: two date inputs (defaulting to the current calendar year so far)
// and a download button that GETs /export/saft. Gated on the same
// entity-type reads as the export itself, so nobody lands on a form
// whose submit can only 403 (the same link-vs-dead-end reasoning
// moduleReportLinks' RequiredRead already applies one level up).
func (h *Handler) saftExportPage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	locale := localeFromRequest(w, r)
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	msg, err := saftAccessDenial(r.Context(), ts)
	if !h.denyPageUnless(w, r, &rc, locale, msg == "", err, "check SAF-T export page access") {
		return
	}

	now := time.Now().UTC()
	view := saftPageView{
		Heading:     h.catalog.T(locale, "saft.heading"),
		Intro:       h.catalog.T(locale, "saft.intro"),
		FromLabel:   h.catalog.T(locale, "saft.from_label"),
		ToLabel:     h.catalog.T(locale, "saft.to_label"),
		ButtonLabel: h.catalog.T(locale, "saft.download_button"),
		DefaultFrom: time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02"),
		DefaultTo:   now.Format("2006-01-02"),
	}
	var buf bytes.Buffer
	if err := saftPageTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render SAF-T export page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render SAF-T export page shell", err)
	}
}

type saftPageView struct {
	Heading     string
	Intro       string
	FromLabel   string
	ToLabel     string
	ButtonLabel string
	DefaultFrom string
	DefaultTo   string
}

var saftPageTmpl = template.Must(template.New("saftExport").Parse(`
<h1>{{.Heading}}</h1>
<p>{{.Intro}}</p>
<form class="uc-form uc-saft-form" method="get" action="/export/saft">
  <label>{{.FromLabel}} <input type="date" name="from" value="{{.DefaultFrom}}" required></label>
  <label>{{.ToLabel}} <input type="date" name="to" value="{{.DefaultTo}}" required></label>
  <button type="submit">{{.ButtonLabel}}</button>
</form>
`))
