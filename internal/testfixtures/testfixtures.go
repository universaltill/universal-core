// Package testfixtures holds cross-package shared test setup — fixture
// publishing sequences duplicated across more than one test package,
// where a package-internal helper (the usual answer) can't be shared
// because the call sites live in different packages. Same shape/rationale
// as internal/testexec (shared cmd/ smoke-test helpers): a small,
// test-only, exported package rather than either duplicating the setup
// or reaching into another package's _test.go file.
package testfixtures

import (
	"context"
	"database/sql"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
)

// PublishStatusPickerFixtures publishes foundation, purchasing (the
// entity/StatusType under test in the two internal/e2e call sites), and
// sales (a real wrong-StatusType candidate set for a
// status_id/from_status_id/to_status_id narrowing bug to fail open into,
// in the internal/e2e call sites — and, in the internal/api call site,
// the complementary SalesOrder picker whose own narrowed result set is
// asserted directly) into tenantDB. Shared by internal/e2e (status_id_picker_test.go,
// status_transition_picker_test.go) and internal/api
// (reference_search_test.go's
// TestReferenceSearch_SourceField_StatusIDAutoScopedToOwnStatusType) —
// three call sites across two packages that otherwise can't share a
// package-internal helper.
func PublishStatusPickerFixtures(t *testing.T, ctx context.Context, tenantDB *sql.DB, actor audit.Actor) {
	t.Helper()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}
	if err := sales.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishForms: %v", err)
	}
	if err := sales.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}
}
