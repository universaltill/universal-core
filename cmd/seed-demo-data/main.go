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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
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

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	s := &seeder{
		ctx:        context.Background(),
		tenantID:   *tenantID,
		actor:      audit.Actor{Type: audit.ActorHuman, ID: *actorID},
		entityDefs: data.NewEntityDefinitionRepo(sqlDB),
		crud:       crud.NewEngine(sqlDB),
		defs:       map[string]*entity.Definition{},
	}

	currencies := s.seedCurrencies()
	uoms := s.seedUnitsOfMeasure()
	vendors, customers := s.seedParties()
	items := s.seedItems(uoms)
	s.seedInventory(items)
	s.seedPurchaseOrders(vendors, currencies, items)

	log.Printf("demo data seeded for tenant %s (%d currencies, %d units, %d vendors, %d customers, %d items)",
		*tenantID, len(currencies), len(uoms), len(vendors), len(customers), len(items))
}

type seeder struct {
	ctx        context.Context
	tenantID   string
	actor      audit.Actor
	entityDefs *data.EntityDefinitionRepo
	crud       *crud.Engine
	defs       map[string]*entity.Definition // cached per entity type, this run
}

func (s *seeder) def(entityType string) *entity.Definition {
	if d, ok := s.defs[entityType]; ok {
		return d
	}
	v, err := s.entityDefs.GetPublished(s.ctx, s.tenantID, entityType)
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
	existing, err := s.crud.ListByField(s.ctx, def, s.tenantID, keyField, keyValue)
	if err != nil {
		log.Fatalf("list %s by %s: %v", entityType, keyField, err)
	}
	if len(existing) > 0 {
		return existing[0].ID
	}
	rec, err := s.crud.Create(s.ctx, def, s.tenantID, fields, s.actor)
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
		existing, err := s.crud.ListByField(s.ctx, roleDef, s.tenantID, "party_id", partyID)
		if err != nil {
			log.Fatalf("list PartyRole by party_id: %v", err)
		}
		for _, r := range existing {
			if r.Data["role_type"] == roleType {
				return
			}
		}
		if _, err := s.crud.Create(s.ctx, roleDef, s.tenantID, map[string]any{
			"party_id": partyID, "role_type": roleType,
		}, s.actor); err != nil {
			log.Fatalf("create PartyRole: %v", err)
		}
	}

	vendors = map[string]string{}
	for _, name := range []string{"Acme Textiles", "Gulf Steel Supply", "Anatolia Parts Co."} {
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
		{"SKU-2001", "Installation Consulting", "service", "EA"},
	} {
		fields := map[string]any{"sku": it.sku, "name": it.name, "item_type": it.itemType}
		if uomID, ok := uoms[it.uom]; ok {
			fields["base_uom_id"] = uomID
		}
		ids[it.sku] = s.getOrCreate("Item", "sku", it.sku, fields)
	}
	return ids
}

// seedInventory gives every stock Item an on-hand quantity — service
// items (no natural inventory concept) are deliberately skipped, same
// as InventoryItem's own doc comment describes the entity's scope.
func (s *seeder) seedInventory(items map[string]string) {
	def := s.def("InventoryItem")
	levels := map[string]float64{"SKU-1001": 500, "SKU-1002": 120, "SKU-1003": 300}
	for sku, itemID := range items {
		qty, ok := levels[sku]
		if !ok {
			continue
		}
		existing, err := s.crud.ListByField(s.ctx, def, s.tenantID, "item_id", itemID)
		if err != nil {
			log.Fatalf("list InventoryItem by item_id: %v", err)
		}
		if len(existing) > 0 {
			continue
		}
		if _, err := s.crud.Create(s.ctx, def, s.tenantID, map[string]any{
			"item_id": itemID, "qty_on_hand": qty, "qty_available_to_promise": qty,
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
	}
	for _, o := range orders {
		existing, err := s.crud.ListByField(s.ctx, poDef, s.tenantID, "po_number", o.poNumber)
		if err != nil {
			log.Fatalf("list PurchaseOrder by po_number: %v", err)
		}
		if len(existing) > 0 {
			continue
		}

		poID, err := s.crud.Create(s.ctx, poDef, s.tenantID, map[string]any{
			"po_number":   o.poNumber,
			"vendor_id":   vendors[o.vendor],
			"currency_id": currencies[o.currency],
			"order_date":  o.date,
			"status":      o.status,
		}, s.actor)
		if err != nil {
			log.Fatalf("create PurchaseOrder for %s: %v", o.vendor, err)
		}
		var total float64
		for _, l := range o.lines {
			lineTotal := l.qty * l.unitCost
			total += lineTotal
			if _, err := s.crud.Create(s.ctx, lineDef, s.tenantID, map[string]any{
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
		if _, err := s.crud.Update(s.ctx, poDef, s.tenantID, poID.ID, map[string]any{
			"po_number": o.poNumber, "vendor_id": vendors[o.vendor], "currency_id": currencies[o.currency],
			"order_date": o.date, "status": o.status, "total": total,
		}, &expectedVersion, s.actor); err != nil {
			log.Fatalf("update PurchaseOrder total: %v", err)
		}
	}
}
