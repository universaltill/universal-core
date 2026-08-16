package purchasing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
)

// Publish brings a tenant's Purchasing module online (the *sql.DB passed
// in is already resolved to that tenant's own database — ADR-0003):
// every All() Definition, published into the entity_definitions registry
// via the normal draft -> approve -> publish lifecycle
// (moduleseed.PublishAll, shared with internal/kernel/foundation.Publish
// — see that package's doc comment for the idempotency/resume/
// concurrency contract this inherits unchanged).
//
// Unlike foundation, Purchasing is NOT part of every tenant's baseline —
// ADR-0001 §8 draws the "always present" line specifically around the
// foundation set. Call this only for a tenant that has actually licensed
// the Purchasing module (module-gating itself isn't built yet — this
// function doesn't check anything, the caller decides whether to call it
// — see QUEUE.md).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	items := make([]moduleseed.Item, 0, len(All()))
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("purchasing definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Purchasing Form Definitions online —
// separate from Publish for the same reason
// foundation.PublishForms is separate from foundation.Publish (a form is
// a presentation choice, not the "always present" entity guarantee).
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("purchasing form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// statusSpec aliases the shared seeder's Spec — the third consumer
// (internal/kernel/modulebundle) pulled the twin purchasing/sales
// copies into internal/kernel/statusgraph, the same threshold that
// created moduleseed. A kernel dependency, not the cross-module import
// the original duplication note ruled out.
type statusSpec = statusgraph.Spec

// purchaseOrderStatusSpecs, vendorInvoiceStatusSpecs, stockTransferStatusSpecs,
// and rfqStatusSpecs are package-level (rather than inlined into
// PublishStatuses below) so StatusSpecs can expose the identical literals
// — Translations included — to cmd/backfill-status-name-translations
// (uc-infra#244) without a second, driftable copy.
//
// Translations content: draft/submitted/approved/received match this
// module's own shipped tr/fa/ar help topics (internal/help/content/
// {tr,fa,ar}/entity/{PurchaseOrder,VendorInvoice,StockTransfer,
// RequestForQuotation}.md) — the exact words those topics already use to
// describe this same status graph. cancelled instead reuses the
// pre-existing field.PurchaseOrder.status.cancelled / field.
// RequestForQuotation.status.cancelled / field.StockTransfer.status.
// cancelled catalog entries (internal/i18n/locales/{ar,fa,tr}.json,
// already shipped and used by internal/api/reporting.go and
// rfq_report.go) — those help topics only use the verbal-noun form
// ("الإلغاء"/"لغو کردن"/"İptal", as in "cancelling is possible from...")
// in prose, not the status-label form itself. Either way, not
// independently invented wording.
var purchaseOrderStatusSpecs = []statusSpec{
	{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}, Sequence: 1, IsInitial: true, IsTerminal: false},
	{Code: "submitted", Name: "Submitted", Translations: map[string]string{"ar": "مُقدَّم", "fa": "ارسال‌شده", "tr": "Gönderildi"}, Sequence: 2, IsInitial: false, IsTerminal: false},
	{Code: "approved", Name: "Approved", Translations: map[string]string{"ar": "معتمد", "fa": "تأییدشده", "tr": "Onaylandı"}, Sequence: 3, IsInitial: false, IsTerminal: false},
	{Code: "received", Name: "Received", Translations: map[string]string{"ar": "مستلم", "fa": "دریافت‌شده", "tr": "Teslim Alındı"}, Sequence: 4, IsInitial: false, IsTerminal: true},
	{Code: "cancelled", Name: "Cancelled", Translations: map[string]string{"ar": "ملغى", "fa": "لغوشده", "tr": "İptal Edildi"}, Sequence: 5, IsInitial: false, IsTerminal: true},
}

var vendorInvoiceStatusSpecs = []statusSpec{
	{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}, Sequence: 1, IsInitial: true, IsTerminal: false},
	{Code: "matched", Name: "Matched", Translations: map[string]string{"ar": "مطابقة", "fa": "تطبیق‌شده", "tr": "Eşleştirildi"}, Sequence: 2, IsInitial: false, IsTerminal: false},
	// Sequence 3, ahead of paid/void: display-order only (this
	// kernel has no other meaning for Sequence), placed here
	// because an exception is a "still in flight" state like
	// draft/matched, not a resolved one.
	{Code: "match_exception", Name: "Match Exception", Translations: map[string]string{"ar": "استثناء المطابقة", "fa": "استثنای تطبیق", "tr": "Eşleşme İstisnası"}, Sequence: 3, IsInitial: false, IsTerminal: false},
	{Code: "paid", Name: "Paid", Translations: map[string]string{"ar": "مدفوعة", "fa": "پرداخت‌شده", "tr": "Ödendi"}, Sequence: 4, IsInitial: false, IsTerminal: true},
	// void's own word, not "cancelled"'s (ar ملغى/fa لغوشده/tr İptal
	// Edildi) — Void vs. Cancelled are different words in this kernel's
	// English (see VendorInvoice's own doc comment on why), and ar/fa's
	// own VendorInvoice.md help topics draw the identical distinction
	// (باطلة vs. ملغى; باطل‌شده vs. لغوشده). tr's own help topic does NOT
	// — it uses the bare word "İptal" for void, the same lemma
	// "cancelled" ships as "İptal Edildi" (tr/entity/VendorInvoice.md
	// alongside tr/entity/{PurchaseOrder,StockTransfer,
	// RequestForQuotation,SalesOrder}.md's identical prose shorthand for
	// their own "cancelled" codes) — reusing that word here would make
	// void and cancelled indistinguishable to a Turkish reader, the
	// opposite of what ar/fa's own distinct words accomplish. tr's void
	// gets its own distinct word instead, "Geçersiz Kılındı" ("rendered
	// void/invalid" — the ordinary Turkish term for a voided invoice,
	// matching this status set's own past-participle style, e.g.
	// "Onaylandı"/"Ödendi"), not sourced from the help topic's own
	// (ambiguous, on this one point) prose.
	{Code: "void", Name: "Void", Translations: map[string]string{"ar": "باطلة", "fa": "باطل‌شده", "tr": "Geçersiz Kılındı"}, Sequence: 5, IsInitial: false, IsTerminal: true},
}

