// Package modulebundle loads and installs erp_module bundles — the
// data/metadata artifact type ADR-0002 defines (Entity + Form
// Definitions plus status graphs, matching the registry's own JSON
// shape; NOT executable code, which is the erp_connector artifact's
// separate territory). Bundle format v1 is specified by ADR-0012.
//
// Load parses and validates a bundle without touching any database —
// the CLI's -validate mode is exactly Load and nothing else. Install
// drives a loaded bundle through the same machinery the built-in Go
// modules use: moduleseed.PublishAll for the registry lifecycle
// (draft→approve→publish, idempotent/resumable) and statusgraph.Seed
// for the tenant's StatusType/Status/StatusTransition records. An
// installed bundle is indistinguishable from a built-in module to the
// rest of the kernel: the generic engine, nav, permissions, import/
// export all pick it up through the registry with no further wiring.
//
// Install refuses divergent overwrites: a (key, version) already in
// the registry with semantically different content is an error (all
// conflicts collected and reported together), while byte-different but
// semantically identical JSON — and true re-installs — are idempotent
// no-ops, same contract as re-running a Go module's Publish.
package modulebundle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
)

// FormatV1 is the only bundle format this installer accepts. A future
// v2 fails Load cleanly (both here and via the strict manifest decode,
// which rejects fields v1 doesn't know) rather than half-installing.
const FormatV1 = "erp_module/v1"

// reservedModules are the built-in Go modules' keys. A bundle may never
// claim one: Install's ownership guard compares a published
// definition's Module against the BUNDLE'S OWN declared module, so a
// bundle calling itself "foundation" would otherwise satisfy the guard
// and republish Party at a higher version — and GetPublished takes the
// highest version, so it wins (independent review, demonstrated: a
// one-field "Party" v99 replaced foundation's real definition). The set
// mirrors cmd/provision-tenant's modulePublishers plus foundation
// itself; a new built-in module must be added here too.
//
// It had already drifted by three modules (assets, projects, hr) before
// anyone noticed, and the comment claimed a parity test that did not
// exist — so ReservedModules is now exported and
// cmd/provision-tenant's own test asserts the two lists agree. The
// drift is not cosmetic: the independent review demonstrated a bundle
// declaring an unlisted built-in module key installing its own
// Employee v1, after which the real hr.Publish returns nil (moduleseed
// skips an already-published key/version) and reports success while
// the genuine Definition never lands.
var ReservedModules = map[string]bool{
	"foundation": true,
	"purchasing": true,
	"sales":      true,
	"finance":    true,
	"assets":     true,
	"projects":   true,
	"hr":         true,
	"crm":        true,
}

// manifest is the raw file shape. Definitions stay json.RawMessage so
// each is unmarshaled+validated by its own package's Unmarshal — this
// package never re-specifies what a valid Definition is.
type manifest struct {
	Format       string            `json:"format"`
	Module       string            `json:"module"`
	Name         string            `json:"name"`
	Entities     []json.RawMessage `json:"entities"`
	Forms        []json.RawMessage `json:"forms,omitempty"`
	StatusGraphs []StatusGraph     `json:"status_graphs,omitempty"`
}

// StatusGraph is one StatusType's seed spec, the data form of the
// arguments statusgraph.Seed takes.
type StatusGraph struct {
	EntityType     string       `json:"entity_type"`
	StatusTypeCode string       `json:"status_type_code"`
	StatusTypeName string       `json:"status_type_name"`
	Statuses       []StatusSpec `json:"statuses"`
	Transitions    []Transition `json:"transitions"`
}

type StatusSpec struct {
	Code       string  `json:"code"`
	Name       string  `json:"name"`
	Sequence   float64 `json:"sequence"`
	IsInitial  bool    `json:"is_initial"`
	IsTerminal bool    `json:"is_terminal"`
}

type Transition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Bundle is a parsed, validated erp_module bundle.
type Bundle struct {
	Module       string
	Name         string
	Entities     []*entity.Definition
	Forms        []*form.Definition
	StatusGraphs []StatusGraph
}

