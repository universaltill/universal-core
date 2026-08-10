// The UBL export endpoint (universaltill/uc-infra#27) — one business
// document out as OASIS UBL 2.1 XML, produced by internal/kernel/ubl
// (see that package's doc comment for the document mappings, the
// summary-invoice-line decision, and the core-not-plugin placement
// note). GET /export/{entityType}/{id}/ubl for entityType in
// PurchaseOrder | SalesOrder | CustomerInvoice.
//
// Like the SAF-T export (saftexport.go) and unlike the generic CSV
// export, the document has a fixed schema in which per-field redaction
// is not meaningful — but the guarded engine still redacts, and a
// redacted field would come out of stringField/floatField as ""/0 and
// produce a schema-valid document whose amounts are silently false
// (independent review finding, this card). So access is gated up front
// on BOTH read permission for every entity type the document disclose
// AND the absence of field-level hiding on any field the document
// actually reads. One audit row records each export.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/money"
	"github.com/universaltill/universal-core/internal/kernel/ubl"
)

// ublDocReads maps each exportable entity type to every entity type its
// document disclose AND the exact fields read from each — the
// whole-document permission gate checks both levels against this one
// declaration, and the build funcs read nothing outside it. Order
// documents render their lines (POLine/SOLine → Item) and both parties;
// an invoice renders its customer and the linked sales order's number
// only (the summary-line shape), so Item and the line entities are
// correctly absent from its set.
//
// PartyRole (party_id, role_type) is declared in every entry because
// ublTenantParty (uc-infra#119) resolves the own_organization role for
// every document type, the same PartyRole.party_id/role_type read
// saftFieldReads already declares for SAF-T's identical lookup — an
// actor lacking PartyRole read access, or with either field hidden,
// must be refused up front, not silently degraded to "no statutory
// profile configured" as if the tenant genuinely had none (the
// "silently dropped" failure shape saftFieldReads's own comment
// describes, not the "rendered blank" one). Party.tax_id is already
// declared below for the counterparty and covers the tenant's own
// org Party too — HiddenFields is a per-entity-type check, not
// per-record, so one declaration gates both reads.
var ublDocReads = map[string]map[string][]string{
	"PurchaseOrder": {
		"PurchaseOrder": {"po_number", "vendor_id", "order_date", "currency_id", "total"},
		"POLine":        {"purchase_order_id", "item_id", "qty", "unit_price", "line_total"},
		"Item":          {"sku", "name"},
		"Party":         {"name", "tax_id"},
		"PartyRole":     {"party_id", "role_type"},
		"Currency":      {"code", "minor_unit"},
	},
	"SalesOrder": {
		"SalesOrder": {"so_number", "customer_id", "order_date", "currency_id", "total"},
		"SOLine":     {"sales_order_id", "item_id", "qty", "unit_price", "line_total"},
		"Item":       {"sku", "name"},
		"Party":      {"name", "tax_id"},
		"PartyRole":  {"party_id", "role_type"},
		"Currency":   {"code", "minor_unit"},
	},
	"CustomerInvoice": {
		"CustomerInvoice": {"invoice_number", "sales_order_id", "customer_id", "invoice_date", "currency_id", "total"},
		"SalesOrder":      {"so_number"},
		"Party":           {"name", "tax_id"},
		"PartyRole":       {"party_id", "role_type"},
		"Currency":        {"code", "minor_unit"},
	},
}

