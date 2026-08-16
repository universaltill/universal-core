package projects

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

// Publish brings a tenant's Projects module online — same mechanism as
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
			return fmt.Errorf("projects definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Projects Form Definitions online —
// separate from Publish for the same reason every other module keeps
// them separate (a form is a presentation choice, not the entity
// guarantee).
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("projects form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// projectStatusSpecs/taskStatusSpecs are package-level (not inlined into
// PublishStatuses below) so StatusSpecs can expose the identical
// literals — Translations included — to cmd/backfill-status-name-
// translations (uc-infra#244) without a second, driftable copy.
// Translations are the same words already shipped in
// field.Project.status_id.* / field.Task.status_id.* in
// internal/i18n/locales/{ar,fa,tr}.json.
var projectStatusSpecs = []statusgraph.Spec{
	{Code: "planned", Name: "Planned", Translations: map[string]string{"ar": "مخطط", "fa": "برنامه‌ریزی‌شده", "tr": "Planlandı"}, Sequence: 1, IsInitial: true},
	{Code: "active", Name: "Active", Translations: map[string]string{"ar": "نشط", "fa": "فعال", "tr": "Aktif"}, Sequence: 2},
	{Code: "on_hold", Name: "On Hold", Translations: map[string]string{"ar": "معلق", "fa": "متوقف", "tr": "Beklemede"}, Sequence: 3},
	{Code: "completed", Name: "Completed", Translations: map[string]string{"ar": "مكتمل", "fa": "تکمیل‌شده", "tr": "Tamamlandı"}, Sequence: 4, IsTerminal: true},
	{Code: "cancelled", Name: "Cancelled", Translations: map[string]string{"ar": "ملغى", "fa": "لغوشده", "tr": "İptal Edildi"}, Sequence: 5, IsTerminal: true},
}

var taskStatusSpecs = []statusgraph.Spec{
	{Code: "todo", Name: "To Do", Translations: map[string]string{"ar": "قيد الانتظار", "fa": "انجام‌نشده", "tr": "Yapılacak"}, Sequence: 1, IsInitial: true},
	{Code: "in_progress", Name: "In Progress", Translations: map[string]string{"ar": "قيد التنفيذ", "fa": "در حال انجام", "tr": "Devam Ediyor"}, Sequence: 2},
	{Code: "blocked", Name: "Blocked", Translations: map[string]string{"ar": "محجوب", "fa": "مسدود", "tr": "Engellendi"}, Sequence: 3},
	// Terminal is descriptive, not enforcing (nothing in
	// ValidateStatusTransition reads it), so flagging done does not
	// conflict with the reopening edge below — it is what lets a
	// filter or report tell a finished task from a live one.
	{Code: "done", Name: "Done", Translations: map[string]string{"ar": "منجز", "fa": "انجام‌شده", "tr": "Tamamlandı"}, Sequence: 4, IsTerminal: true},
	{Code: "cancelled", Name: "Cancelled", Translations: map[string]string{"ar": "ملغى", "fa": "لغوشده", "tr": "İptal Edildi"}, Sequence: 5, IsTerminal: true},
}

// StatusSpecs exposes this module's Status Specs (the identical literals
// PublishStatuses passes to statusgraph.Seed, including Translations),
// keyed by StatusTypeCode — see purchasing.StatusSpecs's own doc comment
// for why (cmd/backfill-status-name-translations, uc-infra#244).
func StatusSpecs() map[string][]statusgraph.Spec {
	return map[string][]statusgraph.Spec{
		"project_status": statusgraph.CopySpecs(projectStatusSpecs),
		"task_status":    statusgraph.CopySpecs(taskStatusSpecs),
	}
}

// PublishStatuses seeds the StatusType/Status/StatusTransition records
// Project's and Task's StatusTypeCodes need before the guarded engine
// will accept a create (same requirement and shape as every other
// module's PublishStatuses; the shared seeder is
// internal/kernel/statusgraph).
//
// project_status: planned -> active -> completed is the happy path.
// on_hold is a real state a project returns FROM — unlike a document's
// approval chain, pausing and resuming work is normal, so active
// <-> on_hold is bidirectional. cancelled is terminal and reachable
// from any live state; completed is terminal because reopening finished
// work is a new project or a new task, not a status edit.
//
// task_status: todo -> in_progress -> done, with blocked reachable
// from — and returning to — BOTH todo and in_progress (being blocked is
// temporary by definition, and a task can be blocked before anyone
// starts it). cancelled is terminal from any live state. Unlike a
// project, a task CAN return from done to in_progress: work that turns
// out to be incomplete is the same task continuing, and forcing a
// duplicate would corrupt the time entries already logged against it.
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
		"Project", "project_status", "Project Status",
		projectStatusSpecs,
		[][2]string{
			{"planned", "active"},
			{"active", "on_hold"},
			{"on_hold", "active"},
			{"active", "completed"},
			{"planned", "cancelled"},
			{"active", "cancelled"},
			{"on_hold", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed project_status: %w", err)
	}

	if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"Task", "task_status", "Task Status",
		taskStatusSpecs,
		[][2]string{
			{"todo", "in_progress"},
			// Blocked is reachable from todo, not only from in_progress:
			// "waiting on a spec" before kickoff is one of the commonest
			// states a task sits in, and the alternative — start it just
			// to block it — falsifies the actuals this module exists to
			// collect (independent review).
			{"todo", "blocked"},
			{"blocked", "todo"},
			{"in_progress", "blocked"},
			{"blocked", "in_progress"},
			{"in_progress", "done"},
			// Reopening: the same task continuing, so the time already
			// logged against it stays attached.
			{"done", "in_progress"},
			{"todo", "cancelled"},
			{"in_progress", "cancelled"},
			{"blocked", "cancelled"},
		},
		actor,
	); err != nil {
		return fmt.Errorf("seed task_status: %w", err)
	}
	return nil
}