// Load parses raw bundle bytes and runs every validation that needs no
// database: strict manifest decode, format version, per-definition
// Unmarshal+Validate, module-key consistency, in-bundle uniqueness,
// form targets, and status-graph coherence. A bundle that Loads is
// installable up to registry-state questions (divergence, foreign
// ownership), which are Install's to answer.
func Load(raw []byte) (*Bundle, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("modulebundle: parse bundle: %w", err)
	}
	if dec.More() {
		return nil, errors.New("modulebundle: bundle contains trailing content after the manifest object")
	}
	if m.Format != FormatV1 {
		return nil, fmt.Errorf("modulebundle: unsupported bundle format %q (this installer accepts %q)", m.Format, FormatV1)
	}
	if m.Module == "" || m.Module != strings.ToLower(m.Module) || strings.ContainsAny(m.Module, " \t\n") {
		return nil, fmt.Errorf("modulebundle: module key %q must be non-empty lowercase with no whitespace", m.Module)
	}
	if ReservedModules[m.Module] {
		return nil, fmt.Errorf("modulebundle: module key %q is reserved for a built-in module", m.Module)
	}
	if len(m.Entities) == 0 {
		return nil, errors.New("modulebundle: bundle declares no entities")
	}

	b := &Bundle{Module: m.Module, Name: m.Name, StatusGraphs: m.StatusGraphs}

	entityByType := map[string]*entity.Definition{}
	for i, raw := range m.Entities {
		if err := rejectUnknownFields(raw, &entity.Definition{}); err != nil {
			return nil, fmt.Errorf("modulebundle: entities[%d]: %w", i, err)
		}
		def, err := entity.Unmarshal(raw)
		if err != nil {
			return nil, fmt.Errorf("modulebundle: entities[%d]: %w", i, err)
		}
		if def.Module != m.Module {
			return nil, fmt.Errorf("modulebundle: entity %s declares module %q, bundle is %q", def.EntityType, def.Module, m.Module)
		}
		// Keyed by entity type alone, not (type, version): two versions
		// of one type in a single bundle would leave the form/status
		// coherence checks validating against whichever came last, and
		// v1 has no multi-version install story (ADR-0012).
		if _, dup := entityByType[def.EntityType]; dup {
			return nil, fmt.Errorf("modulebundle: duplicate entity %s in bundle", def.EntityType)
		}
		entityByType[def.EntityType] = def
		b.Entities = append(b.Entities, def)
	}

	seenForm := map[string]bool{}
	for i, raw := range m.Forms {
		if err := rejectUnknownFields(raw, &form.Definition{}); err != nil {
			return nil, fmt.Errorf("modulebundle: forms[%d]: %w", i, err)
		}
		def, err := form.Unmarshal(raw)
		if err != nil {
			return nil, fmt.Errorf("modulebundle: forms[%d]: %w", i, err)
		}
		if _, ok := entityByType[def.EntityType]; !ok {
			return nil, fmt.Errorf("modulebundle: form for %s has no matching entity in the bundle", def.EntityType)
		}
		if seenForm[def.EntityType] {
			return nil, fmt.Errorf("modulebundle: duplicate form for %s in bundle", def.EntityType)
		}
		seenForm[def.EntityType] = true
		b.Forms = append(b.Forms, def)
	}

	graphByCode := map[string]bool{}
	graphEntityTypes := map[string]bool{}
	for i, g := range m.StatusGraphs {
		def, ok := entityByType[g.EntityType]
		if !ok {
			return nil, fmt.Errorf("modulebundle: status_graphs[%d] targets %s, which is not in the bundle", i, g.EntityType)
		}
		if def.StatusTypeCode != g.StatusTypeCode {
			return nil, fmt.Errorf("modulebundle: status_graphs[%d] code %q does not match entity %s's status_type_code %q", i, g.StatusTypeCode, g.EntityType, def.StatusTypeCode)
		}
		// Two graphs sharing a code would merge into one StatusType at
		// seed time — the in-bundle twin of the cross-module hijack the
		// ownership guard closes.
		if graphByCode[g.StatusTypeCode] {
			return nil, fmt.Errorf("modulebundle: duplicate status_type_code %q in bundle", g.StatusTypeCode)
		}
		graphByCode[g.StatusTypeCode] = true
		graphEntityTypes[g.EntityType] = true
		if len(g.Statuses) == 0 {
			return nil, fmt.Errorf("modulebundle: status_graphs[%d] (%s) declares no statuses", i, g.StatusTypeCode)
		}
		byCode := map[string]bool{}
		initials := 0
		for _, s := range g.Statuses {
			if s.Code == "" {
				return nil, fmt.Errorf("modulebundle: status_graphs[%d] (%s) has a status with an empty code", i, g.StatusTypeCode)
			}
			if byCode[s.Code] {
				return nil, fmt.Errorf("modulebundle: status_graphs[%d] (%s) duplicates status code %q", i, g.StatusTypeCode, s.Code)
			}
			byCode[s.Code] = true
			if s.IsInitial {
				initials++
			}
		}
		if initials != 1 {
			return nil, fmt.Errorf("modulebundle: status_graphs[%d] (%s) must have exactly one initial status, has %d", i, g.StatusTypeCode, initials)
		}
		for _, tr := range g.Transitions {
			if !byCode[tr.From] || !byCode[tr.To] {
				return nil, fmt.Errorf("modulebundle: status_graphs[%d] (%s) transition %s->%s references an undeclared status", i, g.StatusTypeCode, tr.From, tr.To)
			}
		}
	}

	// The other direction: an entity declaring a status_type_code with
	// no graph installs "successfully" but is unusable — every create
	// fails with "status type ... is not published for this tenant",
	// a dead module reported as an installed one (independent review).
	for _, def := range b.Entities {
		if def.StatusTypeCode != "" && !graphEntityTypes[def.EntityType] {
			return nil, fmt.Errorf("modulebundle: entity %s declares status_type_code %q but the bundle has no matching status graph", def.EntityType, def.StatusTypeCode)
		}
	}

	return b, nil
}

