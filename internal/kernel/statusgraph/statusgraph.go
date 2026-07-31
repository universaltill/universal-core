// Package statusgraph is the shared StatusType/Status/StatusTransition
// seeder that internal/kernel/purchasing and internal/kernel/sales each
// carried as identical private copies, now needed a third time by
// internal/kernel/modulebundle — the same threshold that created
// moduleseed out of four copies of the publish algorithm.
//
// The purchasing/sales duplication was deliberate at two copies: those
// packages are independently licensable modules with no Go-level
// dependency on EACH OTHER, and that reasoning still stands — this is a
// kernel package both may depend on (they already depend on crud,
// entity, moduleseed), not a cross-module import.
//
// Seed is idempotent — every StatusType/Status looked up by its natural
// key, every StatusTransition by its from/to pair — and scoped by
// status_type_id, not just code: two StatusTypes can both have a
// "draft" Status, and a code-only lookup could silently reuse the wrong
// row's id, corrupting the graph (the 2026-07-29 code-collision bug's
// fix, preserved from the originals).
package statusgraph

import (
	"context"
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// Spec is one Status row within a StatusType's graph.
type Spec struct {
	Code, Name            string
	Sequence              float64
	IsInitial, IsTerminal bool
}

// Seed seeds one StatusType, its Status rows, and its StatusTransition
// edges, returning status ids by code. statusTypeDef/statusDef/
// transitionDef are the published foundation Definitions, resolved by
// the caller through the registry (modules have no Go-level foundation
// dependency — the runtime dependency is "foundation published for this
// tenant", same as every cross-module reference field).
func Seed(
	ctx context.Context,
	engine *crud.Engine,
	statusTypeDef, statusDef, transitionDef *entity.Definition,
	entityType, code, name string,
	statuses []Spec,
	edges [][2]string,
	actor audit.Actor,
) (map[string]string, error) {
	getOrCreate := func(d *entity.Definition, keyField, keyValue string, fields map[string]any) (string, error) {
		existing, err := engine.ListByField(ctx, d, keyField, keyValue)
		if err != nil {
			return "", fmt.Errorf("list %s by %s: %w", d.EntityType, keyField, err)
		}
		if len(existing) > 0 {
			return existing[0].ID, nil
		}
		rec, err := engine.Create(ctx, d, fields, actor)
		if err != nil {
			return "", fmt.Errorf("create %s %v: %w", d.EntityType, fields, err)
		}
		return rec.ID, nil
	}

	statusTypeID, err := getOrCreate(statusTypeDef, "code", code, map[string]any{
		"entity_type": entityType, "code": code, "name": name,
	})
	if err != nil {
		return nil, fmt.Errorf("seed %s StatusType: %w", code, err)
	}

	existingByCode := map[string]string{}
	existingStatuses, err := engine.ListByField(ctx, statusDef, "status_type_id", statusTypeID)
	if err != nil {
		return nil, fmt.Errorf("list existing Status for %s: %w", code, err)
	}
	for _, s := range existingStatuses {
		if c, _ := s.Data["code"].(string); c != "" {
			existingByCode[c] = s.ID
		}
	}

	statusIDs := make(map[string]string, len(statuses))
	for _, s := range statuses {
		if id, ok := existingByCode[s.Code]; ok {
			statusIDs[s.Code] = id
			continue
		}
		rec, err := engine.Create(ctx, statusDef, map[string]any{
			"status_type_id": statusTypeID, "code": s.Code, "name": s.Name,
			"sequence": s.Sequence, "is_initial": s.IsInitial, "is_terminal": s.IsTerminal,
		}, actor)
		if err != nil {
			return nil, fmt.Errorf("seed %s Status: %w", s.Code, err)
		}
		statusIDs[s.Code] = rec.ID
	}

	for _, edge := range edges {
		from, to := statusIDs[edge[0]], statusIDs[edge[1]]
		existing, err := engine.ListByField(ctx, transitionDef, "from_status_id", from)
		if err != nil {
			return nil, fmt.Errorf("list StatusTransition by from_status_id: %w", err)
		}
		found := false
		for _, t := range existing {
			if to2, _ := t.Data["to_status_id"].(string); to2 == to {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if _, err := engine.Create(ctx, transitionDef, map[string]any{
			"status_type_id": statusTypeID, "from_status_id": from, "to_status_id": to,
		}, actor); err != nil {
			return nil, fmt.Errorf("seed StatusTransition %s->%s: %w", edge[0], edge[1], err)
		}
	}
	return statusIDs, nil
}
