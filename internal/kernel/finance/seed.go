package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
)

// Publish brings a tenant's Finance module online — same mechanism as
// purchasing.Publish/sales.Publish (see purchasing.Publish's own doc
// comment for the idempotency/resume/concurrency contract this inherits
// unchanged). No PublishStatuses counterpart: none of this module's
// entities opted into the foundation Status/StatusType pattern yet (see
// finance.go's own doc comments on why FiscalYear/Period use a plain
// FieldEnum for now).
func Publish(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewEntityDefinitionRepo(db)
	items := make([]moduleseed.Item, 0, len(All()))
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			return fmt.Errorf("finance definition %s is invalid: %w", def.EntityType, err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// PublishForms brings a tenant's Finance Form Definitions online —
// separate from Publish for the same reason purchasing.PublishForms is
// separate from purchasing.Publish.
func PublishForms(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	repo := data.NewFormDefinitionRepo(db)
	forms := AllForms()
	items := make([]moduleseed.Item, 0, len(forms))
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("finance form %s is invalid: %w", f.EntityType, err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal form %s: %w", f.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: f.EntityType, Version: f.Version, Raw: raw})
	}
	return moduleseed.PublishAll(ctx, repo, items, actor)
}

// DefaultGLCurrency is the fallback ISO 4217 code SyncGLAccounts uses
// when a finance.Account record has no currency_id set — gl_accounts.
// currency is NOT NULL, but Account.currency_id is optional (an account
// isn't required to declare one, and cmd/seed-demo-data's sample chart
// doesn't set it on any account today). A real per-tenant functional/
// base currency concept doesn't exist yet anywhere in this kernel
// (foundation.Currency has no is_base-style flag) — this constant is a
// known, narrow simplification for this first slice, not a hidden
// assumption: revisit once a tenant-level base currency is actually
// modeled, per erp/BACKLOG-TASKS.md.
const DefaultGLCurrency = "USD"

// SyncGLAccounts brings gl_accounts (the ledger core's own typed chart of
// accounts, ADR-0004) up to date with every published finance.Account
// record — the one narrow, hand-written bridge from the generic entity
// engine to a deterministic-core table, called explicitly (never
// automatically on every write; no lifecycle-hook mechanism exists yet
// in internal/kernel/crud — see ADR-0004's own "not fully closed" note)
// from cmd/seed-demo-data and from this package's own Publish. Idempotent
// by construction (GLAccountRepo.UpsertByCode), safe to call any number
// of times.
func SyncGLAccounts(ctx context.Context, db *sql.DB, actor audit.Actor) error {
	entityDefs := data.NewEntityDefinitionRepo(db)
	accountDefRaw, err := entityDefs.GetPublished(ctx, "Account")
	if err != nil {
		return fmt.Errorf("look up published Account definition: %w", err)
	}
	accountDef, err := entity.Unmarshal(accountDefRaw.Definition)
	if err != nil {
		return fmt.Errorf("unmarshal Account definition: %w", err)
	}

	currencyDefRaw, err := entityDefs.GetPublished(ctx, "Currency")
	if err != nil {
		return fmt.Errorf("look up published Currency definition: %w", err)
	}
	currencyDef, err := entity.Unmarshal(currencyDefRaw.Definition)
	if err != nil {
		return fmt.Errorf("unmarshal Currency definition: %w", err)
	}

	engine := crud.NewEngine(db)
	accounts, err := engine.List(ctx, accountDef)
	if err != nil {
		return fmt.Errorf("list Account records: %w", err)
	}

	glAccounts := data.NewGLAccountRepo(db)
	currencyCodeCache := map[string]string{}
	for _, acc := range accounts {
		code, _ := acc.Data["code"].(string)
		name, _ := acc.Data["name"].(string)
		accountType, _ := acc.Data["type"].(string)
		isActive, _ := acc.Data["is_active"].(bool)

		currency := DefaultGLCurrency
		usedFallback := true
		if currencyID, _ := acc.Data["currency_id"].(string); currencyID != "" {
			usedFallback = false
			if cached, ok := currencyCodeCache[currencyID]; ok {
				currency = cached
			} else {
				currencyRec, err := engine.Get(ctx, currencyDef, currencyID)
				if err != nil {
					return fmt.Errorf("resolve Account %s currency %s: %w", code, currencyID, err)
				}
				if c, _ := currencyRec.Data["code"].(string); c != "" {
					currency = c
				}
				currencyCodeCache[currencyID] = currency
			}
		}
		if usedFallback {
			// Not a hidden assumption (see DefaultGLCurrency's own doc
			// comment), but it should be observable, not silent — a
			// GBP-only tenant whose accounts never set currency_id would
			// otherwise get every gl_account silently labeled USD with no
			// trace anywhere (a real gap independent review caught).
			log.Printf("finance: SyncGLAccounts: Account %s has no currency_id set, defaulting gl_accounts.currency to %s", code, DefaultGLCurrency)
		}

		if _, err := glAccounts.UpsertByCode(ctx, code, name, accountType, currency, isActive); err != nil {
			return fmt.Errorf("sync gl_account %s: %w", code, err)
		}
	}
	return nil
}