// exportUBL streams one record as a UBL 2.1 document attachment.
func (h *Handler) exportUBL(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	entityType, id := r.PathValue("entityType"), r.PathValue("id")

	reads, ok := ublDocReads[entityType]
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "UBL export is not available for entity type "+entityType)
		return
	}
	// Same malformed-id guard every record route applies (see idPattern's
	// own comment): without it a non-UUID surfaces as a driver-error 500
	// — and the raw path value would reach the server log through the
	// Postgres error text.
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}
	for et, fields := range reads {
		allowed, err := ts.crud.CanRead(r.Context(), et)
		if err != nil {
			writeInternalError(w, "check "+et+" read permission for UBL export", err)
			return
		}
		if !allowed {
			httpx.WriteError(w, http.StatusForbidden, "UBL export requires read access to "+et)
			return
		}
		// A field this document reads that is hidden for this actor
		// would not error — the guarded engine deletes it from the
		// record and the document would render it as ""/0: a schema-
		// valid file with silently false amounts or a nameless party.
		// Refusing outright is the only honest answer.
		hidden, err := ts.crud.HiddenFields(r.Context(), et)
		if err != nil {
			writeInternalError(w, "resolve hidden "+et+" fields for UBL export", err)
			return
		}
		for _, f := range fields {
			if hidden[f] {
				httpx.WriteError(w, http.StatusForbidden, "UBL export requires unredacted access to "+et+"."+f)
				return
			}
		}
	}

	var (
		payload []byte
		number  string // the document's natural key, for the filename
		format  string // the audit row's format marker
	)
	switch entityType {
	case "PurchaseOrder", "SalesOrder":
		payload, number, err = h.buildUBLOrder(r, rc, ts, entityType, id)
		format = "ubl-2.1-order"
	case "CustomerInvoice":
		payload, number, err = h.buildUBLInvoice(r, rc, ts, id)
		format = "ubl-2.1-invoice"
	default:
		// Unreachable while the switch and ublDocReads agree; a key
		// added to the map without a case must fail loudly here, not
		// fall through to an empty 200 plus an audit row for an export
		// that produced no bytes.
		writeInternalError(w, "UBL export", fmt.Errorf("entity type %s is in ublDocReads but has no build case", entityType))
		return
	}
	if err != nil {
		writeUBLError(w, entityType, id, err)
		return
	}

	// Audit before the response bytes, same disclosure-accountability
	// reasoning as the SAF-T export's own comment (ADR-0001 §14).
	entry, err := audit.New(entityType, id, audit.ActionExport, rc.Actor, map[string]any{
		"format": format,
		"number": number,
	})
	if err != nil {
		writeInternalError(w, "build UBL export audit entry", err)
		return
	}
	if err := data.NewAuditRepo(ts.db).Insert(r.Context(), ts.db, entry); err != nil {
		writeInternalError(w, "record UBL export audit entry", err)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.xml"`, safeFilenameComponent(number)))
	w.Write(payload)
}

// ublInputError marks an assembly failure that is the record's state
// rather than the server's — a missing currency, an empty line set, a
// dangling reference — and surfaces as a 400 with the message verbatim.
type ublInputError struct{ msg string }

func (e ublInputError) Error() string { return e.msg }

// ublPrimaryDefLookupError marks a failed lookup of the URL's own
// document entity type's Definition (PurchaseOrder/SalesOrder/
// CustomerInvoice) — distinct from data.ErrNotFound on that same type's
// *record* (the later ts.crud.Get call, never wrapped in either error
// type here). "This entity type has no published Definition at all" is
// not "id doesn't exist"; it's the exact condition every other CRUD-ish
// route in this package already answers via writeDefinitionLookupError
// (handlers.go) — a 404 with the honest "no published definition for
// entity type %q" message — so UBL export matches that established
// convention instead of a bespoke response. Independent review of this
// card's first draft (uc-infra#179) caught that draft silently 500ing
// this case instead: it had folded the primary type into the same
// "secondary lookup" bucket below, which is correct for e.g. Currency
// or Party but wrong for the document's own type, and untested (the
// review's own coverage pass showed 0 hits on that line).
type ublPrimaryDefLookupError struct {
	entityType string
	err        error
}

func (e ublPrimaryDefLookupError) Error() string {
	return fmt.Sprintf("look up %s definition: %s", e.entityType, e.err)
}

func (e ublPrimaryDefLookupError) Unwrap() error { return e.err }

