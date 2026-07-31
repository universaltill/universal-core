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
}
