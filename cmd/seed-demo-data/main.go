// Command seed-demo-data populates a tenant with a small, realistic
// sample dataset — the "Demo Organization" tenant's actual data, so
// logging in shows real vendors/customers/items/purchase orders instead
// of an empty app. Idempotent by design (see seeder.getOrCreate): safe
// to re-run after `cmd/provision-tenant` publishes a new module, and
// meant to be extended, not replaced, as new modules land — add a new
// seedX method here alongside whatever new module/entity introduced it,
// the same "grow it, don't rewrite it" discipline cmd/provision-tenant's
// own modulePublishers map already follows.
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// DATABASE_URL is the control-plane database (ADR-0003) — -tenant-id is
// resolved to that tenant's own database via internal/tenantdb.Router,
// the same pattern cmd/provision-tenant uses.
func main() {
	controlDBURL := os.Getenv("DATABASE_URL")
	if controlDBURL == "" {
		log.Fatal("DATABASE_URL is required (the control-plane database — see this file's own doc comment)")
	}
	tenantID := flag.String("tenant-id", "", "tenant to seed sample data into (required)")
	actorID := flag.String("actor-id", "", "audit actor id for every record this creates (required)")
	flag.Parse()
	if *tenantID == "" {
		log.Fatal("-tenant-id is required")
	}
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}

	controlDB, err := sql.Open("pgx", controlDBURL)
	if err != nil {
		log.Fatalf("open control database: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.Ping(); err != nil {
		log.Fatalf("ping control database: %v", err)
	}

	router, err := tenantdb.NewRouter(controlDB, controlDBURL)
	if err != nil {
		log.Fatalf("build tenant router: %v", err)
	}
	sqlDB, err := router.Get(context.Background(), *tenantID)
	if err != nil {
		log.Fatalf("resolve tenant %s database: %v", *tenantID, err)
	}

	s := &seeder{
		ctx:        context.Background(),
		actor:      audit.Actor{Type: audit.ActorHuman, ID: *actorID},
		entityDefs: data.NewEntityDefinitionRepo(sqlDB),
		crud:       crud.NewEngine(sqlDB),
		defs:       map[string]*entity.Definition{},
	}

	// PurchaseOrder.status_id needs purchase_order_status's StatusType/
	// Status/StatusTransition rows to already exist for this tenant —
	// normally cmd/provision-tenant's job (required module setup, not
	// sample data — see purchasing.PublishStatuses's doc comment), but
	// idempotent and cheap enough to also run here so this command stays
	// self-sufficient against a tenant provisioned before this seeder
	// grew a PurchaseOrder step that depends on it.
	if err := purchasing.PublishStatuses(context.Background(), sqlDB, s.actor); err != nil {
		log.Fatalf("publish purchase_order_status: %v", err)
	}

	currencies := s.seedCurrencies()
	uoms := s.seedUnitsOfMeasure()
	vendors, customers := s.seedParties()
	items := s.seedItems(uoms)
	s.seedInventory(items)
	s.seedPurchaseOrders(vendors, currencies, items)
	s.seedGoodsReceipts()

	log.Printf("demo data seeded for tenant %s (%d currencies, %d units, %d vendors, %d customers, %d items)",
		*tenantID, len(currencies), len(uoms), len(vendors), len(customers), len(items))
}

type seeder struct {
	ctx        context.Context
	actor      audit.Actor
	entityDefs *data.EntityDefinitionRepo
	crud       *crud.Engine
	defs       map[string]*entity.Definition // cached per entity type, this run
}