// secondaryDefLookupError marks a failed entity-Definition lookup
// (ts.entityDef) for anything OTHER than the URL's own document type —
// Party, PartyRole, Currency, SalesOrder (the invoice's linked order),
// the line entity, Item — as distinct from data.ErrNotFound on the
// document's own record (uc-infra#179). Both wrap data.ErrNotFound when
// the failing Definition simply isn't published (e.g. a partial/failed
// module publish), so without this marker writeUBLError could not tell
// "an unrelated internal lookup is broken" apart from "the requested
// document doesn't exist" — and must never report the former as the
// latter. Always a 500: unlike the primary type above, there is no
// honest "not found" framing a caller could act on for a document field
// they didn't even ask about. Shared with saftexport.go's
// ownOrganizationParty (SAF-T's own caller never maps any error to a
// 404, so wrapping there is behavior-neutral for that path — done only
// for consistency, since ownOrganizationParty is reachable from UBL
// export too, via ublTenantParty).
type secondaryDefLookupError struct {
	entityType string
	err        error
}

func (e secondaryDefLookupError) Error() string {
	return fmt.Sprintf("look up %s definition: %s", e.entityType, e.err)
}

func (e secondaryDefLookupError) Unwrap() error { return e.err }

func writeUBLError(w http.ResponseWriter, entityType, id string, err error) {
	var inputErr ublInputError
	var primErr ublPrimaryDefLookupError
	var secErr secondaryDefLookupError
	switch {
	// Both def-lookup cases are checked before the plain ErrNotFound
	// case below: each wraps data.ErrNotFound too when the failing
	// Definition simply isn't published, so either would otherwise still
	// match errors.Is(err, data.ErrNotFound) and take the generic branch.
	case errors.As(err, &primErr):
		writeDefinitionLookupError(w, primErr.entityType, primErr.err)
	case errors.As(err, &secErr):
		writeInternalError(w, "assemble UBL document for "+entityType, err)
	case errors.Is(err, data.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, entityType+" "+id+" not found")
	case errors.As(err, &inputErr):
		httpx.WriteError(w, http.StatusBadRequest, inputErr.Error())
	default:
		writeInternalError(w, "assemble UBL document for "+entityType, err)
	}
}

// buildUBLOrder assembles and serializes one PurchaseOrder or
// SalesOrder as a UBL Order. The two differ only in field names and
// which side of the document the tenant stands on: the tenant buys on a
// PurchaseOrder and sells on a SalesOrder.
func (h *Handler) buildUBLOrder(r *http.Request, rc httpx.RequestContext, ts tenantScope, entityType, id string) (payload []byte, number string, err error) {
	ctx := r.Context()
	var numberField, partyField, dateField, lineEntity, lineParent string
	if entityType == "PurchaseOrder" {
		numberField, partyField, dateField = "po_number", "vendor_id", "order_date"
		lineEntity, lineParent = "POLine", "purchase_order_id"
	} else {
		numberField, partyField, dateField = "so_number", "customer_id", "order_date"
		lineEntity, lineParent = "SOLine", "sales_order_id"
	}

	def, err := ts.entityDef(ctx, entityType)
	if err != nil {
		return nil, "", ublPrimaryDefLookupError{entityType: entityType, err: err}
	}
	rec, err := ts.crud.Get(ctx, def, id)
	if err != nil {
		return nil, "", err
	}
	number = stringField(rec.Data, numberField)

	counterparty, err := h.ublParty(ctx, ts, stringField(rec.Data, partyField), entityType)
	if err != nil {
		return nil, "", err
	}
	currency, minor, err := h.ublCurrency(ctx, ts, rec.Data, entityType, number)
	if err != nil {
		return nil, "", err
	}
	lines, err := h.ublLines(ctx, ts, lineEntity, lineParent, id)
	if err != nil {
		return nil, "", err
	}
	if len(lines) == 0 {
		return nil, "", ublInputError{msg: fmt.Sprintf("cannot export %s %s as UBL: it has no lines (UBL requires at least one order line)", entityType, number)}
	}

	in := ubl.OrderInput{
		ID:           number,
		IssueDate:    stringField(rec.Data, dateField),
		CurrencyCode: currency,
		MinorUnit:    minor,
		Lines:        lines,
		// amountField, not floatField (uc-infra#136): this same function
		// builds both a PurchaseOrder (total is FieldMoney now, minor
		// units) and a SalesOrder (total is still FieldNumber, major
		// units) document — def is whichever of the two rec actually is,
		// so amountField's own field-type check is what keeps this one
		// call site correct for both without an entityType branch here.
		Total: amountField(rec.Data, def, "total"),
	}
	tenant, err := h.ublTenantParty(r, rc, ts)
	if err != nil {
		return nil, "", err
	}
	if entityType == "PurchaseOrder" {
		in.Buyer, in.Seller = tenant, counterparty
	} else {
		in.Buyer, in.Seller = counterparty, tenant
	}

	doc, err := ubl.BuildOrder(in)
	if err != nil {
		return nil, "", ublInputError{msg: err.Error()}
	}
	payload, err = ubl.MarshalOrder(doc)
	return payload, number, err
}

