package recordmigrate_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/universaltill/universal-core/internal/kernel/recordmigrate"
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

	name := fmt.Sprintf("uc_test_recmig_%d", time.Now().UnixNano())
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
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("apply tenant migrations: %v", err)
	}
	return tenantDB
}

func actor() audit.Actor { return audit.Actor{Type: audit.ActorHuman, ID: "recordmigrate-test"} }

// widgetDef is a deliberately generic fixture — recordmigrate must not
// know or care about any real entity type, so testing it against
// PurchaseOrder or InventoryItem would prove less, not more.
func widgetDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "MigrationWidget",
		Version:    1,
		Module:     "test",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "legacy_code", Type: entity.FieldString},
			{Name: "new_code", Type: entity.FieldString},
			{Name: "keep_me", Type: entity.FieldString},
		},
	}
}

func setup(t *testing.T) (context.Context, *crud.Engine, *entity.Definition) {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	def := widgetDef()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	repo := data.NewEntityDefinitionRepo(tenantDB)
	if _, err := repo.CreateDraft(ctx, def.EntityType, def.Version, raw, actor()); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if err := repo.Approve(ctx, def.EntityType, def.Version, actor()); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := repo.Publish(ctx, def.EntityType, def.Version, actor()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return ctx, crud.NewEngine(tenantDB), def
}

func discard(string, ...any) {}

// The core contract: migrate what needs it, leave what doesn't, skip
// what can't, and count all three separately.
func TestRun_MigratesSkipsAndCounts(t *testing.T) {
	ctx, engine, def := setup(t)

	mk := func(fields map[string]any) string {
		t.Helper()
		rec, err := engine.Create(ctx, def, fields, actor())
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return rec.ID
	}
	toMigrate := mk(map[string]any{"name": "A", "legacy_code": "old-a", "keep_me": "keep-a"})
	done := mk(map[string]any{"name": "B", "new_code": "already-b", "keep_me": "keep-b"})
	unmigratable := mk(map[string]any{"name": "C", "keep_me": "keep-c"})

	var logs []string
	res, err := recordmigrate.Run(ctx, engine, def,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			if v, _ := rec.Data["new_code"].(string); v != "" {
				return nil, recordmigrate.AlreadyDone, ""
			}
			legacy, _ := rec.Data["legacy_code"].(string)
			if legacy == "" {
				return nil, recordmigrate.Skip, "no legacy_code to convert"
			}
			fields := recordmigrate.CopyExcept(rec.Data, "legacy_code")
			fields["new_code"] = strings.TrimPrefix(legacy, "old-")
			return fields, recordmigrate.Migrate, ""
		},
		actor(),
		recordmigrate.Options{Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Migrated != 1 || res.AlreadyDone != 1 || res.Skipped != 1 {
		t.Errorf("Result = %+v, want 1 migrated / 1 already done / 1 skipped", res)
	}

	// The skip reason must reach the log naming the record, or nobody
	// can act on it.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, unmigratable) || !strings.Contains(joined, "no legacy_code to convert") {
		t.Errorf("skip warning must name the record and the reason; got:\n%s", joined)
	}

	got, err := engine.Get(ctx, def, toMigrate)
	if err != nil {
		t.Fatalf("get migrated record: %v", err)
	}
	if v, _ := got.Data["new_code"].(string); v != "a" {
		t.Errorf("new_code = %q, want %q", v, "a")
	}
	// Full field replacement, both halves: the old key is gone and
	// every unrelated field survived. A transform returning only its own
	// changes would silently erase keep_me — the failure this rule
	// exists to prevent.
	if _, present := got.Data["legacy_code"]; present {
		t.Error("legacy_code must be dropped — omitting it from the returned map is how a key is removed")
	}
	if v, _ := got.Data["keep_me"].(string); v != "keep-a" {
		t.Errorf("keep_me = %q, want keep-a: unrelated fields must survive the migration", v)
	}
	if v, _ := got.Data["name"].(string); v != "A" {
		t.Errorf("name = %q, want A", v)
	}

	// An already-done record must not be rewritten at all — not merely
	// left semantically equal. A pointless Update would bump its version
	// and write a misleading audit row.
	untouched, err := engine.Get(ctx, def, done)
	if err != nil {
		t.Fatalf("get already-done record: %v", err)
	}
	if untouched.Version != 1 {
		t.Errorf("already-done record is at version %d, want 1 — it must not be rewritten", untouched.Version)
	}
}

// A dry run must report exactly what a real run would do, and write
// nothing. The ordering that makes the first half true — validating
// before the dry-run branch — is the subtle part recordmigrate exists
// to share.
func TestRun_DryRunPreviewMatchesRealRunAndWritesNothing(t *testing.T) {
	ctx, engine, def := setup(t)

	goodRec, err := engine.Create(ctx, def, map[string]any{"name": "good", "legacy_code": "old-x"}, actor())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := engine.Create(ctx, def, map[string]any{"name": "bad", "legacy_code": "old-y"}, actor()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The "bad" record's transform produces a record missing the
	// Required `name` — the shape of a legacy row that this migration
	// has no value to supply. It must be counted as skipped in BOTH a
	// dry run and a real run.
	transform := func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
		fields := recordmigrate.CopyExcept(rec.Data, "legacy_code")
		fields["new_code"] = "converted"
		if n, _ := rec.Data["name"].(string); n == "bad" {
			delete(fields, "name")
		}
		return fields, recordmigrate.Migrate, ""
	}

	dry, err := recordmigrate.Run(ctx, engine, def, transform, actor(),
		recordmigrate.Options{DryRun: true, Logf: discard})
	if err != nil {
		t.Fatalf("dry Run: %v", err)
	}
	if dry.Migrated != 1 || dry.Skipped != 1 {
		t.Errorf("dry run Result = %+v, want 1 migrated / 1 skipped — validation must run before the dry-run branch, or the preview promises a migration that would actually fail", dry)
	}

	// Nothing written.
	unchanged, err := engine.Get(ctx, def, goodRec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if unchanged.Version != 1 {
		t.Errorf("dry run wrote to the record: version %d, want 1", unchanged.Version)
	}
	if _, present := unchanged.Data["new_code"]; present {
		t.Error("dry run must not write new_code")
	}

	// And the real run agrees with the preview, exactly.
	real, err := recordmigrate.Run(ctx, engine, def, transform, actor(),
		recordmigrate.Options{Logf: discard})
	if err != nil {
		t.Fatalf("real Run: %v", err)
	}
	if real != dry {
		t.Errorf("real run %+v disagrees with dry-run preview %+v", real, dry)
	}
}

// A record whose transform fails validation is skipped, not fatal — a
// batch that stops dead on one bad legacy row is worse than one that
// finishes and names the rows needing a human.
func TestRun_InvalidRecordIsSkippedNotFatal(t *testing.T) {
	ctx, engine, def := setup(t)
	for _, n := range []string{"one", "two", "three"} {
		if _, err := engine.Create(ctx, def, map[string]any{"name": n}, actor()); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	res, err := recordmigrate.Run(ctx, engine, def,
		func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
			fields := recordmigrate.CopyExcept(rec.Data)
			fields["new_code"] = "x"
			if n, _ := rec.Data["name"].(string); n == "two" {
				delete(fields, "name") // violates Required
			}
			return fields, recordmigrate.Migrate, ""
		},
		actor(), recordmigrate.Options{Logf: discard})
	if err != nil {
		t.Fatalf("one bad record must not fail the run: %v", err)
	}
	if res.Migrated != 2 || res.Skipped != 1 {
		t.Errorf("Result = %+v, want 2 migrated / 1 skipped", res)
	}
}

// A Transform that returns no action does nothing. The zero Action is
// deliberately not Migrate, so a caller that forgets to set one cannot
// accidentally write whatever the transform happened to return.
func TestRun_ZeroActionWritesNothing(t *testing.T) {
	ctx, engine, def := setup(t)
	rec, err := engine.Create(ctx, def, map[string]any{"name": "untouched"}, actor())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := recordmigrate.Run(ctx, engine, def,
		func(data.Record) (map[string]any, recordmigrate.Action, string) {
			return map[string]any{"name": "clobbered"}, 0, ""
		},
		actor(), recordmigrate.Options{Logf: discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Migrated != 0 || res.Skipped != 1 {
		t.Errorf("Result = %+v, want 0 migrated / 1 skipped", res)
	}
	after, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v, _ := after.Data["name"].(string); v != "untouched" {
		t.Errorf("name = %q — a transform with no action must not write", v)
	}
}

// Re-running a completed migration is a no-op, which is what makes
// recovering from a partial failure safe.
func TestRun_IsIdempotent(t *testing.T) {
	ctx, engine, def := setup(t)
	for _, n := range []string{"a", "b"} {
		if _, err := engine.Create(ctx, def, map[string]any{"name": n, "legacy_code": "old"}, actor()); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	transform := func(rec data.Record) (map[string]any, recordmigrate.Action, string) {
		if v, _ := rec.Data["new_code"].(string); v != "" {
			return nil, recordmigrate.AlreadyDone, ""
		}
		fields := recordmigrate.CopyExcept(rec.Data, "legacy_code")
		fields["new_code"] = "done"
		return fields, recordmigrate.Migrate, ""
	}
	first, err := recordmigrate.Run(ctx, engine, def, transform, actor(), recordmigrate.Options{Logf: discard})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Migrated != 2 {
		t.Errorf("first run migrated %d, want 2", first.Migrated)
	}
	second, err := recordmigrate.Run(ctx, engine, def, transform, actor(), recordmigrate.Options{Logf: discard})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Migrated != 0 || second.AlreadyDone != 2 {
		t.Errorf("second run = %+v, want 0 migrated / 2 already done", second)
	}
}

// Logf is required rather than optional-with-a-default: a migration
// that silently discarded its skip warnings would report "3 skipped"
// with no way to learn which three.
func TestRun_RequiresLogf(t *testing.T) {
	ctx, engine, def := setup(t)
	_, err := recordmigrate.Run(ctx, engine, def,
		func(data.Record) (map[string]any, recordmigrate.Action, string) {
			return nil, recordmigrate.AlreadyDone, ""
		},
		actor(), recordmigrate.Options{})
	if err == nil {
		t.Error("Run must reject Options with no Logf")
	}
}

func TestCopyExcept(t *testing.T) {
	src := map[string]any{"a": 1, "b": 2, "c": 3}
	got := recordmigrate.CopyExcept(src, "b")
	if _, present := got["b"]; present {
		t.Error("b must be omitted")
	}
	if got["a"] != 1 || got["c"] != 3 {
		t.Errorf("got %v, want a and c preserved", got)
	}
	// A copy, not a view — mutating the result must not touch the
	// record's own data, which the caller may still read.
	got["a"] = 99
	if src["a"] != 1 {
		t.Error("CopyExcept must return a copy, not alias the source map")
	}
	if n := len(recordmigrate.CopyExcept(src)); n != 3 {
		t.Errorf("omitting nothing must copy everything; got %d keys", n)
	}
}
