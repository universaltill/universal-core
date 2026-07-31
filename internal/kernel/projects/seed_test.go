package projects

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_projects_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "projects-test"}
}

func publishedDef(t *testing.T, tenantDB *sql.DB, entityType string) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), entityType)
	if err != nil {
		t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	return d
}

func TestAllDefinitionsValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Errorf("%s: %v", def.EntityType, err)
		}
		if def.Module != "projects" {
			t.Errorf("%s declares module %q, want projects", def.EntityType, def.Module)
		}
	}
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Errorf("form %s: %v", f.EntityType, err)
		}
	}
	// The employee/assignee references must target Party, not a
	// nonexistent Employee entity — see the package doc comment on why
	// this kernel has no separate employee master.
	for _, tc := range []struct{ entityType, field string }{
		{"Task", "assignee_id"},
		{"TimeEntry", "employee_id"},
		{"Project", "customer_id"},
	} {
		var def *entity.Definition
		for _, d := range All() {
			if d.EntityType == tc.entityType {
				def = d
			}
		}
		f, ok := def.FieldByName(tc.field)
		if !ok {
			t.Fatalf("%s has no %s field", tc.entityType, tc.field)
		}
		if f.Target != "Party" {
			t.Errorf("%s.%s targets %q, want Party (the Party-Role pattern, not a duplicate master table)", tc.entityType, tc.field, f.Target)
		}
	}
}

func TestPublish_PublishesEveryDefinition(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishForms(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}
	for _, et := range []string{"Project", "Task", "TimeEntry"} {
		if got := publishedDef(t, tenantDB, et); got.Module != "projects" {
			t.Errorf("%s published with module %q", et, got.Module)
		}
		if _, err := data.NewFormDefinitionRepo(tenantDB).GetPublished(ctx, et); err != nil {
			t.Errorf("%s form not published: %v", et, err)
		}
	}
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("re-Publish should be a no-op: %v", err)
	}
}

// Both status graphs, and the two shape decisions that distinguish
// them: a project returns from on_hold, and a task returns from done.
func TestPublishStatuses_BothGraphs(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	flags := map[string]map[string]bool{}
	graph := func(code string) (map[string]string, map[string]bool) {
		t.Helper()
		types, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", code)
		if err != nil || len(types) != 1 {
			t.Fatalf("StatusType %s: err=%v n=%d", code, err, len(types))
		}
		rows, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
		if err != nil {
			t.Fatalf("statuses for %s: %v", code, err)
		}
		byCode := map[string]string{}
		flags[code] = map[string]bool{}
		for _, r := range rows {
			c, _ := r.Data["code"].(string)
			if c == "" {
				continue
			}
			byCode[c] = r.ID
			if v, _ := r.Data["is_initial"].(bool); v {
				flags[code]["initial:"+c] = true
			}
			if v, _ := r.Data["is_terminal"].(bool); v {
				flags[code]["terminal:"+c] = true
			}
		}
		trs, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusTransition"), "status_type_id", types[0].ID)
		if err != nil {
			t.Fatalf("transitions for %s: %v", code, err)
		}
		edges := map[string]bool{}
		for _, tr := range trs {
			from, _ := tr.Data["from_status_id"].(string)
			to, _ := tr.Data["to_status_id"].(string)
			edges[from+"->"+to] = true
		}
		return byCode, edges
	}

	// Two graphs sharing the "cancelled" code must stay scoped to their
	// own status type — the 2026-07-29 collision bug's regression.
	proj, projEdges := graph("project_status")
	task, taskEdges := graph("task_status")
	if proj["cancelled"] == task["cancelled"] {
		t.Error("project_status and task_status must not share a Status row for the same code")
	}
	if len(proj) != 5 || len(task) != 5 {
		t.Errorf("expected 5 statuses each, got project=%d task=%d", len(proj), len(task))
	}
	if !projEdges[proj["on_hold"]+"->"+proj["active"]] {
		t.Error("a project must be able to resume from on_hold — pausing work is normal")
	}
	if !taskEdges[task["done"]+"->"+task["in_progress"]] {
		t.Error("a task must be able to reopen — forcing a duplicate would strand its logged time")
	}
	if projEdges[proj["completed"]+"->"+proj["active"]] {
		t.Error("a completed project must not reopen by status edit")
	}

	// A task must be blockable before it is started — "waiting on a
	// spec" is one of the commonest states a not-yet-started task sits
	// in, and forcing it through in_progress first falsifies the very
	// actuals this module exists to collect (independent review).
	if !taskEdges[task["todo"]+"->"+task["blocked"]] || !taskEdges[task["blocked"]+"->"+task["todo"]] {
		t.Error("todo <-> blocked must both be declared transitions")
	}

	// The lifecycle flags themselves: statusgraph.Seed validates
	// nothing, so without this a future edit could move is_initial and
	// every create would start failing with nothing to explain why.
	for code, want := range map[string]map[string]bool{
		"project_status": {"initial:planned": true, "terminal:completed": true, "terminal:cancelled": true},
		"task_status":    {"initial:todo": true, "terminal:done": true, "terminal:cancelled": true},
	} {
		for flag := range want {
			if !flags[code][flag] {
				t.Errorf("%s is missing %s", code, flag)
			}
		}
		initials := 0
		for f := range flags[code] {
			if strings.HasPrefix(f, "initial:") {
				initials++
			}
		}
		if initials != 1 {
			t.Errorf("%s has %d initial statuses, want exactly 1", code, initials)
		}
	}
}