var stockTransferStatusSpecs = []statusSpec{
	{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}, Sequence: 1, IsInitial: true, IsTerminal: false},
	{Code: "in_transit", Name: "In Transit", Translations: map[string]string{"ar": "قيد النقل", "fa": "در حال انتقال", "tr": "Yolda"}, Sequence: 2, IsInitial: false, IsTerminal: false},
	{Code: "received", Name: "Received", Translations: map[string]string{"ar": "مستلم", "fa": "دریافت‌شده", "tr": "Teslim Alındı"}, Sequence: 3, IsInitial: false, IsTerminal: true},
	{Code: "cancelled", Name: "Cancelled", Translations: map[string]string{"ar": "ملغى", "fa": "لغوشده", "tr": "İptal Edildi"}, Sequence: 4, IsInitial: false, IsTerminal: true},
}

var rfqStatusSpecs = []statusSpec{
	{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}, Sequence: 1, IsInitial: true, IsTerminal: false},
	{Code: "sent", Name: "Sent", Translations: map[string]string{"ar": "مُرسَل", "fa": "ارسال‌شده", "tr": "Gönderildi"}, Sequence: 2, IsInitial: false, IsTerminal: false},
	{Code: "quotes_received", Name: "Quotes Received", Translations: map[string]string{"ar": "تم استلام العروض", "fa": "قیمت‌های دریافت‌شده", "tr": "Teklifler Alındı"}, Sequence: 3, IsInitial: false, IsTerminal: false},
	{Code: "closed", Name: "Closed", Translations: map[string]string{"ar": "مغلق", "fa": "بسته‌شده", "tr": "Kapatıldı"}, Sequence: 4, IsInitial: false, IsTerminal: true},
	{Code: "cancelled", Name: "Cancelled", Translations: map[string]string{"ar": "ملغى", "fa": "لغوشده", "tr": "İptal Edildi"}, Sequence: 5, IsInitial: false, IsTerminal: true},
}

// StatusSpecs exposes this module's Status Specs (the identical content
// PublishStatuses passes to statusgraph.Seed, including Translations),
// keyed by StatusTypeCode — a fresh statusgraph.CopySpecs copy every
// call, safe for a caller to mutate without corrupting this package's
// own seed data. cmd/backfill-status-name-translations (uc-infra#244)
// uses this to look up (status_type_code, code) -> Translations for an
// already-provisioned tenant's existing Status rows without re-running
// Publish/PublishForms/PublishStatuses.
func StatusSpecs() map[string][]statusgraph.Spec {
	return map[string][]statusgraph.Spec{
		"purchase_order_status": statusgraph.CopySpecs(purchaseOrderStatusSpecs),
		"vendor_invoice_status": statusgraph.CopySpecs(vendorInvoiceStatusSpecs),
		"stock_transfer_status": statusgraph.CopySpecs(stockTransferStatusSpecs),
		"rfq_status":            statusgraph.CopySpecs(rfqStatusSpecs),
	}
}

