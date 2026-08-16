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

// PublishFoundationPurchasingSalesFixtures publishes foundation,
// purchasing, and sales (Publish, PublishForms, and — for purchasing and
// sales — PublishStatuses) into tenantDB. Shared by four call sites
// across two packages that otherwise can't share a package-internal
// helper:
//   - internal/e2e/status_id_picker_test.go and
//     status_transition_picker_test.go use purchasing (the entity/
//     StatusType under test) and sales (a real wrong-StatusType candidate
//     set for a status_id/from_status_id/to_status_id narrowing bug to
//     fail open into).
//   - internal/api/reference_search_test.go's
//     TestReferenceSearch_SourceField_StatusIDAutoScopedToOwnStatusType
//     asserts the complementary SalesOrder picker's own narrowed result
//     set directly.
//   - internal/api/ublexport_test.go's setupUBLTenant needs all three
//     modules published as the base tenant fixture for its UBL export
//     tests (Party/Currency/Item/PurchaseOrder/SalesOrder/CustomerInvoice
//     records), unrelated to status pickers.
func PublishFoundationPurchasingSalesFixtures(t *testing.T, ctx context.Context, tenantDB *sql.DB, actor audit.Actor) {
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
