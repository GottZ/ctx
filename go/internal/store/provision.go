// Project provisioning store layer (workflow I-I, design/02 §4.6; masterplan K12
// = the Agent-Key template contract, E4 = server-admin authority, E7 = quota
// seed). ProvisionProject is the ONE-transaction compound that turns a bare repo
// identity into a fully usable, isolated tenant-per-repo (E2): tenant + limits +
// the auto-prefixed '<slug>:main' scope + owner key + the project register row +
// a repo-agent key minted STRICTLY to the K12 template + an optional per-tenant
// LLM-quota seed. Every step shares the transaction, so a failure at ANY step
// rolls the whole thing back — there is never a half-provisioned tenant that
// would wedge the next run on a slug collision (the atomicity gate).
//
// Idempotency is keyed on the project IDENTITY, GLOBALLY (not per-tenant): a
// re-run of `ctx project init` for the same repo returns the existing project
// with Provisioned=false and mints NOTHING (the reveal-once keys are shown ONCE,
// at first provision — a second run is a pure No-op read).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProvisionParams is the fully-resolved input to ProvisionProject. The handler
// derives Slug + Scope from the identity (server-side, single source of truth)
// and validates them BEFORE the store is reached; the store trusts them (they
// are re-gated defensively by CreateTenantTx / AssignTenantScopeTx anyway).
type ProvisionParams struct {
	Slug        string // gated tenant slug (slugPattern, <=24)
	DisplayName string // human tenant name
	Scope       string // the fully-prefixed '<slug>:main' initial scope
	Identity    string // 'github:owner/repo' | 'git-root:<sha>' | 'manual:<slug>'
	Forge       json.RawMessage
	SeedScopes  *int                  // optional tenant max_scopes override (nil = 069 default)
	SeedKeys    *int                  // optional tenant max_keys override (nil = 069 default)
	Quota       *backends.TenantQuota // optional LLM-quota seed for the scope (nil = none, E7)
	CreatedBy   string                // acting server-admin key id ("" -> NULL)
}

// ProvisionResult carries the compound outcome. The two plaintexts are the
// reveal-once secrets (owner key = the operator's tenant-admin credential; agent
// key = the repo-agent MCP credential stored 0600 by the CLI). On an idempotent
// re-run Provisioned=false and BOTH plaintexts are empty — the existing project
// is returned for a bestand read, no new secret is minted.
type ProvisionResult struct {
	Tenant         *Tenant
	Scope          string
	Project        *ProjectRow
	OwnerKey       ApiKey
	OwnerPlaintext string
	AgentKey       ApiKey
	AgentPlaintext string
	Provisioned    bool
}

// ownerKeyLabelSuffix / agentKeyLabelSuffix name the two minted keys so a key
// list is self-describing.
const (
	ownerKeyLabelSuffix = " owner"
	agentKeyLabelSuffix = " repo-agent"
)