// PublishStatuses seeds the actual StatusType/Status/StatusTransition
// *records* PurchaseOrder's StatusTypeCode ("purchase_order_status") and
// VendorInvoice's StatusTypeCode ("vendor_invoice_status") need to
// resolve before crud.Engine.ValidateStatusTransition stops rejecting
// every create/update of either with "status type ... is not published
// for this tenant". foundation.go's StatusType/Status/StatusTransition
// are ordinary entity Definitions — real records via crud.Engine, not a
// bespoke table (see that package's doc comment) — so unlike
// Publish/PublishForms above (registry metadata, identical for every
// tenant) this is genuine tenant data, and unlike cmd/seed-demo-data's
// sample business data (optional, safe to skip) it's required module
// setup: no tenant can create a real PurchaseOrder or VendorInvoice
// through internal/api without it.
//
// Looks StatusType/Status/StatusTransition up through the registry
// (data.EntityDefinitionRepo.GetPublished), the same as
// cmd/seed-demo-data's seeder.def helper, rather than importing
// foundation.StatusType()/Status()/StatusTransition() directly —
// purchasing has no Go-level dependency on foundation (PurchaseOrder's
// own doc comment: the dependency is "foundation published for this
// tenant" at runtime, resolved through the registry, same as every
// other cross-module reference field in this kernel). Requires
// foundation to already be published in this database (ADR-0001 §8;
// cmd/provision-tenant always publishes foundation before any module).
//
// purchase_order_status: draft is the only is_initial status; draft->
// submitted->approved->received is the happy path; cancellation is
// reachable from draft, submitted, or approved but not from received
// (goods already arrived — reversing that is a return/credit-note
// event, a different real-world action, not a status edit) or cancelled
// itself. received and cancelled are both is_terminal.
//
// vendor_invoice_status (VendorInvoice's own doc comment): draft is the
// only is_initial status; draft->matched->paid is the happy path — the
// "matched" transition (and any other Update that resolves to "matched")
// is where purchasing.MatchVendorInvoiceOnUpdate (ledger.go) runs the
// 3-way match. A disagreement no longer rejects the transition outright:
// the hook redirects the write to "match_exception" instead (with a
// reason recorded on the record itself) and returns to "matched" only
// once match_exception->matched is retried and agrees. match_exception
// has no edge to "paid" — that missing edge, not a separate check, is
// what blocks payment release until the exception is resolved. This
// block is enforced by crud.Engine.ValidateStatusTransition, which is
// only ever called from internal/api's HTTP handlers (not from
// crud.Engine.Update itself) — a pre-existing property of this whole
// mechanism, not something this task changes, but worth naming here
// because this task is the first place a missing StatusTransition edge
// is actually load-bearing for money leaving: any caller that reaches
// crud.Engine.Update directly (a kernel-internal caller, a script) is
// not stopped by this graph the way a browser/API client is.
// match_exception->void lets an exception be abandoned the same way
// draft/matched can be voided. void is reachable from draft, matched, or
// match_exception but not paid, same "money already moved, that's a
// credit note/reversal event not a status edit" reasoning as
// sales.PublishStatuses gives for CustomerInvoice's identical shape.
//
// A tenant provisioned before match_exception existed has no such Status
// seeded yet — MatchVendorInvoiceOnUpdate's redirect would then fail to
// resolve it and roll back the whole Update (a worse outcome than this
// hook's original fail-closed rejection, an independent review's own
// finding). This function is idempotent and additive (Idempotent, below)
// — re-running it against an already-provisioned tenant is always safe
// and is what actually picks up match_exception for that tenant; there
// is no separate migration/backfill step, unlike PurchaseOrder's
// status->status_id Version bump (this file's own doc comment on that),
// because nothing here replaces an existing field's meaning, it only
// adds a new reachable Status.
//
// rfq_status (RequestForQuotation's own doc comment, #9): draft is the
// only is_initial status — an RFQ being drafted, vendors/lines still
// being assembled. draft->sent is "the RFQ was actually sent to its
// invited vendors" (a real-world action, not a data edit — nothing else
// in this Definition forces vendors/lines to be non-empty at that
// point, same "declared, not schema-enforced" trust this kernel already
// places in a form submission generally). sent->quotes_received is "at
// least one vendor has responded and staff started recording quote
// lines" — deliberately not auto-derived from RequestForQuotationQuoteLine
// rows existing (this package's generic engine has no cross-record
// "does a child exist" trigger, and forcing one in here would be exactly
// the kind of entity-specific machinery CLAUDE.md's kernel-boundary rule
// warns against outside a Definition); a human/staff action moves it,
// same as every other status transition in this kernel.
// quotes_received->closed is "a vendor was chosen (or the RFQ abandoned
// with an answer in hand) and comparison is done" — deliberately NOT
// "converts to a PurchaseOrder": that conversion is explicit non-goal
// scope for #9 (a future task's job), so "closed" only means "this RFQ's
// own lifecycle is finished," not "a PO now exists." cancellation is
// reachable from draft, sent, or quotes_received but not from closed or
// cancelled itself — once closed, reopening/cancelling is a new RFQ, the
// same "the real-world event has already happened, that's not a status
// edit" reasoning purchase_order_status gives for excluding "received".
// closed and cancelled are both is_terminal.
//
// stock_transfer_status (StockTransfer's own doc comment, #13): draft is
// the only is_initial status — the transfer is recorded but stock hasn't
// moved yet. draft->in_transit is stock physically leaving the source
// facility; in_transit->received is it physically arriving at the
// destination — the happy path, and the whole reason this is a status
// graph rather than a quantity edit (the in-transit state has no other
// way to exist). received is is_terminal with no outbound edge, same
// "the real-world event already happened, that's not a status edit"
// reasoning purchase_order_status gives for excluding edges out of its
// own "received": a transfer that has arrived doesn't un-arrive by
// editing a field; correcting it is a new, reversing transfer. cancelled
// is reachable from draft or in_transit (a transfer can be called off
// before or during transit) but not from received or cancelled itself,
// and is itself is_terminal.
//
// Idempotent: every StatusType/Status looked up by its code, every
// StatusTransition by its from_status_id/to_status_id pair, same
// getOrCreate-by-natural-key shape cmd/seed-demo-data's seeder already
// uses — safe to call on a tenant that's already been seeded (a repeat
// module publish, say).
func PublishStatuses(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)

	def := func(entityType string) (*entity.Definition, error) {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			return nil, fmt.Errorf("look up published %s: %w", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			return nil, fmt.Errorf("unmarshal %s definition: %w", entityType, err)
		}
		return d, nil
	}

	statusTypeDef, err := def("StatusType")
	if err != nil {
		return err
	}
	statusDef, err := def("Status")
	if err != nil {
		return err
	}
	transitionDef, err := def("StatusTransition")
	if err != nil {
		return err
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"PurchaseOrder", "purchase_order_status", "Purchase Order Status",
		purchaseOrderStatusSpecs,
		[][2]string{
			{"draft", "submitted"},
			{"submitted", "approved"},
			{"approved", "received"},
			{"draft", "cancelled"},
			{"submitted", "cancelled"},
			{"approved", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed purchase_order_status: %w", err)
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"VendorInvoice", "vendor_invoice_status", "Vendor Invoice Status",
		vendorInvoiceStatusSpecs,
		[][2]string{
			{"draft", "matched"},
			{"matched", "paid"},
			{"draft", "void"},
			{"matched", "void"},
			// draft->match_exception and matched->match_exception:
			// system-driven, not client-requested — a caller always asks
			// for "matched" (the draft->matched or matched->matched edge
			// above already covers what it requested), and
			// MatchVendorInvoiceOnUpdate is the one that redirects the
			// write to match_exception instead when the 3-way match
			// disagrees. Declared here anyway, even though
			// crud.Engine.ValidateStatusTransition is never actually
			// asked to validate this specific edge (the hook writes
			// through records.UpdateTx directly, past that check) — a
			// StatusTransition graph that only lists client-requestable
			// edges would silently lie to anything that reads it to show
			// "what can this record become" (a future allowed-next-
			// statuses UI, an audit report), since match_exception is a
			// real, routinely-reached state.
			{"draft", "match_exception"},
			{"matched", "match_exception"},
			// match_exception->matched: retry after the underlying data
			// (invoice or the PO/GoodsReceipt it's matched against) is
			// corrected — MatchVendorInvoiceOnUpdate re-runs the same
			// match and, if it now agrees, this is the edge it lands on.
			{"match_exception", "matched"},
			// match_exception->void: abandon a bad invoice instead of
			// fixing it. Deliberately NO match_exception->paid edge —
			// that missing edge is the actual payment-release block, see
			// the doc comment above.
			{"match_exception", "void"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed vendor_invoice_status: %w", err)
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"StockTransfer", "stock_transfer_status", "Stock Transfer Status",
		stockTransferStatusSpecs,
		[][2]string{
			{"draft", "in_transit"},
			{"in_transit", "received"},
			{"draft", "cancelled"},
			{"in_transit", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed stock_transfer_status: %w", err)
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"RequestForQuotation", "rfq_status", "RFQ Status",
		rfqStatusSpecs,
		[][2]string{
			{"draft", "sent"},
			{"sent", "quotes_received"},
			{"quotes_received", "closed"},
			{"draft", "cancelled"},
			{"sent", "cancelled"},
			{"quotes_received", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed rfq_status: %w", err)
	}

	return nil
}