// buildUBLInvoice assembles and serializes one CustomerInvoice as a UBL
// Invoice (tenant = AccountingSupplierParty — the issuer).
func (h *Handler) buildUBLInvoice(r *http.Request, rc httpx.RequestContext, ts tenantScope, id string) (payload []byte, number string, err error) {
	ctx := r.Context()
	def, err := ts.entityDef(ctx, "CustomerInvoice")
	if err != nil {
		return nil, "", ublPrimaryDefLookupError{entityType: "CustomerInvoice", err: err}
	}
	rec, err := ts.crud.Get(ctx, def, id)
	if err != nil {
		return nil, "", err
	}
	number = stringField(rec.Data, "invoice_number")

	customer, err := h.ublParty(ctx, ts, stringField(rec.Data, "customer_id"), "CustomerInvoice")
	if err != nil {
		return nil, "", err
	}
	currency, minor, err := h.ublCurrency(ctx, ts, rec.Data, "CustomerInvoice", number)
	if err != nil {
		return nil, "", err
	}

	// The linked SalesOrder contributes only its number (OrderReference
	// + the summary line's description). A dangling reference degrades
	// to omitting it rather than failing: the invoice's own data is
	// complete without it.
	orderRef := ""
	if soID := stringField(rec.Data, "sales_order_id"); soID != "" {
		soDef, err := ts.entityDef(ctx, "SalesOrder")
		if err != nil {
			return nil, "", secondaryDefLookupError{entityType: "SalesOrder", err: err}
		}
		if so, err := ts.crud.Get(ctx, soDef, soID); err == nil {
			orderRef = stringField(so.Data, "so_number")
		} else if !errors.Is(err, data.ErrNotFound) {
			return nil, "", err
		}
	}

	tenant, err := h.ublTenantParty(r, rc, ts)
	if err != nil {
		return nil, "", err
	}
	doc, err := ubl.BuildInvoice(ubl.InvoiceInput{
		ID:           number,
		IssueDate:    stringField(rec.Data, "invoice_date"),
		OrderRef:     orderRef,
		CurrencyCode: currency,
		MinorUnit:    minor,
		Supplier:     tenant,
		Customer:     customer,
		// amountField, not floatField (nit from independent review of
		// uc-infra#136): CustomerInvoice.total is still FieldNumber
		// today, so this reads identically to floatField either way —
		// but using the same Definition-driven helper as buildUBLOrder's
		// two call sites means this one self-corrects for free the day
		// CustomerInvoice.total migrates too, instead of needing its own
		// separate fix then.
		Total: amountField(rec.Data, def, "total"),
	})
	if err != nil {
		return nil, "", ublInputError{msg: err.Error()}
	}
	payload, err = ubl.MarshalInvoice(doc)
	return payload, number, err
}

// ublParty resolves a Party reference into the serializer's Party. A
// dangling required reference is the exporting record's problem, not
// the server's — a 400, not a 404 (the URL's record exists) or a 500.
func (h *Handler) ublParty(ctx context.Context, ts tenantScope, partyID, docEntityType string) (ubl.Party, error) {
	if partyID == "" {
		return ubl.Party{}, ublInputError{msg: "cannot export " + docEntityType + " as UBL: it has no counterparty set"}
	}
	def, err := ts.entityDef(ctx, "Party")
	if err != nil {
		return ubl.Party{}, secondaryDefLookupError{entityType: "Party", err: err}
	}
	rec, err := ts.crud.Get(ctx, def, partyID)
	if errors.Is(err, data.ErrNotFound) {
		return ubl.Party{}, ublInputError{msg: "cannot export " + docEntityType + " as UBL: its counterparty Party " + partyID + " does not exist"}
	}
	if err != nil {
		return ubl.Party{}, err
	}
	return ubl.Party{
		Name:  stringField(rec.Data, "name"),
		TaxID: stringField(rec.Data, "tax_id"),
	}, nil
}

