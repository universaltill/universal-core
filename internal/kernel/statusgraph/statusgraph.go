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
//
// Name is the required English/fallback value. Translations optionally
// supplies additional locales (keyed by locale code, e.g. "ar"/"fa"/"tr" —
// never "en", see BuildI18nName's own doc comment on why) — this is the follow-up
// uc-infra#244 promised when Name was still the only content Seed had to
// wrap (uc-infra#214, ADR-0030/0031): a caller that only sets Name keeps
// behaving exactly as before (nil Translations is fine, Seed treats it the
// same as an empty map), and one that also sets Translations gets those
// locales merged into the same i18n_text object.
type Spec struct {
	Code, Name            string
	Translations          map[string]string
	Sequence              float64
	IsInitial, IsTerminal bool
}

// CopySpecs returns a deep copy of specs — specifically, each Spec's own
// Translations map is a fresh copy, never the caller's original. Every
// module's exported StatusSpecs() (purchasing.StatusSpecs and its five
// siblings) returns this rather than its package-level var directly: the
// package-level vars are shared, process-lifetime state that Seed itself
// only ever reads, but a StatusSpecs() caller mutating a returned Spec's
// Translations map in place (e.g. `specs[0].Translations["ar"] = x`)
// would otherwise corrupt every future seed of that module for the rest
// of the process — the exact hazard a plain "read-only, don't mutate"
// comment doesn't actually prevent.
func CopySpecs(specs []Spec) []Spec {
	out := make([]Spec, len(specs))
	for i, s := range specs {
		out[i] = s
		if s.Translations != nil {
			out[i].Translations = make(map[string]string, len(s.Translations))
			for k, v := range s.Translations {
				out[i].Translations[k] = v
			}
		}
	}
	return out
}

// BuildI18nName builds the i18n_text object (ADR-0009) a Status.name
// field stores from a Spec's plain-string Name plus its optional
// Translations map — the one place that wrap happens, so Seed and a
// backfill tool updating an already-seeded tenant's existing rows
// (uc-infra#244) apply the identical merge rather than two independent
// copies that could drift. "en" always comes from Name: a "en" key
// inside Translations is overwritten by this, deliberately, since Name
// is documented as the required fallback and letting Translations
// silently win would make the two ways of setting English-language
// content disagree about which one applies.
func BuildI18nName(name string, translations map[string]string) map[string]any {
	obj := make(map[string]any, len(translations)+1)
	for locale, text := range translations {
		obj[locale] = text
	}
	obj["en"] = name
	return obj
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
		// name: wrapped as i18n_text (uc-infra#214, ADR-0030). "en" is
		// always Name, the required fallback; any additional locale in
		// Translations is merged in alongside it (uc-infra#244) — a
		// caller-supplied "en" key in Translations would be silently
		// overwritten by Name's own assignment below, so BuildI18nName
		// (the single place this merge happens) documents Name as the
		// only source of "en" rather than accepting two.
		rec, err := engine.Create(ctx, statusDef, map[string]any{
			"status_type_id": statusTypeID, "code": s.Code, "name": BuildI18nName(s.Name, s.Translations),
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
