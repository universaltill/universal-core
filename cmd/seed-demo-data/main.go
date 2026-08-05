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
	"maps"
	"math"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
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
	// An unattended pipeline seeding run is an ai_agent actor, and
	// ADR-0001 §14 makes that distinction first-class — hard-coding
	// ActorHuman would write a falsified actor_type onto every record
	// this command creates (uc-infra#72, same shape as
	// cmd/install-module's fix).
	actorType := flag.String("actor-type", string(audit.ActorHuman), "audit actor type: human | ai_agent")
	modelVersion := flag.String("model-version", "", "model version, required when -actor-type is ai_agent")
	flag.Parse()
	if *tenantID == "" {
		log.Fatal("-tenant-id is required")
	}
	if *actorID == "" {
		log.Fatal("-actor-id is required")
	}
	// Resolved and checked before any database work: an operator who
	// mistyped the actor should learn that immediately, not after the
	// control-plane connection already ran (same discipline as
	// cmd/install-module).
	actor := audit.Actor{Type: audit.ActorType(*actorType), ID: *actorID, ModelVersion: *modelVersion}
	switch actor.Type {
	case audit.ActorHuman, audit.ActorAgent:
	default:
		log.Fatalf("invalid actor: -actor-type must be %q or %q, got %q", audit.ActorHuman, audit.ActorAgent, *actorType)
	}
	// A human actor carrying a model version is the same class of
	// falsified audit metadata this fix exists to prevent the other
	// way around (uc-infra#72 independent review) — Validate() alone
	// only rejects an EMPTY ModelVersion on an agent, never a populated
	// one on a human, so that half of the mistake needs its own check.
	if actor.Type == audit.ActorHuman && *modelVersion != "" {
		log.Fatalf("invalid actor: -model-version is only meaningful when -actor-type is %q", audit.ActorAgent)
	}
	if err := actor.Validate(); err != nil {
		log.Fatalf("invalid actor: %v", err)
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

	engine := crud.NewEngine(sqlDB)
	// Same composition-root wiring internal/api/handlers.go's scope()
	// does for real requests — sample GoodsReceiptLines/CustomerInvoices
	// created below post to the ledger through the exact same path a
	// real user's create/update would, proving the hooks work end to
	// end against real seed data, not just unit tests.
	engine.SetHook("GoodsReceiptLine", purchasing.PostGoodsReceiptLineToLedger)
	engine.SetHook("CustomerInvoice", sales.PostCustomerInvoiceToLedger)
	engine.SetHook("VendorInvoice", purchasing.MatchVendorInvoiceOnUpdate)
	engine.SetHook("StockTransfer", purchasing.ValidateStockTransfer)

	s := &seeder{
		ctx:        context.Background(),
		actor:      actor,
		entityDefs: data.NewEntityDefinitionRepo(sqlDB),
		crud:       engine,
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
	// Same reasoning as the purchasing.PublishStatuses call above, now
	// covering sales_order_status/customer_invoice_status — cheap and
	// idempotent enough to also run here so this command stays
	// self-sufficient against a tenant provisioned before Sales existed.
	if err := sales.PublishStatuses(context.Background(), sqlDB, s.actor); err != nil {
		log.Fatalf("publish sales statuses: %v", err)
	}
	// Assets: statuses ONLY, exactly like purchasing/sales above. An
	// earlier draft also called assets.Publish/PublishForms here, which
	// would have installed an entire module into tenants that never
	// licensed it — the seeder seeds data, cmd/provision-tenant decides
	// entitlement (independent review). A tenant without the module
	// simply has no FixedAsset Definition, and seedFixedAssets skips.
	if hasPublished(context.Background(), s.entityDefs, "FixedAsset") {
		if err := assets.PublishStatuses(context.Background(), sqlDB, s.actor); err != nil {
			log.Fatalf("publish assets statuses: %v", err)
		}
	}

	currencies := s.seedCurrencies()
	uoms := s.seedUnitsOfMeasure()
	accounts := s.seedFinance()
	s.seedRoles()
	s.seedOrgChart()
	// gl_accounts (the ledger core's own typed chart, ADR-0004) is a
	// projection of the finance.Account records seedFinance just
	// created/confirmed — sync it now so cmd/backfill-... or any future
	// posting code has a real, matching gl_accounts row to post against
	// immediately, not only after some separate manual step.
	if err := finance.SyncGLAccounts(context.Background(), sqlDB, s.actor); err != nil {
		log.Fatalf("sync gl_accounts: %v", err)
	}
	vendors, customers := s.seedParties()
	items := s.seedItems(uoms)
	// Gated like every other post-provisioning entity: a tenant
	// provisioned before #12 has purchasing published but no Facility
	// (the #70 gap), and s.def log.Fatalfs on an unpublished type — so
	// without this the seeder died AFTER creating Items and BEFORE
	// purchase orders, sales orders and everything downstream, leaving a
	// half-seeded tenant (independent review). Falling back to the
	// pre-#12 single-row shape keeps such a tenant seeding correctly
	// rather than skipping inventory and gutting the reporting demo.
	if hasPublished(s.ctx, s.entityDefs, "Facility") {
		facilities := s.seedFacilities()
		s.seedInventory(items, facilities)
		// Gated separately from Facility, not folded into the check
		// above: a tenant provisioned before #13 has Facility published
		// and StockTransfer not (#70), and seeding into an unpublished
		// type is a log.Fatalf, not a skip — the same separate-gate
		// shape Case/Opportunity below already uses.
		if hasPublished(s.ctx, s.entityDefs, "StockTransfer") {
			s.seedStockTransfers(items, facilities)
		}
	} else {
		log.Printf("Facility is not published for this tenant — seeding inventory without a location dimension (pre-#12 shape); re-run cmd/provision-tenant, then cmd/backfill-inventory-facility, to pick up multi-facility stock")
		s.seedInventoryWithoutFacilities(items)
	}
	s.seedReorderRules(items)
	s.seedPurchaseOrders(vendors, currencies, items)
	s.seedGoodsReceipts()
	s.seedVendorInvoices(vendors, currencies)
	soIDs := s.seedSalesOrders(customers, currencies, items)
	s.seedCustomerInvoices(customers, currencies, soIDs)
	if hasPublished(s.ctx, s.entityDefs, "FixedAsset") {
		s.seedFixedAssets(currencies, accounts, vendors)
	}
	if hasPublished(s.ctx, s.entityDefs, "Case") {
		if err := crm.PublishStatuses(context.Background(), sqlDB, s.actor); err != nil {
			log.Fatalf("publish crm statuses: %v", err)
		}
		s.seedCases(customers, items, soIDs)
		// Gated separately from Case, not folded into the check above:
		// a tenant provisioned before the pipeline entities shipped has
		// Case published and Opportunity not (#70), and seeding into an
		// unpublished type is a log.Fatalf, not a skip.
		if hasPublished(s.ctx, s.entityDefs, "Opportunity") {
			s.seedPipeline(customers, currencies)
		}
	}
	if hasPublished(s.ctx, s.entityDefs, "Employee") {
		if err := hr.PublishStatuses(context.Background(), sqlDB, s.actor); err != nil {
			log.Fatalf("publish hr statuses: %v", err)
		}
		s.seedHR()
	}
	if hasPublished(s.ctx, s.entityDefs, "Project") {
		if err := projects.PublishStatuses(context.Background(), sqlDB, s.actor); err != nil {
			log.Fatalf("publish projects statuses: %v", err)
		}
		s.seedProjects(currencies, customers)
	}

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

// hasPublished reports whether an entity type is published for this
// tenant — the "is this module licensed here" check the assets seeding
// is gated on. A lookup miss is a definitive no, not an error: an
// unlicensed module is the normal case for most tenants.
func hasPublished(ctx context.Context, repo *data.EntityDefinitionRepo, entityType string) bool {
	_, err := repo.GetPublished(ctx, entityType)
	return err == nil
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

// seedFinance seeds a small standard chart of accounts (5 top-level
// accounts + child accounts under each, demonstrating Account's
// parent_account_id self-reference), one FiscalYear with two Periods
// (one closed, one open — demonstrating the field meaningfully rather
// than leaving every seeded Period in its default state), and a handful
// of TaxCodes/CostCenters shaped for the UK+GCC launch markets (BACKLOG.md
// R1), same reasoning seedParties' own doc comment gives for its vendor/
// customer names.
func (s *seeder) seedFinance() map[string]string {
	accounts := []struct {
		code, name, accountType, parentCode string
	}{
		{"1000", "Assets", "asset", ""},
		{"1100", "Cash and Bank", "asset", "1000"},
		{"1200", "Accounts Receivable", "asset", "1000"},
		{"1300", "Inventory", "asset", "1000"},
		{"1400", "Fixed Assets", "asset", "1000"},
		// Accumulated depreciation is a contra-asset: it lives under
		// Assets and carries a credit balance that offsets 1400.
		{"1450", "Accumulated Depreciation", "asset", "1000"},
		{"2000", "Liabilities", "liability", ""},
		{"2100", "Accounts Payable", "liability", "2000"},
		{"3000", "Equity", "equity", ""},
		{"3100", "Retained Earnings", "equity", "3000"},
		{"4000", "Income", "income", ""},
		{"4100", "Sales Revenue", "income", "4000"},
		{"5000", "Expenses", "expense", ""},
		{"5100", "Cost of Goods Sold", "expense", "5000"},
		{"5200", "Operating Expenses", "expense", "5000"},
		{"5300", "Depreciation Expense", "expense", "5000"},
	}
	accountIDs := map[string]string{}
	for _, a := range accounts {
		fields := map[string]any{
			"code": a.code, "name": a.name, "type": a.accountType, "is_active": true,
		}
		if a.parentCode != "" {
			// Parents are seeded before children in the list above, so
			// accountIDs[a.parentCode] is always already populated here —
			// no second pass needed.
			fields["parent_account_id"] = accountIDs[a.parentCode]
		}
		accountIDs[a.code] = s.getOrCreate("Account", "code", a.code, fields)
	}

	fyID := s.getOrCreate("FiscalYear", "name", "FY2026", map[string]any{
		"name": "FY2026", "start_date": "2026-01-01", "end_date": "2026-12-31", "status": "open",
	})
	s.getOrCreate("Period", "name", "2026-01", map[string]any{
		"fiscal_year_id": fyID, "name": "2026-01",
		"start_date": "2026-01-01", "end_date": "2026-01-31", "status": "closed",
	})
	s.getOrCreate("Period", "name", "2026-02", map[string]any{
		"fiscal_year_id": fyID, "name": "2026-02",
		"start_date": "2026-02-01", "end_date": "2026-02-28", "status": "open",
	})

	for _, t := range []struct {
		code, name, taxType, jurisdiction string
		rate                              float64
	}{
		{"VAT5", "VAT Standard 5%", "vat", "QA", 5},
		{"VAT0", "VAT Zero-Rated", "vat", "QA", 0},
		{"VAT20", "UK VAT Standard 20%", "vat", "GB", 20},
	} {
		s.getOrCreate("TaxCode", "code", t.code, map[string]any{
			"code": t.code, "name": t.name, "rate": t.rate,
			"tax_type": t.taxType, "jurisdiction": t.jurisdiction,
		})
	}

	for _, c := range []struct{ code, name, ccType string }{
		{"CC-100", "Procurement", "operational"},
		{"CC-200", "Sales", "operational"},
		{"CC-900", "Head Office", "overhead"},
	} {
		s.getOrCreate("CostCenter", "code", c.code, map[string]any{
			"code": c.code, "name": c.name, "type": c.ccType,
		})
	}
	return accountIDs
}

// seedFixedAssets registers three representative assets and generates
// each one's real depreciation schedule through assets.Build — not a
// hand-written table of plausible-looking rows, so the demo tenant
// shows exactly what the shipped arithmetic produces, remainder
// distribution included (FA-1003's base does not divide evenly, and its
// term is short enough that the step-down is visible in the seeded
// rows).
func (s *seeder) seedFixedAssets(currencies, accounts, vendors map[string]string) {
	scheduleDef := s.def("DepreciationSchedule")

	// The currency's own minor_unit drives the conversion — FixedAsset
	// carries currency_id precisely so the scale is never guessed, and
	// an earlier draft hardcoded 2 anyway (independent review). The demo
	// chart seeds four currencies, so "it's all USD" was never true.
	usdID := currencies["USD"]
	scale := s.currencyMinorUnit(usdID)
	div := math.Pow(10, float64(scale))

	for _, a := range []struct {
		number, name, location, acquired string
		cost, salvage                    float64
		months                           int
	}{
		{"FA-1001", "Delivery Van", "Main Depot", "2026-01-15", 48000, 6000, 60},
		{"FA-1002", "Warehouse Racking", "Main Depot", "2026-02-01", 12500, 0, 120},
		// A term whose base does NOT divide evenly, short enough that the
		// step-down from 833.34 to 833.33 lands inside the seeded rows —
		// so the demo tenant actually exhibits the remainder distribution
		// rather than merely claiming to.
		{"FA-1003", "Forklift Battery", "Main Depot", "2026-03-01", 10000, 0, 12},
	} {
		costMinor, err := assets.MinorUnits(a.cost, scale)
		if err != nil {
			log.Fatalf("convert cost for %s: %v", a.number, err)
		}
		salvageMinor, err := assets.MinorUnits(a.salvage, scale)
		if err != nil {
			log.Fatalf("convert salvage for %s: %v", a.number, err)
		}

		assetID := s.getOrCreate("FixedAsset", "asset_number", a.number, map[string]any{
			"asset_number":                        a.number,
			"name":                                map[string]any{"en": a.name},
			"location":                            a.location,
			"acquisition_date":                    a.acquired,
			"cost":                                a.cost,
			"salvage_value":                       a.salvage,
			"useful_life_months":                  float64(a.months),
			"depreciation_method":                 "straight_line",
			"currency_id":                         usdID,
			"status_id":                           s.statusID("fixed_asset_status", "in_service"),
			"asset_account_id":                    accounts["1400"],
			"accumulated_depreciation_account_id": accounts["1450"],
			"depreciation_expense_account_id":     accounts["5300"],
		})

		periods, err := assets.Build(assets.Input{
			Method:           assets.MethodStraightLine,
			AcquisitionDate:  a.acquired,
			CostMinor:        costMinor,
			SalvageMinor:     salvageMinor,
			UsefulLifeMonths: a.months,
		})
		if err != nil {
			log.Fatalf("build depreciation schedule for %s: %v", a.number, err)
		}

		// Idempotency is per SEQUENCE, not per asset: an interrupted run
		// leaves a partial schedule, and "this asset already has some
		// rows" would strand it there forever. Same extend-the-prefix
		// discipline seedPurchaseOrders already applies to stage dates
		// (independent review caught this file shipping below its own
		// established bar).
		existing, err := s.crud.ListByField(s.ctx, scheduleDef, "fixed_asset_id", assetID)
		if err != nil {
			log.Fatalf("list existing schedule for %s: %v", a.number, err)
		}
		seeded := make(map[int]bool, len(existing))
		for _, row := range existing {
			if seq, ok := row.Data["sequence"].(float64); ok {
				seeded[int(seq)] = true
			}
		}
		// Only the first year of each schedule is seeded: 60 or 120 rows
		// per asset would bury the rest of the demo data without showing
		// anything the first twelve don't.
		for _, p := range periods[:min(12, len(periods))] {
			if seeded[p.Sequence] {
				continue
			}
			if _, err := s.crud.Create(s.ctx, scheduleDef, map[string]any{
				"fixed_asset_id":      assetID,
				"sequence":            float64(p.Sequence),
				"period_end":          p.PeriodEnd,
				"depreciation_amount": float64(p.DepreciationMinor) / div,
				"book_value":          float64(p.BookValueMinor) / div,
			}, s.actor); err != nil {
				log.Fatalf("create DepreciationSchedule %s/%d: %v", a.number, p.Sequence, err)
			}
		}
	}

	s.seedMaintenanceOrders(currencies, vendors)
}

// seedMaintenanceOrders gives the demo tenant a maintenance history to
// look at on the asset form's related list — one completed job with a
// real cost and one still scheduled, so both halves of the lifecycle
// are visible rather than just the happy end state.
func (s *seeder) seedMaintenanceOrders(currencies, vendors map[string]string) {
	// Self-sufficient against a tenant provisioned before this entity
	// existed — the same reasoning the sales/assets status publishing
	// above uses. Without this, re-running the seeder on the demo tenant
	// before it is re-provisioned aborts the whole run at s.def, after
	// assets and schedules are already written.
	if !hasPublished(s.ctx, s.entityDefs, "MaintenanceOrder") {
		return
	}
	assetDef := s.def("FixedAsset")
	assetID := func(number string) string {
		recs, err := s.crud.ListByField(s.ctx, assetDef, "asset_number", number)
		if err != nil || len(recs) == 0 {
			log.Fatalf("look up FixedAsset %s for maintenance seeding: %v (n=%d)", number, err, len(recs))
		}
		return recs[0].ID
	}
	// Named, not "whichever the map yields first": Go randomizes map
	// iteration, so an arbitrary pick meant a fresh seed could attribute
	// a brake-disc replacement to a textile mill, and two environments
	// got different demo data (independent review). A missing key is
	// fine — vendor_id is optional.
	vendorID := vendors["Anatolia Parts Co."]

	for _, m := range []struct {
		number, asset, mType, desc, scheduled, completed, status string
		cost                                                     float64
	}{
		{"MO-2001", "FA-1001", "preventive", "12,000 km service", "2026-04-10", "2026-04-10", "completed", 450},
		{"MO-2002", "FA-1001", "corrective", "Replace brake discs", "2026-08-15", "", "scheduled", 0},
		{"MO-2003", "FA-1002", "inspection", "Annual racking safety inspection", "2026-06-01", "", "scheduled", 0},
	} {
		fields := map[string]any{
			"order_number":     m.number,
			"asset_id":         assetID(m.asset),
			"maintenance_type": m.mType,
			"description":      map[string]any{"en": m.desc},
			"scheduled_date":   m.scheduled,
			"cost":             m.cost,
			"currency_id":      currencies["USD"],
			"status_id":        s.statusID("maintenance_order_status", m.status),
		}
		if m.completed != "" {
			fields["completed_date"] = m.completed
		}
		if vendorID != "" {
			fields["vendor_id"] = vendorID
		}
		s.getOrCreate("MaintenanceOrder", "order_number", m.number, fields)
	}
}

// currencyMinorUnit reads a Currency record's scale, defaulting to the
// entity's own default of 2 when the record is missing or malformed —
// the same value foundation.Currency() declares, so the fallback can
// never disagree with the schema.
func (s *seeder) currencyMinorUnit(currencyID string) int {
	if currencyID == "" {
		return 2
	}
	rec, err := s.crud.Get(s.ctx, s.def("Currency"), currencyID)
	if err != nil {
		return 2
	}
	if v, ok := rec.Data["minor_unit"].(float64); ok {
		return int(v)
	}
	return 2
}

// seedRoles gives a tenant reference-data example Roles (foundation.Role,
// ADR-0005 — Core-owned access-control roles, distinct from PartyRole's
// business-relationship roles). Deliberately does NOT seed any UserRole
// grants alongside them: a UserRole.user_id must be a real Zitadel OIDC
// "sub" (see UserRole's own doc comment) — this command has no way to
// know which real Zitadel user, if any, should hold a role in whatever
// tenant it's pointed at, and inventing a fake sub would create a
// misleading "phantom user has access" record with no real Zitadel
// account behind it. Granting a real role to a real person is exactly
// what the not-yet-built Self-service tenant member management page
// (erp/BACKLOG-TASKS.md Phase 2) is for — this just gives that page
// something real to assign once it exists.
func (s *seeder) seedRoles() {
	for _, r := range []struct{ code, name, description string }{
		{"finance_manager", "Finance Manager", "Approves invoices and journal postings over the tenant's threshold"},
		{"warehouse_supervisor", "Warehouse Supervisor", "Manages goods receipt and inventory adjustments"},
		{"sales_rep", "Sales Rep", "Creates and manages sales orders and customer invoices"},
	} {
		s.getOrCreate("Role", "code", r.code, map[string]any{
			"code": r.code, "name": r.name, "description": r.description,
		})
	}
}

// seedOrgChart gives a tenant a small example Department/Position
// hierarchy (foundation.Department/Position — the org-chart entities,
// `erp/BACKLOG-TASKS.md`'s "Department/org-chart model" task) so a fresh
// tenant's Position form doesn't open onto an empty department_id
// picker with nothing to select — the same "sample data actually
// demonstrates the pattern" reasoning seedParties' own doc comment gives
// for PartyRole. Position's title has no natural-key alternative
// (unlike Department's code), so it's keyed on title directly here —
// fine for this small, fixed demo set, not a general pattern.
func (s *seeder) seedOrgChart() {
	companyID := s.getOrCreate("Department", "code", "co", map[string]any{
		"code": "co", "name": "Demo Organization",
	})
	financeID := s.getOrCreate("Department", "code", "fin", map[string]any{
		"code": "fin", "name": "Finance", "parent_department_id": companyID,
	})
	warehouseID := s.getOrCreate("Department", "code", "wh", map[string]any{
		"code": "wh", "name": "Warehouse", "parent_department_id": companyID,
	})

	cfoID := s.getOrCreate("Position", "title", "CFO", map[string]any{
		"title": "CFO", "department_id": financeID,
	})
	s.getOrCreate("Position", "title", "Finance Manager", map[string]any{
		"title": "Finance Manager", "department_id": financeID, "reports_to_position_id": cfoID,
	})
	s.getOrCreate("Position", "title", "Warehouse Supervisor", map[string]any{
		"title": "Warehouse Supervisor", "department_id": warehouseID,
	})
}

// ensurePartyRole gives partyID the named PartyRole if it doesn't hold
// it already. A method rather than seedParties' local closure because
// seedPipeline needs the same idempotency for the `contact` role
// (ADR-0014) — and PartyRole has no single natural key, so getOrCreate
// cannot express it.
func (s *seeder) ensurePartyRole(partyID, roleType string) {
	roleDef := s.def("PartyRole")
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

// seedParties creates both vendors and customers, tagging each with a
// PartyRole row — the reference-data-model.md Party-Role pattern this
// kernel is built around, so the sample data actually demonstrates it
// instead of leaving PartyRole empty. Names lean into the UK+GCC+Turkey
// launch markets (BACKLOG.md's R1) rather than generic placeholders.
func (s *seeder) seedParties() (vendors, customers map[string]string) {
	seedRole := s.ensurePartyRole

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

// inventoryLevel is one item's stock at one facility.
type inventoryLevel struct {
	facility string
	onHand   float64
	atp      float64
}

// inventoryLevels is the single declaration of what the demo tenant's
// stock looks like — shared by seedInventory and the pre-#12 fallback
// so the two can never drift into disagreeing about the numbers.
var inventoryLevels = map[string][]inventoryLevel{
	// Split across two locations: total ATP is healthy, and neither
	// row alone tells the whole story.
	"SKU-1001": {{"MAIN", 400, 400}, {"STORE-01", 100, 100}},
	"SKU-1002": {{"MAIN", 120, 120}},
	"SKU-1003": {{"MAIN", 250, 250}, {"STORE-01", 50, 50}},
	"SKU-1004": {{"MAIN", 80, 0}}, // fully committed — stockout risk
	"SKU-1005": {{"MAIN", 45, 45}},
	"SKU-1006": {{"MAIN", 600, 600}},
	"SKU-1007": {{"MAIN", 25, -10}}, // over-committed — stockout risk
	"SKU-1008": {{"STORE-01", 200, 200}},
	"SKU-1009": {{"MAIN", 0, 0}}, // never restocked — stockout risk
	"SKU-1010": {{"MAIN", 60, 60}},
}

// seedFacilities gives the demo tenant three stock locations — a main
// warehouse, a retail store and a virtual bucket — so the (item,
// facility) key #12 introduced has more than one facility to be a key
// over. A single-facility demo would let a per-facility bug through
// unnoticed, which is the whole reason the multi-location work exists.
//
// The virtual facility is not filler: reference-data-model.md §3 lists
// `virtual` as a facility type precisely for stock that is owned but
// not in a countable place — goods in transit, consignment, quarantine
// — and #13's stock transfer needs somewhere for in-transit stock to
// live.
func (s *seeder) seedFacilities() map[string]string {
	ids := map[string]string{}
	for _, f := range []struct{ code, name, facilityType string }{
		{"MAIN", "Main Warehouse", "warehouse"},
		{"STORE-01", "Doha Retail Store", "store"},
		{"TRANSIT", "In Transit", "virtual"},
	} {
		ids[f.code] = s.getOrCreate("Facility", "code", f.code, map[string]any{
			"code": f.code, "name": f.name, "facility_type": f.facilityType, "is_active": true,
		})
	}
	return ids
}

// seedInventory gives every stock Item an on-hand + available-to-promise
// level — service items (no natural inventory concept) are deliberately
// skipped, same as InventoryItem's own doc comment describes the
// entity's scope. onHand and availableToPromise deliberately differ for
// a few SKUs (fully or over-committed against existing sales
// allocations, not modeled by this simplified kernel) so the mgmt
// reporting workbench's stockout-risk table has real rows to show, not
// just a demo of an empty state.
//
// **The stockout SKUs are stocked at ONE facility on purpose.** The
// stockout report sums availability across facilities (#12/ADR-0015),
// so an item that is empty in the store but full in the warehouse must
// NOT appear as at risk. If every SKU were split across locations, that
// aggregation would be untested by the demo data and a regression to
// per-row counting would still produce a plausible-looking dashboard.
// SKU-1001 and SKU-1003 are split across two facilities and remain
// healthy; the three at-risk SKUs are single-facility and genuinely
// exhausted everywhere.
func (s *seeder) seedInventory(items, facilities map[string]string) {
	def := s.def("InventoryItem")
	levels := inventoryLevels
	for sku, itemID := range items {
		rows, ok := levels[sku]
		if !ok {
			continue
		}
		existing, err := s.crud.ListByField(s.ctx, def, "item_id", itemID)
		if err != nil {
			log.Fatalf("list InventoryItem by item_id: %v", err)
		}

		// This CONVERGES on the table above rather than only inserting
		// what is missing, and the difference is not academic — an
		// independent review measured the insert-only version inflating
		// the demo tenant's stock on the upgrade path ADR-0015 itself
		// prescribes (re-provision, then backfill, then re-seed). Two
		// ways it went wrong, both structural:
		//
		//   - a pre-#12 row carries the OLD quantity (SKU-1001 held 500
		//     before this card split it into 400 + 100). The backfill
		//     stamps it at MAIN, an insert-only seeder sees MAIN covered
		//     and skips, so the stale 500 stands while the new STORE-01
		//     row is added on top;
		//   - SKU-1008 MOVED facility in this card, MAIN -> STORE-01. The
		//     backfilled MAIN row is stock at a facility the table no
		//     longer declares, and STORE-01 gets created beside it.
		//     Doubled.
		//
		// So: a row already at a declared facility has its quantities
		// corrected; a row that is facility-less or sits at an undeclared
		// facility is REUSED — re-pointed at a declared facility that has
		// no row yet — rather than left to rot beside a new one.
		// Re-pointing rather than deleting keeps this non-destructive,
		// and genuine surplus is reported rather than silently removed:
		// a seeder quietly deleting stock rows in a tenant someone has
		// been clicking around in is worse than a loud warning.
		type want struct {
			facilityID string
			onHand     float64
			atp        float64
		}
		desired := make([]want, 0, len(rows))
		declared := map[string]bool{}
		for _, row := range rows {
			facilityID := facilities[row.facility]
			if facilityID == "" {
				log.Fatalf("seedInventory: no facility seeded for code %q", row.facility)
			}
			desired = append(desired, want{facilityID, row.onHand, row.atp})
			declared[facilityID] = true
		}

		atFacility := map[string]data.Record{}
		var loose []data.Record
		for _, e := range existing {
			fid, _ := e.Data["facility_id"].(string)
			if _, taken := atFacility[fid]; fid != "" && declared[fid] && !taken {
				atFacility[fid] = e
				continue
			}
			loose = append(loose, e)
		}

		set := func(rec data.Record, w want) {
			curFacility, _ := rec.Data["facility_id"].(string)
			curOnHand, _ := rec.Data["qty_on_hand"].(float64)
			curATP, _ := rec.Data["qty_available_to_promise"].(float64)
			if curFacility == w.facilityID && curOnHand == w.onHand && curATP == w.atp {
				return // already correct — don't churn the version or the audit trail
			}
			version := rec.Version
			if _, err := s.crud.Update(s.ctx, def, rec.ID, map[string]any{
				"item_id": itemID, "facility_id": w.facilityID,
				"qty_on_hand": w.onHand, "qty_available_to_promise": w.atp,
			}, &version, s.actor); err != nil {
				log.Fatalf("reconcile InventoryItem %s: %v", rec.ID, err)
			}
		}

		for _, w := range desired {
			if rec, ok := atFacility[w.facilityID]; ok {
				set(rec, w)
				continue
			}
			if len(loose) > 0 {
				set(loose[0], w)
				loose = loose[1:]
				continue
			}
			if _, err := s.crud.Create(s.ctx, def, map[string]any{
				"item_id": itemID, "facility_id": w.facilityID,
				"qty_on_hand": w.onHand, "qty_available_to_promise": w.atp,
			}, s.actor); err != nil {
				log.Fatalf("create InventoryItem: %v", err)
			}
		}
		for _, extra := range loose {
			fid, _ := extra.Data["facility_id"].(string)
			log.Printf("WARNING: InventoryItem %s for %s sits at facility %q, which the demo data does not declare — left in place; it will skew stock totals",
				extra.ID, sku, fid)
		}
	}
}

// seedInventoryWithoutFacilities is the pre-#12 shape, kept for a
// tenant that has purchasing published but not Facility yet (#70).
// One row per item carrying the item's TOTAL across the facilities the
// current table splits it into, so the stock figures such a tenant
// reports match a fully-provisioned one — the location dimension is
// what it lacks, not the quantities.
//
// This exists to be deleted. Once #70 makes existing tenants pick up new
// Definitions automatically, no tenant can be in this state and the
// fallback becomes dead code.
func (s *seeder) seedInventoryWithoutFacilities(items map[string]string) {
	def := s.def("InventoryItem")
	for sku, itemID := range items {
		rows, ok := inventoryLevels[sku]
		if !ok {
			continue
		}
		var onHand, atp float64
		for _, r := range rows {
			onHand += r.onHand
			atp += r.atp
		}
		existing, err := s.crud.ListByField(s.ctx, def, "item_id", itemID)
		if err != nil {
			log.Fatalf("list InventoryItem by item_id: %v", err)
		}
		if len(existing) > 0 {
			continue
		}
		if _, err := s.crud.Create(s.ctx, def, map[string]any{
			"item_id": itemID, "qty_on_hand": onHand, "qty_available_to_promise": atp,
		}, s.actor); err != nil {
			log.Fatalf("create InventoryItem: %v", err)
		}
	}
}

// seedReorderRules gives three demo items a ReorderRule (#30), chosen
// against seedInventory's levels and seedPurchaseOrders' open orders so
// the purchasing report's reorder-signal section actually demonstrates
// the position math, not just an empty state:
//
//   - SKU-1004 (on-hand 80, no open PO): position 80 <= 150+50 -> FIRES,
//     with the P90 expected-days context (its lead-time history comes
//     from overall stats — its only PO's vendor has one completed order,
//     below forecast.MinSamples).
//   - SKU-1010 (on-hand 60, 90 on order via PO-2026-0006): position 150
//     <= 200 -> FIRES, with the P50 context this rule opts into.
//   - SKU-1001 (on-hand 500, 2000 on order via PO-2026-0002): on-hand
//     alone is below the 600 reorder point, but position 2500 is not ->
//     does NOT fire — the live demo of BA acceptance #4 (a huge open PO
//     holds the position up; the goods are already coming).
//
// seedStockTransfers gives the demo tenant one real StockTransfer (#13)
// so the entity's list page, form and validation hook (this file already
// registers purchasing.ValidateStockTransfer on the seeding engine) have
// live data behind them instead of an empty state — the same reason
// every other entity in this file is seeded at all.
//
// **Deliberately a single "draft" transfer, not the whole lifecycle.**
// Moving stock between InventoryItem rows is explicitly NOT built yet
// (purchasing.StockTransfer's own doc comment: qty_on_hand debit/credit
// is a later, careful pass), so a seeded "in_transit" or "received"
// transfer would assert on the dashboard that 25 units left MAIN while
// every stock report still counts them there — demo data contradicting
// the reports beside it, which is exactly the class of bug an
// independent review already caught in seedInventory. "draft" is the one
// state that is honestly true today: the transfer is recorded, nothing
// has moved. When the qty_on_hand pass lands, this is where the richer
// lifecycle belongs.
//
// Dedups on item_id, the same "no natural key of its own" shape
// seedReorderRules below uses — StockTransfer has no transfer number.
func (s *seeder) seedStockTransfers(items, facilities map[string]string) {
	itemID, ok := items["SKU-1001"]
	if !ok {
		return
	}
	from, to := facilities["MAIN"], facilities["STORE-01"]
	if from == "" || to == "" {
		return
	}

	def := s.def("StockTransfer")
	existing, err := s.crud.ListByField(s.ctx, def, "item_id", itemID)
	if err != nil {
		log.Fatalf("list StockTransfer by item_id: %v", err)
	}
	if len(existing) > 0 {
		return
	}
	if _, err := s.crud.Create(s.ctx, def, map[string]any{
		"item_id":          itemID,
		"from_facility_id": from,
		"to_facility_id":   to,
		"qty":              float64(25),
		"transfer_date":    "2026-07-28",
		"status_id":        s.statusID("stock_transfer_status", "draft"),
		"notes":            "Replenishment for the retail store",
	}, s.actor); err != nil {
		log.Fatalf("create StockTransfer: %v", err)
	}
}

// Dedups on item_id — a ReorderRule has no natural key of its own, and
// one rule per item is this simplified, warehouse-less model's whole
// shape (see purchasing.ReorderRule's doc comment on the #12 deferral).
func (s *seeder) seedReorderRules(items map[string]string) {
	def := s.def("ReorderRule")
	rules := []struct {
		sku                       string
		reorderPoint, safetyStock float64
		confidence                string
	}{
		{"SKU-1004", 150, 50, "p90"},
		{"SKU-1010", 200, 0, "p50"},
		{"SKU-1001", 600, 0, "p90"},
	}
	for _, r := range rules {
		itemID, ok := items[r.sku]
		if !ok {
			log.Fatalf("seedReorderRules: no seeded item for %s", r.sku)
		}
		existing, err := s.crud.ListByField(s.ctx, def, "item_id", itemID)
		if err != nil {
			log.Fatalf("list ReorderRule by item_id: %v", err)
		}
		if len(existing) > 0 {
			continue
		}
		if _, err := s.crud.Create(s.ctx, def, map[string]any{
			"item_id":                     itemID,
			"reorder_point":               r.reorderPoint,
			"safety_stock":                r.safetyStock,
			"target_lead_time_confidence": r.confidence,
		}, s.actor); err != nil {
			log.Fatalf("create ReorderRule for %s: %v", r.sku, err)
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
// statusID looks up a Status record by its code, scoped to a specific
// StatusType code — required now that a second module (sales) exists:
// "draft" alone is ambiguous once purchase_order_status,
// sales_order_status, and customer_invoice_status all declare their own
// "draft" Status row with a different status_type_id. This used to be a
// plain code-only lookup when purchase_order_status was the only
// StatusType in the system (see git history) — that comment predicted
// exactly this generalization would be needed "if a second StatusType
// ever" existed, which sales.PublishStatuses' two StatusTypes now do.
// A seeder method (not a local closure inside seedPurchaseOrders) since
// seedGoodsReceipts/seedSalesOrders/seedCustomerInvoices all need the
// same lookup.
func (s *seeder) statusID(statusTypeCode, code string) string {
	statusTypeDef := s.def("StatusType")
	types, err := s.crud.ListByField(s.ctx, statusTypeDef, "code", statusTypeCode)
	if err != nil {
		log.Fatalf("list StatusType by code %q: %v", statusTypeCode, err)
	}
	if len(types) == 0 {
		log.Fatalf("no StatusType record for code %q (was its module's PublishStatuses run?)", statusTypeCode)
	}

	statusDef := s.def("Status")
	recs, err := s.crud.ListByField(s.ctx, statusDef, "status_type_id", types[0].ID)
	if err != nil {
		log.Fatalf("list Status by status_type_id for %q: %v", statusTypeCode, err)
	}
	for _, r := range recs {
		if c, _ := r.Data["code"].(string); c == code {
			return r.ID
		}
	}
	log.Fatalf("no Status record for code %q under StatusType %q", code, statusTypeCode)
	return ""
}

func (s *seeder) seedPurchaseOrders(vendors, currencies, items map[string]string) {
	poDef := s.def("PurchaseOrder")
	lineDef := s.def("POLine")

	type line struct {
		sku      string
		qty      float64
		unitCost float64
	}
	// stages (#29): the six R10 lead-time timestamps, chronological per
	// PO and deliberately varied in duration — this is #30's forecast
	// demo data, so the spread matters more than the individual values.
	// Received POs carry the full chain; in-flight ones a realistic
	// prefix (censored data the forecast has to handle); drafts and the
	// cancelled PO none.
	orders := []struct {
		poNumber string
		vendor   string
		currency string
		date     string
		status   string
		stages   map[string]string
		lines    []line
	}{
		// Received (#30 review): a second completed Acme Textiles order —
		// 25 days against PO-2026-0004's 9 — gives the live demo a vendor
		// with n>=2, so the report shows a real per-vendor P50/P90 row
		// (Acme P50 17 / P90 23.4), not only insufficient-history rows.
		{"PO-2026-0001", "Acme Textiles", "USD", "2026-07-01", "received",
			map[string]string{"sourced_at": "2026-07-04", "production_start_at": "2026-07-08", "production_ready_at": "2026-07-18", "shipped_at": "2026-07-22", "customs_cleared_at": "2026-07-24", "received_at": "2026-07-26"},
			[]line{{"SKU-1002", 40, 18.5}}},
		{"PO-2026-0002", "Gulf Steel Supply", "QAR", "2026-07-10", "submitted",
			map[string]string{"sourced_at": "2026-07-14"},
			[]line{{"SKU-1001", 2000, 0.35}}},
		{"PO-2026-0003", "Anatolia Parts Co.", "TRY", "2026-07-15", "draft", nil,
			[]line{{"SKU-1003", 150, 4.2}, {"SKU-2001", 8, 120}}},
		{"PO-2026-0004", "Acme Textiles", "USD", "2026-07-18", "received",
			map[string]string{"sourced_at": "2026-07-19", "production_start_at": "2026-07-20", "production_ready_at": "2026-07-23", "shipped_at": "2026-07-24", "customs_cleared_at": "2026-07-26", "received_at": "2026-07-27"},
			[]line{{"SKU-1005", 60, 22}, {"SKU-1006", 30, 9.5}}},
		{"PO-2026-0005", "Doha Fasteners LLC", "QAR", "2026-07-19", "received",
			map[string]string{"sourced_at": "2026-07-20", "production_start_at": "2026-07-22", "production_ready_at": "2026-07-25", "shipped_at": "2026-07-26", "customs_cleared_at": "2026-07-28", "received_at": "2026-07-30"},
			[]line{{"SKU-1004", 3000, 0.42}, {"SKU-1009", 5000, 0.08}}},
		{"PO-2026-0006", "Istanbul Weaving Mills", "TRY", "2026-07-19", "approved",
			map[string]string{"sourced_at": "2026-07-21", "production_start_at": "2026-07-24"},
			[]line{{"SKU-1010", 90, 26.5}}},
		{"PO-2026-0007", "Manchester Packaging Ltd", "GBP", "2026-07-20", "submitted",
			map[string]string{"sourced_at": "2026-07-23"},
			[]line{{"SKU-1007", 400, 3.1}, {"SKU-1008", 250, 5.75}}},
		{"PO-2026-0008", "Gulf Steel Supply", "QAR", "2026-07-20", "cancelled", nil,
			[]line{{"SKU-1001", 500, 0.35}}},
		{"PO-2026-0009", "Anatolia Parts Co.", "TRY", "2026-07-21", "draft", nil,
			[]line{{"SKU-2002", 4, 90}}},
		{"PO-2026-0010", "Doha Fasteners LLC", "QAR", "2026-07-21", "approved",
			map[string]string{"sourced_at": "2026-07-23", "production_start_at": "2026-07-26", "production_ready_at": "2026-07-30"},
			[]line{{"SKU-1009", 8000, 0.08}}},
	}
	for _, o := range orders {
		existing, err := s.crud.ListByField(s.ctx, poDef, "po_number", o.poNumber)
		if err != nil {
			log.Fatalf("list PurchaseOrder by po_number: %v", err)
		}
		if len(existing) > 0 {
			// Stage backfill (#29, converged for #30): a PO seeded before
			// the current stage table keeps its identity but gains any
			// stage it's missing — per-stage, not all-or-nothing. The
			// first version skipped the whole PO once sourced_at was set,
			// which stranded the live tenant between two seed versions:
			// #29's reseed gave PO-2026-0001 a four-stage prefix, so
			// #30's extension of that same PO to a full received chain
			// (the review's fix for "the demo never shows a per-vendor
			// quantile") was silently skipped on the one tenant that
			// matters. Idempotent as before — a stage that already holds
			// any value is never overwritten — and a PO needing nothing
			// writes nothing. One consequence of filling a HOLE between
			// existing stages: the fill becomes a new NotBefore
			// comparison point, so a live PO whose existing stages
			// contradict the seed table's values aborts this run with the
			// validation error (log.Fatalf below) — clear or correct the
			// offending stage by hand; the seeder deliberately refuses to
			// overwrite it, and refusing beats writing a reversed chain.
			rec := existing[0]
			missing := map[string]string{}
			for k, v := range o.stages {
				if cur, _ := rec.Data[k].(string); cur == "" {
					missing[k] = v
				}
			}
			if len(missing) == 0 {
				continue
			}
			fields := make(map[string]any, len(rec.Data)+len(missing))
			maps.Copy(fields, rec.Data)
			for k, v := range missing {
				fields[k] = v
			}
			expectedVersion := rec.Version
			if _, err := s.crud.Update(s.ctx, poDef, rec.ID, fields, &expectedVersion, s.actor); err != nil {
				log.Fatalf("backfill stages on %s: %v", o.poNumber, err)
			}
			continue
		}

		createFields := map[string]any{
			"po_number":   o.poNumber,
			"vendor_id":   vendors[o.vendor],
			"currency_id": currencies[o.currency],
			"order_date":  o.date,
			"status_id":   s.statusID("purchase_order_status", o.status),
		}
		for k, v := range o.stages {
			createFields[k] = v
		}
		poID, err := s.crud.Create(s.ctx, poDef, createFields, s.actor)
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
		totalFields := map[string]any{
			"po_number": o.poNumber, "vendor_id": vendors[o.vendor], "currency_id": currencies[o.currency],
			"order_date": o.date, "status_id": s.statusID("purchase_order_status", o.status), "total": total,
		}
		// Full-replacement semantics (see comment above): the stages set
		// at create time must ride along or this update would drop them.
		for k, v := range o.stages {
			totalFields[k] = v
		}
		if _, err := s.crud.Update(s.ctx, poDef, poID.ID, totalFields, &expectedVersion, s.actor); err != nil {
			log.Fatalf("update PurchaseOrder total: %v", err)
		}
	}
}

// seedGoodsReceipts gives every already-"received" PurchaseOrder
// (PO-2026-0001, PO-2026-0004, PO-2026-0005 in seedPurchaseOrders' own
// table above) a
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

	received, err := s.crud.ListByField(s.ctx, poDef, "status_id", s.statusID("purchase_order_status", "received"))
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

// seedVendorInvoices gives PO-2026-0004 and PO-2026-0005 (the two
// "received" PurchaseOrders seedGoodsReceipts already gave a real
// GoodsReceipt+GoodsReceiptLines to) a real VendorInvoice each — sample
// data that actually exercises purchasing.MatchVendorInvoiceOnUpdate
// end to end against real received data, the same reasoning
// seedCustomerInvoices gives for driving CustomerInvoice through real
// draft->issued transitions rather than creating pre-set final state.
// Both PurchaseOrders were received in full (seedGoodsReceipts), so
// each invoice's total is set to exactly its PurchaseOrder's own total
// — the received value the match hook computes will equal that exactly,
// so both invoices match cleanly (a seeder demonstrating its own hook
// rejecting sample data would be a bug, not a feature — a genuine
// mismatch case belongs in a unit test, not here). VINV-2026-0001 goes
// draft->matched->paid, VINV-2026-0002 stops at draft->matched — same
// "show more than one live lifecycle state" reasoning as
// seedCustomerInvoices' own issued/paid split. Looks the PurchaseOrder
// up by po_number (ListByField) rather than needing seedPurchaseOrders
// to return a po_number->id map — smaller, more isolated change than
// growing that function's signature for one new caller.
func (s *seeder) seedVendorInvoices(vendors, currencies map[string]string) {
	invDef := s.def("VendorInvoice")
	poDef := s.def("PurchaseOrder")

	invoices := []struct {
		invoiceNumber, poNumber, vendor, currency, date string
		// statusPath is every status this invoice moves through after
		// draft, in order — same shape as seedCustomerInvoices' own.
		statusPath []string
	}{
		{"VINV-2026-0001", "PO-2026-0004", "Acme Textiles", "USD", "2026-07-24", []string{"matched", "paid"}},
		{"VINV-2026-0002", "PO-2026-0005", "Doha Fasteners LLC", "QAR", "2026-07-25", []string{"matched"}},
	}
	for _, inv := range invoices {
		existing, err := s.crud.ListByField(s.ctx, invDef, "invoice_number", inv.invoiceNumber)
		if err != nil {
			log.Fatalf("list VendorInvoice by invoice_number: %v", err)
		}
		if len(existing) > 0 {
			continue
		}
		pos, err := s.crud.ListByField(s.ctx, poDef, "po_number", inv.poNumber)
		if err != nil {
			log.Fatalf("list PurchaseOrder by po_number: %v", err)
		}
		if len(pos) == 0 {
			log.Fatalf("no PurchaseOrder found for po_number %s (was seedPurchaseOrders run first?)", inv.poNumber)
		}
		total, _ := pos[0].Data["total"].(float64)
		fields := map[string]any{
			"invoice_number":    inv.invoiceNumber,
			"purchase_order_id": pos[0].ID,
			"vendor_id":         vendors[inv.vendor],
			"currency_id":       currencies[inv.currency],
			"invoice_date":      inv.date,
			"status_id":         s.statusID("vendor_invoice_status", "draft"),
			"total":             total,
		}
		rec, err := s.crud.Create(s.ctx, invDef, fields, s.actor)
		if err != nil {
			log.Fatalf("create VendorInvoice %s: %v", inv.invoiceNumber, err)
		}
		version := rec.Version
		for _, status := range inv.statusPath {
			fields["status_id"] = s.statusID("vendor_invoice_status", status)
			newVersion, err := s.crud.Update(s.ctx, invDef, rec.ID, fields, &version, s.actor)
			if err != nil {
				log.Fatalf("update VendorInvoice %s to %s: %v", inv.invoiceNumber, status, err)
			}
			version = newVersion
		}
	}
}

// seedSalesOrders dedups on so_number — same getOrCreate-style natural-
// key pattern as seedPurchaseOrders, and for the same reason can't just
// call getOrCreate directly (SOLines need the parent's id first, total
// is only known after the lines exist). Returns the so_number -> id map
// so seedCustomerInvoices can look up the order it bills without a
// second hardcoded table that could drift from this one.
func (s *seeder) seedSalesOrders(customers, currencies, items map[string]string) map[string]string {
	soDef := s.def("SalesOrder")
	lineDef := s.def("SOLine")

	type line struct {
		sku       string
		qty       float64
		unitPrice float64
	}
	orders := []struct {
		soNumber string
		customer string
		currency string
		date     string
		status   string
		lines    []line
	}{
		{"SO-2026-0001", "Doha Retail Group", "QAR", "2026-07-05", "invoiced", []line{{"SKU-1001", 300, 0.6}}},
		{"SO-2026-0002", "London Fashion House", "GBP", "2026-07-12", "invoiced", []line{{"SKU-1002", 20, 32}, {"SKU-1005", 15, 38}}},
		{"SO-2026-0003", "Doha Retail Group", "QAR", "2026-07-18", "fulfilled", []line{{"SKU-1009", 1500, 0.15}}},
		{"SO-2026-0004", "London Fashion House", "GBP", "2026-07-20", "confirmed", []line{{"SKU-1010", 25, 45}}},
		{"SO-2026-0005", "Doha Retail Group", "QAR", "2026-07-22", "draft", []line{{"SKU-1006", 200, 0.9}}},
		{"SO-2026-0006", "London Fashion House", "GBP", "2026-07-23", "cancelled", []line{{"SKU-1002", 10, 32}}},
	}

	soIDs := map[string]string{}
	for _, o := range orders {
		existing, err := s.crud.ListByField(s.ctx, soDef, "so_number", o.soNumber)
		if err != nil {
			log.Fatalf("list SalesOrder by so_number: %v", err)
		}
		if len(existing) > 0 {
			soIDs[o.soNumber] = existing[0].ID
			continue
		}

		soID, err := s.crud.Create(s.ctx, soDef, map[string]any{
			"so_number":   o.soNumber,
			"customer_id": customers[o.customer],
			"currency_id": currencies[o.currency],
			"order_date":  o.date,
			"status_id":   s.statusID("sales_order_status", o.status),
		}, s.actor)
		if err != nil {
			log.Fatalf("create SalesOrder for %s: %v", o.customer, err)
		}
		var total float64
		for _, l := range o.lines {
			lineTotal := l.qty * l.unitPrice
			total += lineTotal
			if _, err := s.crud.Create(s.ctx, lineDef, map[string]any{
				"sales_order_id": soID.ID,
				"item_id":        items[l.sku],
				"qty":            l.qty,
				"unit_price":     l.unitPrice,
				"line_total":     lineTotal,
			}, s.actor); err != nil {
				log.Fatalf("create SOLine: %v", err)
			}
		}
		expectedVersion := soID.Version
		if _, err := s.crud.Update(s.ctx, soDef, soID.ID, map[string]any{
			"so_number": o.soNumber, "customer_id": customers[o.customer], "currency_id": currencies[o.currency],
			"order_date": o.date, "status_id": s.statusID("sales_order_status", o.status), "total": total,
		}, &expectedVersion, s.actor); err != nil {
			log.Fatalf("update SalesOrder total: %v", err)
		}
		soIDs[o.soNumber] = soID.ID
	}
	return soIDs
}

// seedCustomerInvoices gives every "invoiced" SalesOrder (SO-2026-0001,
// SO-2026-0002 above) a real CustomerInvoice — sample data that
// demonstrates the entity exists, same reasoning seedGoodsReceipts gives
// for GoodsReceipt. Dedups on invoice_number (CustomerInvoice does have a
// natural key, unlike GoodsReceipt). SO-2026-0001's invoice is left
// "issued" (billed, not yet paid) and SO-2026-0002's is "paid", so the
// demo data shows both live states of the customer_invoice_status
// lifecycle, not just the happy path's terminal state.
// seedCustomerInvoices creates each invoice in "draft" (customer_invoice_
// status' only is_initial state — real production Creates go through
// this same starting point, internal/api's createRecord handler enforces
// it via ValidateStatusTransition even though this direct-crud.Engine
// path doesn't) and then Updates it through the real draft->issued(->
// paid) transitions, rather than creating it pre-set to its final
// status. Not just more realistic sample data: PostCustomerInvoiceToLedger
// (internal/kernel/sales/ledger.go) is a crud.Hook registered for
// Update, not Create (an unissued invoice has no financial reality yet)
// — creating pre-issued invoices directly would silently never exercise
// it, leaving the ledger-posting path completely unverified by this
// command despite looking like it seeded "issued" sample data.
func (s *seeder) seedCustomerInvoices(customers, currencies, soIDs map[string]string) {
	invDef := s.def("CustomerInvoice")
	soDef := s.def("SalesOrder")

	invoices := []struct {
		invoiceNumber, soNumber, customer, currency, date string
		// statusPath is every status this invoice moves through after
		// draft, in order — {"issued"} for one, {"issued", "paid"} for
		// the other, so the sample data still demonstrates both.
		statusPath []string
	}{
		{"INV-2026-0001", "SO-2026-0001", "Doha Retail Group", "QAR", "2026-07-06", []string{"issued"}},
		{"INV-2026-0002", "SO-2026-0002", "London Fashion House", "GBP", "2026-07-13", []string{"issued", "paid"}},
	}
	for _, inv := range invoices {
		existing, err := s.crud.ListByField(s.ctx, invDef, "invoice_number", inv.invoiceNumber)
		if err != nil {
			log.Fatalf("list CustomerInvoice by invoice_number: %v", err)
		}
		if len(existing) > 0 {
			continue
		}
		so, err := s.crud.Get(s.ctx, soDef, soIDs[inv.soNumber])
		if err != nil {
			log.Fatalf("get SalesOrder %s: %v", inv.soNumber, err)
		}
		total, _ := so.Data["total"].(float64)
		fields := map[string]any{
			"invoice_number": inv.invoiceNumber,
			"sales_order_id": soIDs[inv.soNumber],
			"customer_id":    customers[inv.customer],
			"currency_id":    currencies[inv.currency],
			"invoice_date":   inv.date,
			"status_id":      s.statusID("customer_invoice_status", "draft"),
			"total":          total,
		}
		rec, err := s.crud.Create(s.ctx, invDef, fields, s.actor)
		if err != nil {
			log.Fatalf("create CustomerInvoice %s: %v", inv.invoiceNumber, err)
		}
		version := rec.Version
		for _, status := range inv.statusPath {
			fields["status_id"] = s.statusID("customer_invoice_status", status)
			newVersion, err := s.crud.Update(s.ctx, invDef, rec.ID, fields, &version, s.actor)
			if err != nil {
				log.Fatalf("update CustomerInvoice %s to %s: %v", inv.invoiceNumber, status, err)
			}
			version = newVersion
		}
	}
}

// seedProjects gives the demo tenant a project with a real task
// hierarchy and logged time — a subtask included, because
// parent_task_id being set on only some rows is exactly the shape that
// used to render a ragged task table (board #18's review), and demo
// data that never exercises it would hide a regression.
func (s *seeder) seedProjects(currencies, customers map[string]string) {
	if !hasPublished(s.ctx, s.entityDefs, "Task") || !hasPublished(s.ctx, s.entityDefs, "TimeEntry") {
		return
	}
	taskDef := s.def("Task")
	timeDef := s.def("TimeEntry")

	// The "employee" is a Party carrying the employee PartyRole — the
	// module has no Employee entity, see its package comment.
	engineer := s.getOrCreate("Party", "name", "Demo Engineer", map[string]any{
		"party_type": "person", "name": "Demo Engineer", "status": "active",
	})
	s.getOrCreate("PartyRole", "party_id", engineer, map[string]any{
		"party_id": engineer, "role_type": "employee",
	})

	var customerID string
	for _, name := range []string{"Doha Retail Group", "Gulf Trading LLC"} {
		if id, ok := customers[name]; ok {
			customerID = id
			break
		}
	}

	projectID := s.getOrCreate("Project", "project_code", "PRJ-2026-001", map[string]any{
		"project_code": "PRJ-2026-001",
		"name":         map[string]any{"en": "ERP Rollout", "tr": "ERP Kurulumu"},
		"customer_id":  customerID,
		"start_date":   "2026-02-01",
		"end_date":     "2026-08-31",
		"budget":       75000.0,
		"currency_id":  currencies["USD"],
		"status_id":    s.statusID("project_status", "active"),
	})

	// ProjectBudgetLine (uc-infra#79) is its own module-licensing check
	// (a tenant provisioned before this change has no ProjectBudgetLine
	// Definition published yet) placed before the TimeEntry dedup
	// return below (not independent of it — this function still runs
	// top to bottom — but seeding budget lines only AFTER that return
	// would skip them entirely on any re-run once time entries already
	// exist, which is exactly the ordering bug this placement avoids).
	// category has no natural key of its own (two different projects
	// could share a category), so this uses the same per-row `seen`
	// idempotency pattern seedHR's AttendanceRecord block below uses
	// (a `len(existing) == 0` all-or-nothing guard would leave a
	// partial set stuck forever if a run died after creating some but
	// not all rows — the two other seeders in this same function,
	// Task's title-index and TimeEntry's own dedup, both avoid that for
	// the same reason). The four lines deliberately sum to the
	// project's own 75000.0 budget above: an advisory breakdown that
	// happens to match the total in the demo data, without the two
	// being structurally tied together (see the package doc comment on
	// why Project.budget is not derived from these).
	if hasPublished(s.ctx, s.entityDefs, "ProjectBudgetLine") {
		lineDef := s.def("ProjectBudgetLine")
		existingLines, err := s.crud.ListByField(s.ctx, lineDef, "project_id", projectID)
		if err != nil {
			log.Fatalf("list existing budget lines: %v", err)
		}
		seenCategory := map[string]bool{}
		for _, rec := range existingLines {
			if c, ok := rec.Data["category"].(string); ok {
				seenCategory[c] = true
			}
		}
		for _, l := range []struct {
			category string
			planned  float64
		}{
			{"labour", 45000.0},
			{"materials", 15000.0},
			{"travel", 5000.0},
			{"other", 10000.0},
		} {
			if seenCategory[l.category] {
				continue
			}
			if _, err := s.crud.Create(s.ctx, lineDef, map[string]any{
				"project_id":     projectID,
				"category":       l.category,
				"planned_amount": l.planned,
			}, s.actor); err != nil {
				log.Fatalf("create ProjectBudgetLine(%s): %v", l.category, err)
			}
		}
	}

	// Task has no single-field natural key — its title is an i18n map,
	// so getOrCreate's field lookup can't address it (an earlier draft
	// tried, matched nothing, and duplicated every task on re-run; the
	// idempotency test caught it). Index this project's existing tasks
	// by their English title instead, which repairs a partial run rather
	// than skipping wholesale.
	existingTasks, err := s.crud.ListByField(s.ctx, taskDef, "project_id", projectID)
	if err != nil {
		log.Fatalf("list existing tasks: %v", err)
	}
	taskByTitle := map[string]string{}
	for _, rec := range existingTasks {
		if title, ok := rec.Data["title"].(map[string]any); ok {
			if en, ok := title["en"].(string); ok {
				taskByTitle[en] = rec.ID
			}
		}
	}
	task := func(en string, fields map[string]any) string {
		if id, ok := taskByTitle[en]; ok {
			return id
		}
		rec, err := s.crud.Create(s.ctx, taskDef, fields, s.actor)
		if err != nil {
			log.Fatalf("create Task %q: %v", en, err)
		}
		taskByTitle[en] = rec.ID
		return rec.ID
	}

	parentID := task("Discovery", map[string]any{
		"project_id":      projectID,
		"title":           map[string]any{"en": "Discovery", "tr": "Keşif"},
		"assignee_id":     engineer,
		"estimated_hours": 40.0,
		"due_date":        "2026-03-15",
		"status_id":       s.statusID("task_status", "in_progress"),
	})
	// A subtask: the row that has parent_task_id set where its sibling
	// does not — the shape that used to render a ragged task table.
	subID := task("Stakeholder interviews", map[string]any{
		"project_id":      projectID,
		"parent_task_id":  parentID,
		"title":           map[string]any{"en": "Stakeholder interviews", "tr": "Paydaş görüşmeleri"},
		"assignee_id":     engineer,
		"estimated_hours": 12.0,
		"status_id":       s.statusID("task_status", "todo"),
	})

	loggedFor, err := s.crud.ListByField(s.ctx, timeDef, "task_id", parentID)
	if err != nil {
		log.Fatalf("list existing time entries: %v", err)
	}
	if len(loggedFor) > 0 {
		return
	}
	for _, e := range []struct {
		task  string
		date  string
		hours float64
		note  string
	}{
		{parentID, "2026-02-03", 6.5, "Kickoff and current-state review"},
		{parentID, "2026-02-04", 4.0, "Process mapping"},
		{subID, "2026-02-05", 3.25, "Finance team interview"},
	} {
		if _, err := s.crud.Create(s.ctx, timeDef, map[string]any{
			"task_id":     e.task,
			"employee_id": engineer,
			"entry_date":  e.date,
			"hours":       e.hours,
			"billable":    true,
			"notes":       e.note,
		}, s.actor); err != nil {
			log.Fatalf("create TimeEntry: %v", err)
		}
	}
}

// seedHR gives the demo tenant one employment and a leave request
// against it. Deliberately reuses the same "Demo Engineer" Party that
// seedProjects creates and tags with the `employee` PartyRole, which is
// what makes ADR-0013's shape visible in the data: one Party, an
// Employee record that points at it and carries only the employment,
// and a LeaveRequest hanging off the employment rather than the person.
func (s *seeder) seedHR() {
	if !hasPublished(s.ctx, s.entityDefs, "LeaveRequest") {
		return
	}
	// The Party comes first whether or not Projects is licensed — the
	// employee role is HR's own precondition, not a projects artifact.
	person := s.getOrCreate("Party", "name", "Demo Engineer", map[string]any{
		"party_type": "person", "name": "Demo Engineer", "status": "active",
	})
	s.getOrCreate("PartyRole", "party_id", person, map[string]any{
		"party_id": person, "role_type": "employee",
	})

	employeeID := s.getOrCreate("Employee", "employee_number", "EMP-1001", map[string]any{
		"employee_number": "EMP-1001",
		"party_id":        person,
		"hire_date":       "2024-03-04",
		"status_id":       s.statusID("employee_status", "active"),
	})
	if hasPublished(s.ctx, s.entityDefs, "AttendanceRecord") {
		attDef := s.def("AttendanceRecord")
		existing, err := s.crud.ListByField(s.ctx, attDef, "employee_id", employeeID)
		if err != nil {
			log.Fatalf("list existing attendance: %v", err)
		}
		seen := map[string]bool{}
		for _, rec := range existing {
			if d, ok := rec.Data["entry_date"].(string); ok {
				seen[d] = true
			}
		}
		for _, a := range []struct {
			date   string
			hours  float64
			source string
		}{
			// One row per source, so the demo tenant exercises every
			// enum value's translation rather than two-thirds of them.
			{"2026-07-27", 7.5, "clock"},
			{"2026-07-28", 8.0, "timesheet"},
			{"2026-07-29", 4.0, "manual"},
		} {
			if seen[a.date] {
				continue
			}
			if _, err := s.crud.Create(s.ctx, attDef, map[string]any{
				"employee_id": employeeID, "entry_date": a.date,
				"hours_worked": a.hours, "source": a.source,
			}, s.actor); err != nil {
				log.Fatalf("create AttendanceRecord %s: %v", a.date, err)
			}
		}
	}

	s.getOrCreate("LeaveRequest", "request_number", "LV-2026-001", map[string]any{
		"request_number": "LV-2026-001",
		"employee_id":    employeeID,
		"leave_type":     "annual",
		"start_date":     "2026-08-10",
		"end_date":       "2026-08-14",
		"days":           5.0,
		"reason":         "Summer holiday",
		"status_id":      s.statusID("leave_request_status", "submitted"),
	})
}

// seedCases gives the demo tenant two support cases: one with genuine,
// coherent warranty context (a customer, a product, and the order that
// customer actually bought it on — see the named constants below) and
// one with none, because a case about an account or a delivery is just
// as real and the context fields are optional for exactly that reason.
func (s *seeder) seedCases(customers, items, soIDs map[string]string) {
	// Named keys, like every other seeder in this file. An earlier
	// draft picked one of each map by `range … break`, which Go
	// randomises — so the "warranty context" was three independently
	// random pointers and the cited order belonged to a different
	// customer on every run, with the item never a line on it. That is
	// the exact unenforced gap crm.go documents (#78), reproduced by
	// the demo data with near-certainty, and it made the tenant
	// non-reproducible between seeds (independent review).
	//
	// This triple is coherent: SO-2026-0001 is Doha Retail Group's
	// order and SKU-1001 is its only line.
	const (
		caseCustomer = "Doha Retail Group"
		caseItemSKU  = "SKU-1001"
		caseOrder    = "SO-2026-0001"
	)
	customerID := customers[caseCustomer]
	itemID := items[caseItemSKU]
	soID := soIDs[caseOrder]

	withContext := map[string]any{
		"case_number": "CASE-2026-001",
		"subject":     "Delivered unit fails self-test",
		"description": "Customer reports the unit powers on but fails its self-test on start-up.",
		"customer_id": customerID,
		"priority":    "high",
		"opened_date": "2026-07-29",
		"sla_due_at":  "2026-07-31",
		"status_id":   s.statusID("case_status", "in_progress"),
		// A Party, not an hr.Employee — ADR-0013 rule 4, and the demo
		// data should show the decision rather than leave the field
		// blank while a comment argues about it.
		"assignee_id": s.getOrCreate("Party", "name", "Demo Support Agent", map[string]any{
			"party_type": "person", "name": "Demo Support Agent", "status": "active",
		}),
	}
	// No emptiness guards: seedItems and seedSalesOrders run
	// unconditionally before this, and s.def log.Fatalfs on an
	// unpublished type, so reaching here without them is impossible —
	// a guard would only imply a partial-licensing path this seeder
	// does not have (independent review).
	withContext["item_id"] = itemID
	withContext["sales_order_id"] = soID
	s.getOrCreate("Case", "case_number", "CASE-2026-001", withContext)

	s.getOrCreate("Case", "case_number", "CASE-2026-002", map[string]any{
		"case_number": "CASE-2026-002",
		"subject":     "Invoice address needs correcting",
		"customer_id": customerID,
		"priority":    "low",
		"opened_date": "2026-07-30",
		"status_id":   s.statusID("case_status", "new"),
	})
}

// ensureContactFor wires up ADR-0014's Contact shape and returns the
// person's Party id: a person Party, the `contact` PartyRole, and a
// contact_for PartyRelationship pointing FROM the person TO the
// organization.
//
// The direction matters and is the whole reason this is a helper rather
// than three inline calls — contact_for runs person -> organization
// while its neighbour `employs` runs organization -> person, the kernel
// cannot enforce either, and a reversed row is indistinguishable from a
// correct one by inspection. Writing it in one place, with a test that
// asserts the ends, is what keeps ADR-0014's documented convention from
// being a comment nobody checks.
func (s *seeder) ensureContactFor(personName, organizationID string) string {
	personID := s.getOrCreate("Party", "name", personName, map[string]any{
		"party_type": "person", "name": personName, "status": "active",
	})
	s.ensurePartyRole(personID, "contact")

	relDef := s.def("PartyRelationship")
	existing, err := s.crud.ListByField(s.ctx, relDef, "party_id_from", personID)
	if err != nil {
		log.Fatalf("list PartyRelationship by party_id_from: %v", err)
	}
	for _, r := range existing {
		if r.Data["party_id_to"] == organizationID && r.Data["relationship_type"] == "contact_for" {
			return personID
		}
	}
	if _, err := s.crud.Create(s.ctx, relDef, map[string]any{
		"party_id_from":     personID,
		"party_id_to":       organizationID,
		"relationship_type": "contact_for",
	}, s.actor); err != nil {
		log.Fatalf("create contact_for PartyRelationship: %v", err)
	}
	return personID
}

// seedPipeline gives the demo tenant a CRM pipeline: two campaigns, three
// leads at different points in the funnel, and two open opportunities.
//
// Named keys throughout, and every cross-reference is coherent rather
// than "whichever record came back first" — the mistake #15's review
// caught in seedCases, where three independently random pointers were
// documented as warranty context. Specifically: the converted lead
// (Layla Hassan) names Doha Retail Group as its company, converts to a
// person Party who is a `contact_for` that same organization, and the
// opportunity that lead produced is against that same customer. A
// pipeline demo where the converted lead points at an unrelated account
// would teach the shape wrong.
func (s *seeder) seedPipeline(customers, currencies map[string]string) {
	const (
		expoCampaign  = "Gulf Expo 2026"
		emailCampaign = "Autumn Email Series"
		convertedLead = "Layla Hassan"
		leadCustomer  = "Doha Retail Group"
	)

	// The rep gets a real PartyRole. owner_id is one of the "any Party is
	// accepted" gaps crm.go lists (#78), and a demo tenant whose only
	// roleless person Party is the one five records point at would be
	// demonstrating the loophole — the opposite of what ADR-0013 clause 2
	// asks the sample data to do (independent review).
	salesRep := s.getOrCreate("Party", "name", "Demo Sales Rep", map[string]any{
		"party_type": "person", "name": "Demo Sales Rep", "status": "active",
	})
	s.ensurePartyRole(salesRep, "employee")

	expoID := s.getOrCreate("Campaign", "name", expoCampaign, map[string]any{
		"name":        expoCampaign,
		"channel":     "event",
		"budget":      25000.0,
		"currency_id": currencies["QAR"],
		"start_date":  "2026-09-01",
		"end_date":    "2026-09-30",
		"description": "Trade stand and follow-up programme at the regional expo.",
		"status_id":   s.statusID("campaign_status", "active"),
	})
	s.getOrCreate("Campaign", "name", emailCampaign, map[string]any{
		"name":        emailCampaign,
		"channel":     "email",
		"budget":      3000.0,
		"currency_id": currencies["USD"],
		"start_date":  "2026-10-01",
		"end_date":    "2026-11-15",
		"description": "Four-part sequence to dormant accounts.",
		"status_id":   s.statusID("campaign_status", "planned"),
	})

	// The converted lead, and the Contact it converted into. Doing this
	// in the demo tenant is the point: ADR-0014 says a contact is a
	// Party plus a role plus a relationship, and the kernel cannot yet
	// enforce any of that (#78), so the demo data is what shows the
	// intended shape rather than the loophole (ADR-0013 clause 2).
	contactPartyID := s.ensureContactFor(convertedLead, customers[leadCustomer])

	convertedLeadID := s.getOrCreate("Lead", "name", convertedLead, map[string]any{
		"name":               convertedLead,
		"company_name":       leadCustomer,
		"email":              "layla.hassan@example.com",
		"phone":              "+974 5555 0101",
		"source":             "referral",
		"owner_id":           salesRep,
		"converted_party_id": contactPartyID,
		"notes":              "Introduced by an existing account; now the buying contact.",
		"status_id":          s.statusID("lead_status", "converted"),
	})

	s.getOrCreate("Lead", "name", "Nadia Karim", map[string]any{
		"name":         "Nadia Karim",
		"company_name": "Northline Logistics",
		"email":        "nadia.karim@example.com",
		"phone":        "+974 5555 0102",
		"source":       "event",
		"campaign_id":  expoID,
		"owner_id":     salesRep,
		"notes":        "Visited the stand; asked for a fleet-scale quote.",
		"status_id":    s.statusID("lead_status", "qualified"),
	})
	s.getOrCreate("Lead", "name", "Tomas Berg", map[string]any{
		"name":         "Tomas Berg",
		"company_name": "Berg Manufacturing",
		"email":        "tomas.berg@example.com",
		"source":       "web",
		"owner_id":     salesRep,
		"status_id":    s.statusID("lead_status", "new"),
	})

	// The deal the converted lead produced — same customer the lead
	// named and the contact belongs to.
	s.getOrCreate("Opportunity", "name", leadCustomer+" — POS refresh", map[string]any{
		"name":                leadCustomer + " — POS refresh",
		"customer_id":         customers[leadCustomer],
		"lead_id":             convertedLeadID,
		"amount":              120000.0,
		"currency_id":         currencies["QAR"],
		"probability":         60.0,
		"expected_close_date": "2026-10-15",
		"owner_id":            salesRep,
		"description":         "Replace end-of-life terminals across eleven stores.",
		"status_id":           s.statusID("opportunity_stage", "negotiation"),
	})
	s.getOrCreate("Opportunity", "name", "London Fashion House — warehouse fit-out", map[string]any{
		"name":                "London Fashion House — warehouse fit-out",
		"customer_id":         customers["London Fashion House"],
		"amount":              45000.0,
		"currency_id":         currencies["GBP"],
		"probability":         30.0,
		"expected_close_date": "2026-12-01",
		"owner_id":            salesRep,
		"description":         "Racking, scanners and stock-count tooling for the new site.",
		"status_id":           s.statusID("opportunity_stage", "qualification"),
	})
}
