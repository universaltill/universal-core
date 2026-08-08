// Smoke tests for the real compiled cmd/install-module binary — the
// bundle-fixture install driven exactly the way an operator runs it.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "install-module")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func run(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return testexec.Run(t, binPath, env, args...)
}

func fixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "internal", "kernel", "modulebundle", "testdata", "library_bundle.json"))
	if err != nil {
		t.Fatalf("resolve fixture path: %v", err)
	}
	return p
}

// TestValidateMode_NeedsNoDatabase: -validate parses and validates with
// no DATABASE_URL at all — the property that makes it usable as a
// bundle-authoring check.
func TestValidateMode_NeedsNoDatabase(t *testing.T) {
	stdout, stderr, code := run(t, []string{}, "-validate", "-bundle", fixturePath(t))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr+stdout, "is valid") {
		t.Errorf("expected validity confirmation, got: %s%s", stdout, stderr)
	}
}

func TestValidateMode_RejectsBrokenBundle(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"format":"erp_module/v9"}`), 0o600); err != nil {
		t.Fatalf("write bad bundle: %v", err)
	}
	stdout, stderr, code := run(t, []string{}, "-validate", "-bundle", bad)
	if code == 0 {
		t.Fatalf("expected non-zero exit for an invalid bundle: %s%s", stdout, stderr)
	}
	if !strings.Contains(stderr+stdout, "unsupported bundle format") {
		t.Errorf("expected format error, got: %s%s", stdout, stderr)
	}
}

// TestInstall_EndToEndViaRealBinary: provision a tenant with foundation
// published, run the binary, and confirm the module's definitions are
// published in that tenant's registry.
func TestInstall_EndToEndViaRealBinary(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	controlDSN := testexec.FreshDatabase(t, "uc_test_installmod_control")
	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	tenantID, err := router.Create(ctx, "Install Module Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	if err := foundation.Publish(ctx, tenantDB, audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", tenantID, "-actor-id", "smoke-test")
	if code != 0 {
		t.Fatalf("install exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr+stdout, `module "library" installed`) {
		t.Errorf("expected install confirmation, got: %s%s", stdout, stderr)
	}

	for _, et := range []string{"LibraryBook", "LibraryLoan", "LibraryLoanLine"} {
		if _, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, et); err != nil {
			t.Errorf("%s not published after install: %v", et, err)
		}
	}

	// Second run: idempotent, exit 0.
	_, stderr2, code2 := run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", tenantID, "-actor-id", "smoke-test")
	if code2 != 0 {
		t.Fatalf("re-install should succeed, exited %d: %s", code2, stderr2)
	}
}

// TestInstall_ReportsBlockedItemsAndFailsViaRealBinary is the smoke-test
// counterpart of modulebundle's TestInstall_ReportsRolledBackVersionAsBlocked
// (uc-infra#73): drives the real compiled binary, not just the library
// function, through the exact operator-visible symptom the issue
// reported — a rolled-back version must produce a loud warning and a
// non-zero exit, never the bare "installed" success line.
func TestInstall_ReportsBlockedItemsAndFailsViaRealBinary(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	controlDSN := testexec.FreshDatabase(t, "uc_test_installmod_blocked")
	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	tenantID, err := router.Create(ctx, "Install Module Blocked Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", tenantID, "-actor-id", "smoke-test")
	if code != 0 {
		t.Fatalf("first install exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	if err := data.NewEntityDefinitionRepo(tenantDB).Rollback(ctx, "LibraryLoan", 1, actor); err != nil {
		t.Fatalf("rollback LibraryLoan entity v1: %v", err)
	}
	if err := data.NewFormDefinitionRepo(tenantDB).Rollback(ctx, "LibraryLoan", 1, actor); err != nil {
		t.Fatalf("rollback LibraryLoan form v1: %v", err)
	}

	stdout, stderr, code = run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", tenantID, "-actor-id", "smoke-test")
	combined := stdout + stderr
	if code == 0 {
		t.Fatalf("re-install with a rolled-back item should exit non-zero, got 0: %s", combined)
	}
	if !strings.Contains(combined, "WARNING") || !strings.Contains(combined, "LibraryLoan") || !strings.Contains(combined, "rolled_back") {
		t.Errorf("expected a loud WARNING naming LibraryLoan/rolled_back, got: %s", combined)
	}
	// A stable prefix, not the exact full line + trailing newline: the
	// failure line deliberately reads "was NOT fully installed into
	// tenant" (independent review — an operator grepping logs for the
	// unqualified phrase must not get a false positive on a run that
	// left items behind), so this substring can never appear in it
	// regardless of how either message's punctuation evolves later.
	if strings.Contains(combined, `"library" installed into tenant`) {
		t.Errorf("must not print the unqualified success line when items were left behind: %s", combined)
	}
	if !strings.Contains(combined, "was NOT fully installed") {
		t.Errorf("expected the qualified failure line, got: %s", combined)
	}

	// The unaffected entity/form types must still have published fine —
	// this is a partial report, not an all-or-nothing failure.
	for _, et := range []string{"LibraryBook", "LibraryLoanLine"} {
		if _, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, et); err != nil {
			t.Errorf("%s should still be published: %v", et, err)
		}
	}
}

func TestMissingFlags(t *testing.T) {
	if _, stderr, code := run(t, []string{}); code == 0 || !strings.Contains(stderr, "-bundle is required") {
		t.Errorf("expected -bundle requirement, got code %d: %s", code, stderr)
	}
	if _, stderr, code := run(t, []string{}, "-bundle", fixturePath(t)); code == 0 || !strings.Contains(stderr, "-tenant-id is required") {
		t.Errorf("expected -tenant-id requirement, got code %d: %s", code, stderr)
	}
	if _, stderr, code := run(t, []string{}, "-bundle", fixturePath(t), "-tenant-id", "x"); code == 0 || !strings.Contains(stderr, "-actor-id is required") {
		t.Errorf("expected -actor-id requirement, got code %d: %s", code, stderr)
	}
}

// An ai_agent install must carry a model version — ADR-0001 §14 makes
// AI-actor identity first-class, and audit.Actor.Validate enforces it.
// Hard-coding ActorHuman (the pre-review shape) would have written a
// falsified actor_type for every unattended pipeline install.
func TestActorTypeValidation(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	controlDSN := testexec.FreshDatabase(t, "uc_test_installmod_actor")
	control := testexec.Open(t, controlDSN)
	if err := db.ApplyControl(context.Background(), control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}

	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", "00000000-0000-0000-0000-000000000000",
		"-actor-id", "pipeline", "-actor-type", "ai_agent")
	if code == 0 || !strings.Contains(stderr, "model_version") {
		t.Errorf("expected ai_agent to require -model-version, got code %d: %s", code, stderr)
	}

	_, stderr, code = run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", "00000000-0000-0000-0000-000000000000",
		"-actor-id", "pipeline", "-actor-type", "wizard")
	if code == 0 || !strings.Contains(stderr, "invalid actor") {
		t.Errorf("expected an unknown actor type to be rejected, got code %d: %s", code, stderr)
	}

	// The other half of the same falsification, the other direction —
	// a human row carrying a model version (uc-infra#72 independent
	// review, finding 4, same as every other actor-flagged cmd/
	// binary's own version of this test): Validate() alone only rejects
	// an EMPTY ModelVersion on an agent, never a populated one on a
	// human. This binary was the one instance where the copy-pasted
	// actor-resolution block had actually drifted and was missing this
	// guard entirely (uc-infra#167) — this case is what would have
	// caught that.
	_, stderr, code = run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", "00000000-0000-0000-0000-000000000000",
		"-actor-id", "pipeline", "-model-version", "claude-x")
	if code == 0 || !strings.Contains(stderr, "-model-version is only meaningful") {
		t.Errorf("expected a human actor with -model-version set to be rejected, got code %d: %s", code, stderr)
	}
}

// TestInstall_ActorTypeAI_WritesRealAuditRows is the positive half
// TestActorTypeValidation deliberately doesn't cover: every rejection
// test above only proves the guardrail exists, never that a SUCCESSFUL
// ai_agent install actually reaches the audit log with the right values
// (same class of wiring mistake uc-infra#72's independent review found
// elsewhere). Also covers uc-infra#124: an ai_agent actor's input_hash
// must be populated, not just model_version — Actor.Validate() now
// enforces this, but only a real run against a real database proves the
// CLI actually supplies it end to end. Runs the real compiled binary and
// reads the installed module's own audit_log rows directly.
func TestInstall_ActorTypeAI_WritesRealAuditRows(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	controlDSN := testexec.FreshDatabase(t, "uc_test_installmod_ai_actor")
	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	tenantID, err := router.Create(ctx, "Install Module AI Actor Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, controlDSN), tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	if err := foundation.Publish(ctx, tenantDB, audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	var watermark int64
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT coalesce(max(id), 0) FROM audit_log`).Scan(&watermark); err != nil {
		t.Fatalf("read audit_log watermark: %v", err)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN},
		"-bundle", fixturePath(t), "-tenant-id", tenantID,
		"-actor-id", "pipeline", "-actor-type", "ai_agent", "-model-version", "claude-test-1")
	if code != 0 {
		t.Fatalf("install exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	var total, wrongActor, missingInputHash int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE id > $1`, watermark).Scan(&total); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if total == 0 {
		t.Fatal("expected the install to write at least one new audit_log row")
	}
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE id > $1
		 AND (actor_type != 'ai_agent' OR actor_id != 'pipeline' OR model_version IS DISTINCT FROM 'claude-test-1')`, watermark,
	).Scan(&wrongActor); err != nil {
		t.Fatalf("count wrong-actor audit_log rows: %v", err)
	}
	if wrongActor != 0 {
		t.Errorf("expected every new audit_log row to carry actor_type=ai_agent, actor_id=pipeline, model_version=claude-test-1, got %d that don't", wrongActor)
	}
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE id > $1 AND input_hash IS NULL`, watermark,
	).Scan(&missingInputHash); err != nil {
		t.Fatalf("count missing-input-hash audit_log rows: %v", err)
	}
	if missingInputHash != 0 {
		t.Errorf("expected every new ai_agent audit_log row to carry a non-null input_hash, got %d that don't", missingInputHash)
	}
}