// ublCurrency resolves the record's currency_id into (ISO code, minor
// unit). currency_id is optional on the entity but a UBL amount's
// currencyID attribute is mandatory, so a record without one cannot be
// exported — a clean 400, never invalid XML (BA acceptance criterion).
func (h *Handler) ublCurrency(ctx context.Context, ts tenantScope, recData map[string]any, docEntityType, number string) (code string, minor int, err error) {
	currencyID := stringField(recData, "currency_id")
	if currencyID == "" {
		return "", 0, ublInputError{msg: fmt.Sprintf("cannot export %s %s as UBL: it has no currency set (UBL amounts require a currency)", docEntityType, number)}
	}
	def, err := ts.entityDef(ctx, "Currency")
	if err != nil {
		return "", 0, secondaryDefLookupError{entityType: "Currency", err: err}
	}
	rec, err := ts.crud.Get(ctx, def, currencyID)
	if errors.Is(err, data.ErrNotFound) {
		return "", 0, ublInputError{msg: fmt.Sprintf("cannot export %s %s as UBL: its Currency %s does not exist", docEntityType, number, currencyID)}
	}
	if err != nil {
		return "", 0, err
	}
	minor = 2
	if v, ok := rec.Data["minor_unit"].(float64); ok {
		minor = int(v)
	}
	return stringField(rec.Data, "code"), minor, nil
}