// TestPublishStatuses_RequiresFoundation: publishing this module's
// statuses into a tenant without foundation is a real operator ordering
// mistake (nothing in this package depends on foundation at the Go
// level). It must fail cleanly, naming what is missing.
func TestPublishStatuses_RequiresFoundation(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	err := PublishStatuses(ctx, tenantDB, humanActor())
	if err == nil {
		t.Fatal("PublishStatuses without foundation should fail, not silently succeed")
	}
	if !strings.Contains(err.Error(), "StatusType") {
		t.Errorf("error should name the missing definition, got %v", err)
	}
}

// TestTask_SelfReferenceCycleRejected: parent_task_id is a
// self-referencing hierarchy, and ADR-0007 put cycle detection in
// crud.Engine generically so a new hierarchy inherits it. This asserts
// the inheritance actually holds for Task rather than assuming it.
func TestTask_SelfReferenceCycleRejected(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"projects", Publish},
		{"projects statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	status := func(typeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", typeCode)
		if len(types) == 0 {
			t.Fatalf("no StatusType %s", typeCode)
		}
		rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s status for %s", code, typeCode)
		return ""
	}

	projDef := publishedDef(t, tenantDB, "Project")
	project, err := engine.Create(ctx, projDef, map[string]any{
		"project_code": "PRJ-1", "name": map[string]any{"en": "Test Project"},
		"start_date": "2026-01-01", "status_id": status("project_status", "planned"),
	}, actor)
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}

	taskDef := publishedDef(t, tenantDB, "Task")
	parent, err := engine.Create(ctx, taskDef, map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Parent"},
		"status_id": status("task_status", "todo"),
	}, actor)
	if err != nil {
		t.Fatalf("create parent Task: %v", err)
	}
	child, err := engine.Create(ctx, taskDef, map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Child"},
		"parent_task_id": parent.ID, "status_id": status("task_status", "todo"),
	}, actor)
	if err != nil {
		t.Fatalf("create child Task: %v", err)
	}

	// Pointing the parent at its own child closes a loop; the engine's
	// generic guard must refuse it.
	version := parent.Version
	_, err = engine.Update(ctx, taskDef, parent.ID, map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Parent"},
		"parent_task_id": child.ID, "status_id": status("task_status", "todo"),
	}, &version, actor)
	if err == nil {
		t.Fatal("a parent_task_id cycle must be rejected (ADR-0007's generic guard)")
	}

	// A task pointing at itself is the degenerate case.
	v2 := child.Version
	if _, err := engine.Update(ctx, taskDef, child.ID, map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Child"},
		"parent_task_id": child.ID, "status_id": status("task_status", "todo"),
	}, &v2, actor); err == nil {
		t.Fatal("a task must not be its own parent")
	}
}

// TestTimeEntry_UsableEndToEnd: hours logged against a task by a Party
// holding the employee role, surfacing through the task's related list.
func TestTimeEntry_UsableEndToEnd(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"projects", Publish},
		{"projects statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	statusFor := func(typeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", typeCode)
		rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s/%s", typeCode, code)
		return ""
	}

	// The "employee" is a Party holding the employee PartyRole — the
	// whole point of this module having no Employee entity.
	partyDef := publishedDef(t, tenantDB, "Party")
	person, err := engine.Create(ctx, partyDef, map[string]any{
		"party_type": "person", "name": "Demo Engineer", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "PartyRole"), map[string]any{
		"party_id": person.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("create employee PartyRole: %v", err)
	}

	project, err := engine.Create(ctx, publishedDef(t, tenantDB, "Project"), map[string]any{
		"project_code": "PRJ-2", "name": map[string]any{"en": "Billable Work"},
		"start_date": "2026-02-01", "status_id": statusFor("project_status", "active"),
	}, actor)
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}
	taskDef := publishedDef(t, tenantDB, "Task")
	task, err := engine.Create(ctx, taskDef, map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Write the thing"},
		"assignee_id": person.ID, "estimated_hours": 8.0,
		"status_id": statusFor("task_status", "in_progress"),
	}, actor)
	if err != nil {
		t.Fatalf("create Task: %v", err)
	}

	teDef := publishedDef(t, tenantDB, "TimeEntry")
	for _, hours := range []float64{3.5, 2.25} {
		if _, err := engine.Create(ctx, teDef, map[string]any{
			"task_id": task.ID, "employee_id": person.ID,
			"entry_date": "2026-02-03", "hours": hours, "billable": true,
		}, actor); err != nil {
			t.Fatalf("create TimeEntry: %v", err)
		}
	}

	// The task's related list — the query the task form runs.
	logged, err := engine.ListByField(ctx, teDef, "task_id", task.ID)
	if err != nil || len(logged) != 2 {
		t.Fatalf("task's time entries: err=%v n=%d", err, len(logged))
	}
	var total float64
	for _, e := range logged {
		h, _ := e.Data["hours"].(float64)
		total += h
	}
	if total != 5.75 {
		t.Errorf("logged hours = %v, want 5.75", total)
	}
}