func (s *seeder) def(entityType string) *entity.Definition {
	if d, ok := s.defs[entityType]; ok {
		return d
	}
	v, err := s.entityDefs.GetPublished(s.ctx, entityType)
	if err != nil {
		log.Fatalf("look up published %s: %v (has this module been provisioned for this tenant? see cmd/provision-tenant)", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		log.Fatalf("unmarshal %s definition: %v", entityType, err)
	}
	s.defs[entityType] = d
	return d
}

// getOrCreate finds an existing record of entityType whose keyField
// equals keyValue and returns its id, or creates one from fields (which
// must itself include keyField: keyValue) and returns the new id — the
// idempotency that makes re-running this command safe (a re-run after a
// new module adds more seedX calls shouldn't duplicate everything
// already seeded). Only practical for entities with a real natural key
// (code, sku, name); join-like entities without one use their own
// narrower dedup logic (see seedPurchaseOrders' doc comment).
func (s *seeder) getOrCreate(entityType, keyField, keyValue string, fields map[string]any) string {
	def := s.def(entityType)
	existing, err := s.crud.ListByField(s.ctx, def, keyField, keyValue)
	if err != nil {
		log.Fatalf("list %s by %s: %v", entityType, keyField, err)
	}
	if len(existing) > 0 {
		return existing[0].ID
	}
	rec, err := s.crud.Create(s.ctx, def, fields, s.actor)
	if err != nil {
		log.Fatalf("create %s %v: %v", entityType, fields, err)
	}
	return rec.ID
}

func (s *seeder) seedCurrencies() map[string]string {
	ids := map[string]string{}
	for _, c := range []struct{ code, name string }{
		{"USD", "US Dollar"},
		{"GBP", "British Pound"},
		{"QAR", "Qatari Riyal"},
		{"TRY", "Turkish Lira"},
	} {
		ids[c.code] = s.getOrCreate("Currency", "code", c.code, map[string]any{"code": c.code, "name": c.name})
	}
	return ids
}

func (s *seeder) seedUnitsOfMeasure() map[string]string {
	ids := map[string]string{}
	for _, u := range []struct{ code, name string }{
		{"EA", "Each"},
		{"BOX", "Box"},
		{"KG", "Kilogram"},
	} {
		ids[u.code] = s.getOrCreate("UnitOfMeasure", "code", u.code, map[string]any{"code": u.code, "name": u.name})
	}
	return ids
}

// seedParties creates both vendors and customers, tagging each with a
// PartyRole row — the reference-data-model.md Party-Role pattern this
// kernel is built around, so the sample data actually demonstrates it
// instead of leaving PartyRole empty. Names lean into the UK+GCC+Turkey
// launch markets (BACKLOG.md's R1) rather than generic placeholders.
func (s *seeder) seedParties() (vendors, customers map[string]string) {
	roleDef := s.def("PartyRole")

	seedRole := func(partyID, roleType string) {
		existing, err := s.crud.ListByField(s.ctx, roleDef, "party_id", partyID)
		if err != nil {
			log.Fatalf("list PartyRole by party_id: %v", err)
		}
		for _, r := range existing {
			if r.Data["role_type"] == roleType {
				return
			}
		}
		if _, err := s.crud.Create(s.ctx, roleDef, map[string]any{
			"party_id": partyID, "role_type": roleType,
		}, s.actor); err != nil {
			log.Fatalf("create PartyRole: %v", err)
		}
	}

	vendors = map[string]string{}
	for _, name := range []string{
		"Acme Textiles", "Gulf Steel Supply", "Anatolia Parts Co.",
		"Doha Fasteners LLC", "Istanbul Weaving Mills", "Manchester Packaging Ltd",
	} {
		id := s.getOrCreate("Party", "name", name, map[string]any{
			"party_type": "organization", "name": name, "status": "active",
		})
		seedRole(id, "vendor")
		vendors[name] = id
	}

	customers = map[string]string{}
	for _, name := range []string{"Doha Retail Group", "London Fashion House"} {
		id := s.getOrCreate("Party", "name", name, map[string]any{
			"party_type": "organization", "name": name, "status": "active",
		})
		seedRole(id, "customer")
		customers[name] = id
	}
	return vendors, customers
}

func (s *seeder) seedItems(uoms map[string]string) map[string]string {
	ids := map[string]string{}
	for _, it := range []struct{ sku, name, itemType, uom string }{
		{"SKU-1001", "Steel Bolt 10mm", "stock", "EA"},
		{"SKU-1002", "Cotton Fabric Roll", "stock", "BOX"},
		{"SKU-1003", "Packaging Material", "stock", "KG"},
		{"SKU-1004", "Steel Bolt 12mm", "stock", "EA"},
		{"SKU-1005", "Denim Fabric Roll", "stock", "BOX"},
		{"SKU-1006", "Zipper Pack (100ct)", "stock", "BOX"},
		{"SKU-1007", "Corrugated Box, Medium", "stock", "EA"},
		{"SKU-1008", "Stretch Wrap Film", "stock", "KG"},
		{"SKU-1009", "Steel Washer 10mm", "stock", "EA"},
		{"SKU-1010", "Wool Fabric Roll", "stock", "BOX"},
		{"SKU-2001", "Installation Consulting", "service", "EA"},
		{"SKU-2002", "Fabric Quality Inspection", "service", "EA"},
	} {
		fields := map[string]any{"sku": it.sku, "name": it.name, "item_type": it.itemType}
		if uomID, ok := uoms[it.uom]; ok {
			fields["base_uom_id"] = uomID
		}
		ids[it.sku] = s.getOrCreate("Item", "sku", it.sku, fields)
	}
	return ids
}

// seedInventory gives every stock Item an on-hand + available-to-promise
// level — service items (no natural inventory concept) are deliberately
// skipped, same as InventoryItem's own doc comment describes the
// entity's scope. onHand and availableToPromise deliberately differ for
// a few SKUs (fully or over-committed against existing sales
// allocations, not modeled by this simplified kernel — see
// InventoryItem's own doc comment) so the mgmt reporting workbench's
// stockout-risk table (internal/api/reporting.go, qty_available_to_promise
// <= 0) has real rows to show, not just a demo of an empty state.
func (s *seeder) seedInventory(items map[string]string) {
	def := s.def("InventoryItem")
	levels := map[string]struct{ onHand, atp float64 }{
		"SKU-1001": {500, 500},
		"SKU-1002": {120, 120},
		"SKU-1003": {300, 300},
		"SKU-1004": {80, 0}, // fully committed — stockout risk
		"SKU-1005": {45, 45},
		"SKU-1006": {600, 600},
		"SKU-1007": {25, -10}, // over-committed — stockout risk
		"SKU-1008": {200, 200},
		"SKU-1009": {0, 0}, // never restocked — stockout risk
		"SKU-1010": {60, 60},
	}
	for sku, itemID := range items {
		level, ok := levels[sku]
		if !ok {
			continue
		}
		existing, err := s.crud.ListByField(s.ctx, def, "item_id", itemID)
		if err != nil {
			log.Fatalf("list InventoryItem by item_id: %v", err)
		}
		if len(existing) > 0 {
			continue
		}
		if _, err := s.crud.Create(s.ctx, def, map[string]any{
			"item_id": itemID, "qty_on_hand": level.onHand, "qty_available_to_promise": level.atp,
		}, s.actor); err != nil {
			log.Fatalf("create InventoryItem: %v", err)
		}
	}
}

// seedPurchaseOrders dedups on po_number, the same getOrCreate-style
// natural-key pattern used everywhere else in this seeder — now that
// PurchaseOrder actually has one (BACKLOG.md/QUEUE.md, 2026-07-21; it
// didn't when this function was first written, hence the coarser
// "skip entirely if this tenant already has any PurchaseOrder" guard
// this replaces). Unlike getOrCreate itself, this can't just call it
// directly: creating POLines needs the parent's id first, and total is
// only known after the lines exist, so each order still needs its own
// create-then-update sequence — only the dedup check is shared.
// statusID looks up a Status record by its code — purchasing.
// PublishStatuses (called in main before any seeding) already seeded
// these. Only one StatusType exists today (purchase_order_status), so a
// plain code lookup is unambiguous; see that function's doc comment if a
// second StatusType ever makes this need scoping by status_type_id too.
// A seeder method (not a local closure inside seedPurchaseOrders) since
// seedGoodsReceipts needs the exact same lookup to find already-
// "received" orders.
func (s *seeder) statusID(code string) string {
	statusDef := s.def("Status")
	recs, err := s.crud.ListByField(s.ctx, statusDef, "code", code)
	if err != nil {
		log.Fatalf("list Status by code %q: %v", code, err)
	}
	if len(recs) == 0 {
		log.Fatalf("no Status record for code %q (was purchasing.PublishStatuses run?)", code)
	}
	return recs[0].ID
}

func (s *seeder) seedPurchaseOrders(vendors, currencies, items map[string]string) {
	poDef := s.def("PurchaseOrder")
	lineDef := s.def("POLine")

	type line struct {
		sku      string
		qty      float64
		unitCost float64
	}
	orders := []struct {
		poNumber string
		vendor   string
		currency string
		date     string
		status   string
		lines    []line
	}{
		{"PO-2026-0001", "Acme Textiles", "USD", "2026-07-01", "approved", []line{{"SKU-1002", 40, 18.5}}},
		{"PO-2026-0002", "Gulf Steel Supply", "QAR", "2026-07-10", "submitted", []line{{"SKU-1001", 2000, 0.35}}},
		{"PO-2026-0003", "Anatolia Parts Co.", "TRY", "2026-07-15", "draft", []line{{"SKU-1003", 150, 4.2}, {"SKU-2001", 8, 120}}},
		{"PO-2026-0004", "Acme Textiles", "USD", "2026-07-18", "received", []line{{"SKU-1005", 60, 22}, {"SKU-1006", 30, 9.5}}},
		{"PO-2026-0005", "Doha Fasteners LLC", "QAR", "2026-07-19", "received", []line{{"SKU-1004", 3000, 0.42}, {"SKU-1009", 5000, 0.08}}},
		{"PO-2026-0006", "Istanbul Weaving Mills", "TRY", "2026-07-19", "approved", []line{{"SKU-1010", 90, 26.5}}},
		{"PO-2026-0007", "Manchester Packaging Ltd", "GBP", "2026-07-20", "submitted", []line{{"SKU-1007", 400, 3.1}, {"SKU-1008", 250, 5.75}}},
		{"PO-2026-0008", "Gulf Steel Supply", "QAR", "2026-07-20", "cancelled", []line{{"SKU-1001", 500, 0.35}}},
		{"PO-2026-0009", "Anatolia Parts Co.", "TRY", "2026-07-21", "draft", []line{{"SKU-2002", 4, 90}}},
		{"PO-2026-0010", "Doha Fasteners LLC", "QAR", "2026-07-21", "approved", []line{{"SKU-1009", 8000, 0.08}}},
	}
	for _, o := range orders {
		existing, err := s.crud.ListByField(s.ctx, poDef, "po_number", o.poNumber)
		if err != nil {
			log.Fatalf("list PurchaseOrder by po_number: %v", err)
		}
		if len(existing) > 0 {
			continue
		}

		poID, err := s.crud.Create(s.ctx, poDef, map[string]any{
			"po_number":   o.poNumber,
			"vendor_id":   vendors[o.vendor],
			"currency_id": currencies[o.currency],
			"order_date":  o.date,
			"status_id":   s.statusID(o.status),
		}, s.actor)
		if err != nil {
			log.Fatalf("create PurchaseOrder for %s: %v", o.vendor, err)
		}
		var total float64
		for _, l := range o.lines {
			lineTotal := l.qty * l.unitCost
			total += lineTotal
			if _, err := s.crud.Create(s.ctx, lineDef, map[string]any{
				"purchase_order_id": poID.ID,
				"item_id":           items[l.sku],
				"qty":               l.qty,
				"unit_price":        l.unitCost,
				"line_total":        lineTotal,
			}, s.actor); err != nil {
				log.Fatalf("create POLine: %v", err)
			}
		}
		// Update takes a full replacement set of fields, not a partial
		// patch (entity.ValidateRecord runs against exactly what's
		// passed here) — po_number has to be repeated even though it's
		// unchanged, same as every other field already was.
		expectedVersion := poID.Version
		if _, err := s.crud.Update(s.ctx, poDef, poID.ID, map[string]any{
			"po_number": o.poNumber, "vendor_id": vendors[o.vendor], "currency_id": currencies[o.currency],
			"order_date": o.date, "status_id": s.statusID(o.status), "total": total,
		}, &expectedVersion, s.actor); err != nil {
			log.Fatalf("update PurchaseOrder total: %v", err)
		}
	}
}

// seedGoodsReceipts gives every already-"received" PurchaseOrder
// (PO-2026-0004, PO-2026-0005 in seedPurchaseOrders' own table above) a
// real GoodsReceipt + one GoodsReceiptLine per POLine, received in full
// 5 days after the order date — sample data that actually demonstrates
// the entity purchasing.GoodsReceipt exists for, the same reasoning
// seedParties gives for tagging PartyRole rather than leaving it empty.
// Deliberately driven by querying PurchaseOrder's own status_id (not a
// second hardcoded po_number list that could silently drift from
// seedPurchaseOrders' table) — dedups on purchase_order_id, since
// GoodsReceipt has no natural key of its own to getOrCreate against.
// That dedup is existence-based, not completeness-based: if this
// process died partway through a PO's lines (after creating the
// GoodsReceipt but before all its GoodsReceiptLines), a re-run would
// see the GoodsReceipt already exists and skip it rather than finishing
// the missing lines. Accepted for demo-data tooling — no transaction
// wraps this loop, same as every other seedX method in this file — but
// worth knowing before assuming a re-run always "heals" a partial state.
func (s *seeder) seedGoodsReceipts() {
	poDef := s.def("PurchaseOrder")
	lineDef := s.def("POLine")
	grDef := s.def("GoodsReceipt")
	grLineDef := s.def("GoodsReceiptLine")

	received, err := s.crud.ListByField(s.ctx, poDef, "status_id", s.statusID("received"))
	if err != nil {
		log.Fatalf("list received PurchaseOrders: %v", err)
	}
	for _, po := range received {
		existing, err := s.crud.ListByField(s.ctx, grDef, "purchase_order_id", po.ID)
		if err != nil {
			log.Fatalf("list GoodsReceipt by purchase_order_id: %v", err)
		}
		if len(existing) > 0 {
			continue
		}

		orderDate, _ := po.Data["order_date"].(string)
		receivedDate := orderDate
		if t, err := time.Parse("2006-01-02", orderDate); err == nil {
			receivedDate = t.AddDate(0, 0, 5).Format("2006-01-02")
		}
		gr, err := s.crud.Create(s.ctx, grDef, map[string]any{
			"purchase_order_id": po.ID,
			"received_date":     receivedDate,
			"notes":             "Received in full",
		}, s.actor)
		if err != nil {
			log.Fatalf("create GoodsReceipt for PO %s: %v", po.ID, err)
		}

		lines, err := s.crud.ListByField(s.ctx, lineDef, "purchase_order_id", po.ID)
		if err != nil {
			log.Fatalf("list POLine by purchase_order_id: %v", err)
		}
		for _, line := range lines {
			if _, err := s.crud.Create(s.ctx, grLineDef, map[string]any{
				"goods_receipt_id": gr.ID,
				"po_line_id":       line.ID,
				"item_id":          line.Data["item_id"],
				"qty_received":     line.Data["qty"],
			}, s.actor); err != nil {
				log.Fatalf("create GoodsReceiptLine for POLine %s: %v", line.ID, err)
			}
		}
	}
}