// ublLines loads a document's line records (POLine/SOLine) and resolves
// each line's Item for its name/sku. Line order is ListByField's own
// ORDER BY created_at, id — entry order, which is what the operator who
// keyed the document expects a trading partner to see (no line-number
// concept exists on these entities yet). An earlier draft re-sorted by
// record UUID here, which is deterministic but arbitrary and shuffled
// the operator's lines half the time — independent review finding.
func (h *Handler) ublLines(ctx context.Context, ts tenantScope, lineEntity, lineParent, parentID string) ([]ubl.Line, error) {
	lineDef, err := ts.entityDef(ctx, lineEntity)
	if err != nil {
		return nil, secondaryDefLookupError{entityType: lineEntity, err: err}
	}
	recs, err := ts.crud.ListByField(ctx, lineDef, lineParent, parentID)
	if err != nil {
		return nil, fmt.Errorf("list %s lines: %w", lineEntity, err)
	}

	itemDef, err := ts.entityDef(ctx, "Item")
	if err != nil {
		return nil, secondaryDefLookupError{entityType: "Item", err: err}
	}
	items := map[string]data.Record{}
	lines := make([]ubl.Line, 0, len(recs))
	for _, rec := range recs {
		line := ubl.Line{
			ID:  rec.ID,
			Qty: floatField(rec.Data, "qty"), // qty never migrates to FieldMoney — it's a quantity, not an amount.
			// amountField, not floatField (uc-infra#136): this function
			// builds lines for both POLine (unit_price/line_total are
			// FieldMoney now, minor units) and SOLine (still FieldNumber,
			// major units) — lineDef is whichever of the two lineEntity
			// actually is, so amountField's own field-type check is what
			// keeps these two call sites correct for both without a
			// lineEntity branch here.
			UnitPrice: amountField(rec.Data, lineDef, "unit_price"),
			LineTotal: amountField(rec.Data, lineDef, "line_total"),
		}
		if itemID := stringField(rec.Data, "item_id"); itemID != "" {
			item, ok := items[itemID]
			if !ok {
				item, err = ts.crud.Get(ctx, itemDef, itemID)
				if err != nil && !errors.Is(err, data.ErrNotFound) {
					return nil, err
				}
				items[itemID] = item
			}
			// A dangling item reference leaves the line's name empty
			// rather than failing the export: the line's own quantity
			// and amounts are still true.
			line.ItemSKU = stringField(item.Data, "sku")
			line.ItemName = stringField(item.Data, "name")
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// ublTenantParty is the exporting tenant's side of every document. Name
// is the tenant display name — falling back to the tenant id when no
// name source is wired (control-plane repo or session), because a UBL
// party with an empty name is materially incomplete while the id is at
// least a truthful identifier. This name resolution is deliberately
// unchanged by uc-infra#119: the own_organization Party's own `name`
// field is never used as a display-name override, the same precedent
// saftCompanyProfile already set for SAF-T's CompanyName (its own
// Company element is sourced from tenantDisplayName too, never the org
// Party's name) — only registration/tax identifiers flow from that
// Party, never the display name.
//
// TaxID comes from the own_organization Party (uc-infra#63/#119) via
// ownOrganizationParty, the same access path and degrade semantics
// saftCompanyProfile uses: no statutory profile configured yet, an
// ambiguous own_organization Party, or a dangling role all leave TaxID
// empty (ubl.Party's own doc comment: empty omits PartyTaxScheme
// entirely, which is UBL-optional, unlike SAF-T's mandatory-with-"NA"
// convention) — today's pre-#119 status quo for every tenant without a
// statutory profile.
func (h *Handler) ublTenantParty(r *http.Request, rc httpx.RequestContext, ts tenantScope) (ubl.Party, error) {
	name := h.tenantDisplayName(r, rc)
	if name == "" {
		name = rc.TenantID
	}
	party := ubl.Party{Name: name}
	org, ok, err := h.ownOrganizationParty(r.Context(), ts)
	if err != nil {
		return ubl.Party{}, err
	}
	if ok {
		party.TaxID = stringField(org.Data, "tax_id")
	}
	return party, nil
}

// floatField reads a JSONB number field as float64 (the entity engine's
// storage type for FieldNumber); anything else — missing, null, a
// string — is 0, same leniency as stringField.
func floatField(recData map[string]any, field string) float64 {
	v, _ := recData[field].(float64)
	return v
}

// amountField reads recData[field] as a major-unit decimal amount — what
// a UBL amount element requires — honoring def's declared field type
// (uc-infra#136). entity.FieldMoney's stored value is minor units
// (1050), so it goes through money.FromAny(...).Major() to reach the
// major-unit decimal (10.50) a UBL amount needs; an entity.FieldNumber
// field is already a major-unit decimal and reads via floatField
// unchanged. Exists because buildUBLOrder/ublLines are shared between an
// entity type whose amount fields have migrated to FieldMoney
// (PurchaseOrder/POLine) and one that hasn't yet (SalesOrder/SOLine) —
// without this, the same unconditional floatField call would read a
// migrated field's raw minor-units integer straight into the XML,
// producing an amount 100x too large.
//
// Falls back to floatField for a field def doesn't declare at all (def
// nil, or FieldByName miss) — a defensive path, not currently reached by
// any real caller (every call site here passes the actual Definition
// ublDocReads/this handler's own lookups already resolved), but the same
// "missing/wrong-shape degrades to 0" leniency floatField/stringField
// already establish, not a reason to fail the whole export.
//
// money.Major() always divides by 100 (money.Decimals' own documented,
// deliberate 2-decimal-place simplification — uc-infra#163, tracked
// separately from this migration) regardless of the document's actual
// currency, so a 3-decimal currency (KWD/BHD) would export a PurchaseOrder/
// POLine FieldMoney amount 10x wrong even though ublCurrency (below)
// already resolves the real minor_unit two functions away for the XML's
// own currencyID/decimal-place metadata. Not a regression this migration
// introduces (every FieldMoney amount in this kernel shares the same
// 2-decimal assumption since ADR-0021), but it is newly REACHABLE here —
// naming it in this comment rather than only in the money package's, so
// a future currency-aware fix knows to check this call site too.
func amountField(recData map[string]any, def *entity.Definition, field string) float64 {
	if def != nil {
		if f, ok := def.FieldByName(field); ok && f.Type == entity.FieldMoney {
			if m, err := money.FromAny(recData[field]); err == nil {
				return m.Major()
			}
			return 0
		}
	}
	return floatField(recData, field)
}