// ProvisionProject runs the atomic compound (design/02 §4.6 step 3). It returns
// the existing project with Provisioned=false when the identity is already
// registered (idempotent No-op), or the freshly built tenant/scope/project +
// both reveal-once keys with Provisioned=true.
//
// Ordering is load-bearing and mirrors bootstrapTenant (§6): tenant -> limits ->
// scope (BEFORE the keys, so their home_scope already exists in
// context_tenant_scopes, T22) -> owner key -> project register row -> repo-agent
// key -> quota seed. All in ONE tx.
//
// ErrTenantSlugExists surfaces when the derived slug collides with an unrelated
// tenant (the identity idempotency already ruled out a prior run of THIS repo) —
// the handler maps it 409. The K12 Agent-Key template is enforced HERE, not by
// the caller: home_scope = the project scope, allowed_scopes = [] (non-nil
// empty, so insertApiKeyTx does NOT inherit the default-tenant '{shared}'),
// write_scopes = []. That is the whole security contract of the repo-agent key
// (§5.5): writableBlockScopes collapses to {home_scope} — it can write its OWN
// scope and nothing else, never shared, never a foreign scope.
func ProvisionProject(ctx context.Context, pool *pgxpool.Pool, p ProvisionParams) (*ProvisionResult, error) {
	var (
		existing *ProjectRow
		out      *ProvisionResult
	)
	if err := pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "store: provision begin", Commit: "store: provision commit"},
		func(tx pgx.Tx) error {
			// 0: global identity idempotency. A prior provision of THIS repo (any tenant)
			// -> return it, mint nothing. FOR UPDATE serialises a concurrent double-init of
			// the same identity so two callers can not both proceed to CreateTenantTx.
			var err error
			existing, err = scanProject(tx.QueryRow(ctx,
				`SELECT `+projectSelect+` FROM context_projects WHERE identity = $1
				  ORDER BY created_at ASC LIMIT 1 FOR UPDATE`, p.Identity))
			if err != nil {
				return fmt.Errorf("store: provision idempotency probe: %w", err)
			}
			if existing != nil {
				// The idempotent exit commits HERE: its commit failure carries its
				// own wording, which one Stages pair cannot hold beside the tail
				// commit's. errTxCommitted then stops the helper from committing again.
				if cerr := tx.Commit(ctx); cerr != nil {
					return fmt.Errorf("store: provision idempotent commit: %w", cerr)
				}
				return errTxCommitted
			}

			// 1: tenant row (23505 slug -> ErrTenantSlugExists -> 409).
			tn, err := CreateTenantTx(ctx, tx, p.Slug, p.DisplayName)
			if err != nil {
				return err
			}

			// 2: seed structural limits over the 069 DEFAULTs (only supplied dimensions).
			if p.SeedScopes != nil || p.SeedKeys != nil {
				newScopes, newKeys := tn.MaxScopes, tn.MaxKeys
				if p.SeedScopes != nil {
					newScopes = p.SeedScopes
				}
				if p.SeedKeys != nil {
					newKeys = p.SeedKeys
				}
				if err := SetTenantLimitsTx(ctx, tx, tn.ID, newScopes, newKeys); err != nil {
					return err
				}
				tn.MaxScopes, tn.MaxKeys = newScopes, newKeys
			}

			// 3: register the initial scope CAP-FREE (maxScopes=-1), BEFORE the keys — so
			// the first scope never wedges on a max_scopes=0 seed and the keys' home_scope
			// already exists (T22).
			capFree := -1
			if _, err := AssignTenantScopeTx(ctx, tx, tn.ID, p.Scope, &capFree); err != nil {
				return err
			}

			// 4: owner key (role='owner', home + allowed = the scope) — the operator's
			// tenant-admin credential, reveal-once.
			ownerKey, ownerPlain, err := MintOwnerKey(ctx, tx, p.Slug+ownerKeyLabelSuffix, p.Scope, []string{p.Scope}, tn.ID)
			if err != nil {
				return err
			}

			// 5: the project register row (tenant_id = the SAME binding tenant, §3.1). A
			// non-live created_by (23503) or a scope/identity race (23505) rolls back the
			// whole tx.
			var createdBy any
			if p.CreatedBy != "" {
				createdBy = p.CreatedBy
			}
			forge := p.Forge
			if len(forge) == 0 {
				forge = json.RawMessage(`{}`)
			}
			project, err := scanProject(tx.QueryRow(ctx,
				`INSERT INTO context_projects (tenant_id, scope, identity, display_name, forge, created_by, created_by_principal)
				 VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6::uuid,
				         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $6::uuid))
				 RETURNING `+projectSelect,
				tn.ID, p.Scope, p.Identity, p.DisplayName, forge, createdBy))
			if err != nil {
				if pgxdb.UniqueViolation(err) {
					return ErrProjectExists
				}
				return fmt.Errorf("store: provision insert project: %w", err)
			}

			// 6: repo-agent key STRICTLY to the K12 template — home = scope, allowed = []
			// (NON-NIL empty: no '{shared}' inheritance), write = [] (NON-NIL empty). The
			// role is 'member' (agents never own). This is the verbindlicher Vertrag §5.5.
			agentKey, agentPlain, err := insertApiKeyTx(ctx, tx,
				p.Slug+agentKeyLabelSuffix, p.Scope, []string{}, []string{}, tn.ID, "member")
			if err != nil {
				return err
			}

			// 7: optional per-tenant LLM-quota seed for the scope (E7) — so a fresh 10k
			// import can not monopolise the inference budget (§6.4). Same UPSERT shape as
			// UpsertTenantQuota, threaded into this tx.
			if p.Quota != nil {
				if err := upsertTenantQuotaTx(ctx, tx, p.Scope, *p.Quota, createdBy); err != nil {
					return err
				}
			}

			out = &ProvisionResult{
				Tenant:         tn,
				Scope:          p.Scope,
				Project:        project,
				OwnerKey:       ownerKey,
				OwnerPlaintext: ownerPlain,
				AgentKey:       agentKey,
				AgentPlaintext: agentPlain,
				Provisioned:    true,
			}
			return nil
		}); err != nil {
		if errors.Is(err, errTxCommitted) {
			return &ProvisionResult{Project: existing, Scope: existing.Scope, Provisioned: false}, nil
		}
		return nil, err
	}
	return out, nil
}

// upsertTenantQuotaTx is the tx-threaded twin of UpsertTenantQuota (quota.go) so
// the quota seed commits atomically with the rest of the provision. by is the
// acting key id ("" -> NULL via the any-nil path the caller passes).
func upsertTenantQuotaTx(ctx context.Context, tx pgx.Tx, scope string, q backends.TenantQuota, by any) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO context_tenant_quota
		    (scope, daily_cost_usd, monthly_cost_usd, daily_calls, on_exceed, enabled, updated_by, updated_by_principal)
		VALUES ($1,$2,$3,$4,$5,$6,$7::uuid,
		        (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $7::uuid))
		ON CONFLICT (scope) DO UPDATE SET
		    daily_cost_usd  = EXCLUDED.daily_cost_usd,
		    monthly_cost_usd = EXCLUDED.monthly_cost_usd,
		    daily_calls     = EXCLUDED.daily_calls,
		    on_exceed       = EXCLUDED.on_exceed,
		    enabled         = EXCLUDED.enabled,
		    updated_by      = EXCLUDED.updated_by,
		    updated_by_principal = EXCLUDED.updated_by_principal,
		    updated_at      = now()`,
		scope, q.DailyCostUSD, q.MonthlyCostUSD, q.DailyCalls, q.OnExceed, q.Enabled, by)
	if err != nil {
		return fmt.Errorf("store: provision quota seed: %w", err)
	}
	return nil
}
