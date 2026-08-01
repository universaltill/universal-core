// Package modules is the registry of this kernel's built-in modules:
// which module keys exist, and the Publish/PublishForms/PublishStatuses
// set behind each. It is composition-root wiring, not kernel domain
// logic — nothing in it is metadata-driven, and nothing under
// internal/kernel may depend on it.
//
// It lives here rather than beside modulebundle.ReservedModules, which
// would otherwise be the tidy home for "the set of built-in module
// keys", for a dependency-direction reason: modulebundle is kernel
// code, and the kernel must not depend on composition-root wiring —
// importing every module package into internal/kernel would invert the
// direction the whole architecture rests on. (An earlier version of
// this comment claimed a concrete import cycle via purchasing. That was
// simply false — `go list -deps ./internal/kernel/purchasing` contains
// no modulebundle, and modulebundle can import purchasing today without
// error. Independent review caught it. The real, narrower cycle is that
// this package's own test imports modulebundle, so the reverse
// direction would cycle the test binary.)
//
// The parity test between Publishers and modulebundle.ReservedModules
// therefore stays a test rather than becoming structural; see
// TestPublishers_MatchReservedModules, which exists because the lists
// already drifted by three modules once, letting a bundle claim a
// built-in key and silently pre-empt the real module.
//
// Extracted from cmd/provision-tenant when cmd/sync-tenant-modules
// (#70) became a second caller.
package modules

import (
	"context"
	"database/sql"

	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
)

// PublishFunc is the shape every module's publish entry point shares.
type PublishFunc func(ctx context.Context, db *sql.DB, actor audit.Actor) error

// Publisher is one module's publish entry points.
//
// PublishStatuses is nilable: it exists because purchasing.PurchaseOrder
// was the first entity to opt into foundation's Status/StatusType
// pattern (required tenant data, unlike cmd/seed-demo-data's sample
// business data), and a module with no status-managed entity simply has
// nothing to seed.
type Publisher struct {
	Publish         PublishFunc
	PublishForms    PublishFunc
	PublishStatuses PublishFunc
	// Definitions is what this module would publish at the current code
	// revision. It lives in the same struct literal as the publish funcs
	// on purpose: a separate switch keyed by module name would be a
	// third list to keep in sync with this map and with
	// modulebundle.ReservedModules, and this file's own comment is about
	// what happens when two such lists drift.
	Definitions func() []*entity.Definition
}

// FoundationKey is the module every tenant has unconditionally
// (ADR-0001 §8), which is why it is deliberately absent from
// Publishers: it is not something an operator opts into per tenant.
const FoundationKey = "foundation"

// Publishers maps a module key to its publish entry points.
var Publishers = map[string]Publisher{
	"purchasing": {purchasing.Publish, purchasing.PublishForms, purchasing.PublishStatuses, purchasing.All},
	"sales":      {sales.Publish, sales.PublishForms, sales.PublishStatuses, sales.All},
	"finance":    {finance.Publish, finance.PublishForms, nil, finance.All},
	"assets":     {assets.Publish, assets.PublishForms, assets.PublishStatuses, assets.All},
	"projects":   {projects.Publish, projects.PublishForms, projects.PublishStatuses, projects.All},
	"hr":         {hr.Publish, hr.PublishForms, hr.PublishStatuses, hr.All},
	"crm":        {crm.Publish, crm.PublishForms, crm.PublishStatuses, crm.All},
}

// Keys returns every optional module key, sorted, for help text and
// deterministic iteration.
func Keys() []string {
	out := make([]string, 0, len(Publishers))
	for k := range Publishers {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings is insertion sort over a handful of keys — avoids pulling
// "sort" in for a map this size and keeps the package dependency-light.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// PublishFoundation publishes the foundation layer's entities and
// forms. Separate from Publishers because foundation is never optional
// (ADR-0001 §8) and has no status graph of its own — its Status/
// StatusType entities are the mechanism other modules' graphs are
// seeded into, not a graph itself.
func PublishFoundation(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	if err := foundation.Publish(ctx, db, actor); err != nil {
		return err
	}
	return foundation.PublishForms(ctx, db, actor)
}

// FoundationDefinitions returns the entity Definitions foundation would
// publish at the current code revision.
func FoundationDefinitions() []*entity.Definition { return foundation.All() }
