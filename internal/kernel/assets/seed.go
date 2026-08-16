package assets

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

// Publish brings a tenant's Assets module online — same mechanism as
// purchasing.Publish/sales.Publish (see purchasing's own doc comment
// for the idempotency/resume/concurrency contract this inherits
// unchanged, and for why the caller decides whether a tenant has
// licensed the module rather than this function checking).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	defs := All()
	items := make([]moduleseed.Item, 0, len(defs))
	for _, def := range defs {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("assets definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Assets Form Definitions online —
// separate from Publish for the same reason every other module keeps
// them separate (a form is a presentation choice, not the entity
// guarantee).
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("assets form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// fixedAssetStatusSpecs/maintenanceOrderStatusSpecs are package-level
// (not inlined into PublishStatuses below) so StatusSpecs can expose the
// identical literals — Translations included — to cmd/backfill-status-
// name-translations (uc-infra#244) without a second, driftable copy.
// Translations are the same words already shipped in
// field.FixedAsset.status_id.* / field.MaintenanceOrder.status_id.* in
// internal/i18n/locales/{ar,fa,tr}.json.
var fixedAssetStatusSpecs = []statusgraph.Spec{
	{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}, Sequence: 1, IsInitial: true},
	{Code: "in_service", Name: "In Service", Translations: map[string]string{"ar": "قيد الاستخدام", "fa": "در حال استفاده", "tr": "Kullanımda"}, Sequence: 2},
	{Code: "fully_depreciated", Name: "Fully Depreciated", Translations: map[string]string{"ar": "مستهلك بالكامل", "fa": "مستهلک‌شده", "tr": "Tamamen Amortismana Tabi"}, Sequence: 3},
	{Code: "disposed", Name: "Disposed", Translations: map[string]string{"ar": "مستبعد", "fa": "واگذارشده", "tr": "Elden Çıkarıldı"}, Sequence: 4, IsTerminal: true},
	{Code: "written_off", Name: "Written Off", Translations: map[string]string{"ar": "مشطوب", "fa": "حذف‌شده", "tr": "Kayıttan Düşüldü"}, Sequence: 5, IsTerminal: true},
}

var maintenanceOrderStatusSpecs = []statusgraph.Spec{
	{Code: "scheduled", Name: "Scheduled", Translations: map[string]string{"ar": "مجدول", "fa": "برنامه‌ریزی‌شده", "tr": "Planlandı"}, Sequence: 1, IsInitial: true},
	{Code: "in_progress", Name: "In Progress", Translations: map[string]string{"ar": "قيد التنفيذ", "fa": "در حال انجام", "tr": "Devam Ediyor"}, Sequence: 2},
	{Code: "completed", Name: "Completed", Translations: map[string]string{"ar": "مكتمل", "fa": "تکمیل‌شده", "tr": "Tamamlandı"}, Sequence: 3, IsTerminal: true},
	{Code: "cancelled", Name: "Cancelled", Translations: map[string]string{"ar": "ملغى", "fa": "لغوشده", "tr": "İptal Edildi"}, Sequence: 4, IsTerminal: true},
}

// StatusSpecs exposes this module's Status Specs (the identical literals
// PublishStatuses passes to statusgraph.Seed, including Translations),
// keyed by StatusTypeCode — see purchasing.StatusSpecs's own doc comment
// for why (cmd/backfill-status-name-translations, uc-infra#244).
func StatusSpecs() map[string][]statusgraph.Spec {
	return map[string][]statusgraph.Spec{
		"fixed_asset_status":       statusgraph.CopySpecs(fixedAssetStatusSpecs),
		"maintenance_order_status": statusgraph.CopySpecs(maintenanceOrderStatusSpecs),
	}
}

// PublishStatuses seeds the StatusType/Status/StatusTransition records
// FixedAsset's StatusTypeCode needs before the guarded engine will
// accept a create (same requirement and same shape as purchasing's and
// sales's own PublishStatuses; the shared seeder is
// internal/kernel/statusgraph).
//
// fixed_asset_status models an asset's real life, not a document's
// approval: `draft` is the only initial state (registered but not yet
// in service, so not yet depreciating); `in_service` is where
// depreciation runs; `fully_depreciated` is reached when the schedule
// is exhausted; `disposed` and `written_off` are the two terminal exits
// and are reachable from any live state, because an asset can be sold
// or destroyed at any point in its life — unlike an order's status
// graph, where skipping steps means skipping an approval. Nothing
// returns from a terminal state: un-disposing an asset is a new
// acquisition, not a status edit.
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
		"FixedAsset", "fixed_asset_status", "Fixed Asset Status",
		fixedAssetStatusSpecs,
		[][2]string{
			{"draft", "in_service"},
			{"in_service", "fully_depreciated"},
			// Disposal and write-off can happen at any point in an
			// asset's life, including before it enters service.
			{"draft", "disposed"},
			{"in_service", "disposed"},
			{"fully_depreciated", "disposed"},
			{"draft", "written_off"},
			{"in_service", "written_off"},
			{"fully_depreciated", "written_off"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed fixed_asset_status: %w", err)
	}

	// maintenance_order_status: scheduled -> in_progress -> completed is
	// the happy path. cancelled is reachable from either live state but
	// not from completed — work that was actually done cannot be
	// un-done by a status edit; reversing its cost is a credit note,
	// the same reasoning sales.PublishStatuses gives for a paid invoice.
	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"MaintenanceOrder", "maintenance_order_status", "Maintenance Order Status",
		maintenanceOrderStatusSpecs,
		[][2]string{
			{"scheduled", "in_progress"},
			{"in_progress", "completed"},
			{"scheduled", "cancelled"},
			{"in_progress", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed maintenance_order_status: %w", err)
	}
	return nil
}