// Install publishes a loaded bundle into one tenant's database (db is
// already resolved to that tenant, ADR-0003) and seeds its status
// graphs. Requires foundation published there (for StatusType/Status/
// StatusTransition), same precondition as every built-in module's
// PublishStatuses.
//
// Every refusal (divergence, foreign entity type, foreign status-type
// code) is decided BEFORE the first write, and all of them are
// reported together rather than one per attempt.
//
// Install is NOT atomic across its write phases — entity publish, form
// publish, then one seed per status graph — but every phase is
// resumable: moduleseed.PublishAll drives each definition forward from
// whatever registry state it's actually in, and statusgraph.Seed
// looks everything up by natural key. A failure partway through
// therefore leaves a partial install that **re-running the same
// install completes**; there is no cleanup step to perform and no
// half-written definition to repair (independent review asked for this
// contract to be written down rather than inferred).
func Install(ctx context.Context, db *sql.DB, b *Bundle, actor audit.Actor) error {
	entityRepo := data.NewEntityDefinitionRepo(db)
	formRepo := data.NewFormDefinitionRepo(db)

	entityItems := make([]moduleseed.Item, 0, len(b.Entities))
	for _, def := range b.Entities {
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("modulebundle: marshal %s: %w", def.EntityType, err)
		}
		entityItems = append(entityItems, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	formItems := make([]moduleseed.Item, 0, len(b.Forms))
	for _, def := range b.Forms {
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("modulebundle: marshal form %s: %w", def.EntityType, err)
		}
		formItems = append(formItems, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}

	var conflicts []string
	checkConflicts := func(repo moduleseed.Repo, kind string, items []moduleseed.Item) error {
		for _, item := range items {
			existing, err := repo.GetVersion(ctx, item.Key, item.Version)
			if errors.Is(err, data.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("modulebundle: check existing %s %s v%d: %w", kind, item.Key, item.Version, err)
			}
			same, err := jsonEqual(existing.Definition, item.Raw)
			if err != nil {
				return fmt.Errorf("modulebundle: compare %s %s v%d: %w", kind, item.Key, item.Version, err)
			}
			if !same {
				conflicts = append(conflicts, fmt.Sprintf("%s %s v%d already exists with different content", kind, item.Key, item.Version))
			}
		}
		return nil
	}
	if err := checkConflicts(entityRepo, "entity", entityItems); err != nil {
		return err
	}
	if err := checkConflicts(formRepo, "form", formItems); err != nil {
		return err
	}
	// An entity type whose PUBLISHED definition belongs to another
	// module may not be claimed by this bundle even at a fresh version —
	// that's a hijack, not an upgrade.
	for _, def := range b.Entities {
		v, err := entityRepo.GetPublished(ctx, def.EntityType)
		if errors.Is(err, data.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("modulebundle: check published %s: %w", def.EntityType, err)
		}
		published, err := entity.Unmarshal(v.Definition)
		if err != nil {
			return fmt.Errorf("modulebundle: unmarshal published %s: %w", def.EntityType, err)
		}
		if published.Module != b.Module {
			conflicts = append(conflicts, fmt.Sprintf("entity type %s is owned by module %q, not %q", def.EntityType, published.Module, b.Module))
		}
	}
	// Status-type ownership: statusgraph.Seed resolves a StatusType by
	// CODE alone, so a bundle naming an existing code would reuse
	// another module's live StatusType row and inject Status rows and
	// transition edges into it. The independent review demonstrated the
	// consequence: a bundle declaring status_type_code
	// "purchase_order_status" added a draft->received edge, and
	// crud.ValidateStatusTransition (which also resolves by code) then
	// let any PurchaseOrder skip submitted/approved — an approval
	// bypass on a financial document, installed by a bundle that never
	// mentions purchasing.
	if len(b.StatusGraphs) > 0 {
		engine := crud.NewEngine(db)
		statusTypeDef, err := publishedEntityDef(ctx, entityRepo, "StatusType")
		if err != nil {
			return err
		}
		for _, g := range b.StatusGraphs {
			existing, err := engine.ListByField(ctx, statusTypeDef, "code", g.StatusTypeCode)
			if err != nil {
				return fmt.Errorf("modulebundle: check existing status type %s: %w", g.StatusTypeCode, err)
			}
			for _, row := range existing {
				owner, _ := row.Data["entity_type"].(string)
				if owner == g.EntityType {
					continue // this bundle's own graph, from a prior install
				}
				conflicts = append(conflicts, fmt.Sprintf(
					"status type code %s is already owned by entity %s", g.StatusTypeCode, owner))
			}
		}
	}

	if len(conflicts) > 0 {
		return fmt.Errorf("modulebundle: refusing to install %s: %s", b.Module, strings.Join(conflicts, "; "))
	}

	if err := moduleseed.PublishAll(ctx, entityRepo, entityItems, actor); err != nil {
		return fmt.Errorf("modulebundle: publish %s entities: %w", b.Module, err)
	}
	if err := moduleseed.PublishAll(ctx, formRepo, formItems, actor); err != nil {
		return fmt.Errorf("modulebundle: publish %s forms: %w", b.Module, err)
	}

	if len(b.StatusGraphs) == 0 {
		return nil
	}
	engine := crud.NewEngine(db)
	statusTypeDef, err := publishedEntityDef(ctx, entityRepo, "StatusType")
	if err != nil {
		return err
	}
	statusDef, err := publishedEntityDef(ctx, entityRepo, "Status")
	if err != nil {
		return err
	}
	transitionDef, err := publishedEntityDef(ctx, entityRepo, "StatusTransition")
	if err != nil {
		return err
	}
	for _, g := range b.StatusGraphs {
		specs := make([]statusgraph.Spec, 0, len(g.Statuses))
		for _, s := range g.Statuses {
			specs = append(specs, statusgraph.Spec{
				Code: s.Code, Name: s.Name, Sequence: s.Sequence,
				IsInitial: s.IsInitial, IsTerminal: s.IsTerminal,
			})
		}
		edges := make([][2]string, 0, len(g.Transitions))
		for _, tr := range g.Transitions {
			edges = append(edges, [2]string{tr.From, tr.To})
		}
		if _, err := statusgraph.Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
			g.EntityType, g.StatusTypeCode, g.StatusTypeName, specs, edges, actor); err != nil {
			return fmt.Errorf("modulebundle: seed %s: %w", g.StatusTypeCode, err)
		}
	}
	return nil
}

