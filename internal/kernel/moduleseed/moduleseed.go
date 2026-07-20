// Package moduleseed is the shared draft->approve->publish-for-a-tenant
// logic that internal/kernel/foundation and internal/kernel/purchasing
// each need for both their entity Definitions and their Form
// Definitions — four near-identical hand-copies of the same resumable
// publish algorithm (foundation's entity Publish, purchasing's entity
// Publish, and now both packages' new form-publishing counterparts)
// crossed this codebase's own "duplicate until a third occurrence
// justifies sharing" threshold (see purchasing/seed.go's original
// reasoning, which this package supersedes — that reasoning was correct
// for two occurrences, not four).
package moduleseed

import (
	"context"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// Repo is the subset of data.EntityDefinitionRepo's/
// data.FormDefinitionRepo's identical method set PublishAll needs.
// Satisfied by both without any change to internal/data — Go interfaces
// are structural, and the three definition-registry repos
// (entity/form/workflow) already share this exact shape by construction
// (internal/data/definitions.go's definitionRepo).
type Repo interface {
	GetVersion(ctx context.Context, tenantID, key string, version int) (data.DefinitionVersion, error)
	CreateDraft(ctx context.Context, tenantID, key string, version int, definition []byte, actor audit.Actor) (data.DefinitionVersion, error)
	Approve(ctx context.Context, tenantID, key string, version int, actor audit.Actor) error
	Publish(ctx context.Context, tenantID, key string, version int, actor audit.Actor) error
}

// Item is one already-marshaled, already-validated Definition to
// publish. Validation stays the caller's job (entity.Definition.Validate
// / form.Definition.Validate) rather than this package taking a
// dependency on either — it only needs to move bytes through the
// registry lifecycle, not understand what they mean.
type Item struct {
	Key     string // entity_type (workflow name, if ever used for that registry)
	Version int
	Raw     []byte
}

// PublishAll drives every item through draft -> approve -> publish for
// tenantID, idempotent and resumable from a partial prior failure:
// publishOne fetches each item's current registry state (not present /
// draft / approved / published / rolled_back) and drives it forward from
// wherever it actually is, rather than assuming a prior call either fully
// completed or never ran. Not safe for concurrent calls publishing the
// same item for the same tenant — every collision surfaces as a clean,
// retryable error (a duplicate CreateDraft hits the registry's
// UNIQUE(tenant_id, key, version) constraint; a duplicate Approve/Publish
// hits the atomic status-guarded UPDATE), never a wrong ordering or
// corrupted row. Expected call pattern is once per tenant onboarding (a
// cold path), where this doesn't come up in practice.
func PublishAll(ctx context.Context, repo Repo, tenantID string, items []Item, actor audit.Actor) error {
	for _, item := range items {
		if err := publishOne(ctx, repo, tenantID, item, actor); err != nil {
			return err
		}
	}
	return nil
}

func publishOne(ctx context.Context, repo Repo, tenantID string, item Item, actor audit.Actor) error {
	v, err := repo.GetVersion(ctx, tenantID, item.Key, item.Version)
	status := v.Status
	switch {
	case errors.Is(err, data.ErrNotFound):
		if _, err := repo.CreateDraft(ctx, tenantID, item.Key, item.Version, item.Raw, actor); err != nil {
			return fmt.Errorf("draft %s v%d: %w", item.Key, item.Version, err)
		}
		status = data.StatusDraft
	case err != nil:
		return fmt.Errorf("check existing %s v%d: %w", item.Key, item.Version, err)
	}

	if status == data.StatusPublished || status == data.StatusRolledBack {
		return nil
	}
	if status == data.StatusDraft {
		if err := repo.Approve(ctx, tenantID, item.Key, item.Version, actor); err != nil {
			return fmt.Errorf("approve %s v%d: %w", item.Key, item.Version, err)
		}
	}
	if err := repo.Publish(ctx, tenantID, item.Key, item.Version, actor); err != nil {
		return fmt.Errorf("publish %s v%d: %w", item.Key, item.Version, err)
	}
	return nil
}