// rejectUnknownFields strict-decodes raw into a throwaway value of the
// target Definition type, so a hand-authored typo ("requred": true)
// fails Load instead of loading as a silently-dropped field and being
// stored in its narrowed form. The real parse still goes through
// entity/form.Unmarshal — this only front-runs it with the strictness
// the manifest decode already applies at the top level.
func rejectUnknownFields(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	return nil
}

// publishedEntityDef resolves one published entity Definition —
// foundation's StatusType/Status/StatusTransition, which every status
// graph needs and which must already be published in the target tenant
// (the same precondition every built-in module's PublishStatuses has).
func publishedEntityDef(ctx context.Context, repo *data.EntityDefinitionRepo, entityType string) (*entity.Definition, error) {
	v, err := repo.GetPublished(ctx, entityType)
	if err != nil {
		return nil, fmt.Errorf("modulebundle: look up published %s (foundation must be published before installing a bundle with status graphs): %w", entityType, err)
	}
	return entity.Unmarshal(v.Definition)
}

// jsonEqual compares two JSON documents semantically — key order and
// whitespace don't count as divergence, content does. The registry
// stores json.Marshal output, but a hand-authored bundle legitimately
// formats the same definition differently.
func jsonEqual(a, b []byte) (bool, error) {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false, fmt.Errorf("left side: %w", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false, fmt.Errorf("right side: %w", err)
	}
	return reflect.DeepEqual(av, bv), nil
}
