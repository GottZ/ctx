// Playwright smoke-test fixtures (web-redesign visual verification debt, HANDOVER §8).
//
// The SPA is fully driven by `/api/**` calls; this module mocks every endpoint a
// page touches on initial load with deterministic, scope-/role-shaped payloads so
// the SHELL + per-area LAYOUT MODES (S3–S7), THEME (TH3/TH4) and GRAPH (G1–G3)
// render real content — no live backend, no real key. Wire shapes mirror
// src/lib/api/types.ts + src/lib/graph/api.ts + src/lib/api/blocks.ts verbatim
// (the Go golden tests are the drift anchor; these fixtures are NOT a second one).

import type { Page, Route } from '@playwright/test'
import { workflowMock } from './issue-fixtures'
import { ISSUES_BASE } from '../src/lib/api/issues'

export type Role = 'server-admin' | 'tenant-admin' | 'member'

/** Which tenant identity a session runs as (cross-tenant / isolation proofs). */
export type TenantKey = 'A' | 'B'

interface TenantDef {
  id: string
  slug: string
  name: string
  home: string
  read: string[]
}

// read_scopes are built PER TENANT and are NOT shared — R-LEAK5 (store/api_keys.go
// :73-74): a foreign tenant without explicit allowed_scopes inherits an EMPTY set,
// never the default tenant's 'shared'. Baking a bare 'shared' into tenant B would
// fixture-encode exactly the cross-tenant read S1 declares structurally impossible.
const TENANTS: Record<TenantKey, TenantDef> = {
  // A = default-tenant existing form: grandfathered FLAT scopes ('home','shared',
  // unprefixed). All 34 existing specs depend on this shape (whoamiFor's default).
  A: { id: '550e8400-e29b-41d4-a716-446655440aaa', slug: 'acme', name: 'Acme Corp', home: 'home', read: ['home', 'shared'] },
  // B = clean self-service tenant: ONLY its own auto-prefixed home scope, NO bare
  // 'shared'. Cross-tenant is grant-only (deliberately none here) → positive
  // isolation: a tenant-B session must never render 'shared'/'acme:*' scopes (E5).
  B: { id: '550e8400-e29b-41d4-a716-446655440bbb', slug: 'globex', name: 'Globex Inc', home: 'globex:home', read: ['globex:home'] },
}

/** The one fixture API key whoami authenticates (exported for the login contract, PV7). */
export const KEY = 'smoke-key'

/**
 * High-entropy tenant sentinel markers (design 06 §4.6/§5.6b, wave PV4): every
 * fixture tenant carries exactly ONE sentinel block in its search/list data.
 * The generated tenant-leak probe (contract.ts) asserts the OWN sentinel is
 * rendered (positive control — the detector provably sees data) and the
 * FOREIGN sentinel appears nowhere in the DOM. Deliberately not dictionary
 * words ('acme'/'shared' collide with legitimate UI copy → false reds or a
 * later loosening); the hex suffixes make an accidental copy collision
 * practically impossible.
 */
export const SENTINEL: Record<TenantKey, string> = {
  A: 'A-SENTINEL-1f9c62d84b7e',
  B: 'B-SENTINEL-8d2a41c97f30',
}

/**
 * Declarative page state for seedSession (design 06 §4.6, wave PV4).
 * 'empty' is the declarative twin of the legacy `empty: true` flag; 'error'
 * fails the core read endpoints (500) so error bands become a declarable
 * state; '10k' swaps /api/search for a synthetic 10 000-item generator with
 * REAL keyset-cursor behaviour (next_after, blocks.ts:33-37) — the target-
 * scale proofs (§6.2) never need 10k JSON rows in the repo.
 */
export type SeedState = 'default' | 'empty' | 'error' | '10k'

/**
 * WhoamiResponse (types.ts:6) shaped per tier — capabilitiesFor reads admin+role.
 * The optional `tenant` selects the identity (default 'A'); omitting it preserves
 * the exact pre-existing default-tenant shape so the 34 legacy specs do not break.
 *
 * `capabilities` (S14, wave PV4): declared SeedOptions building block for the
 * Achse-04 feature gates (e.g. workflow.enabled → viewWorkflow). The server
 * does not send this field yet — the wire shape lands together with the
 * types.ts sync in the Achse-04 wave; until then the field is absent unless a
 * seed explicitly declares flags (no drift for existing specs). Drift anchor:
 * live-tier probe 1 sees the real whoami shape (design 06 §4.7).
 */
export function whoamiFor(
  role: Role,
  tenant: TenantKey = 'A',
  capabilities?: Record<string, boolean>,
): Record<string, unknown> {
  const t = TENANTS[tenant]
  const base = {
    success: true,
    label: 'smoke-key',
    home_scope: t.home,
    read_scopes: t.read, // per tenant, NOT shared
    api_key_id: '0190000000007000800000000000ke7',
    tenant_id: t.id,
    tenant_slug: t.slug,
    tenant_display_name: t.name,
    ...(capabilities !== undefined ? { capabilities } : {}),
  }
  if (role === 'server-admin') return { ...base, admin: true, role: 'owner' }
  if (role === 'tenant-admin') return { ...base, admin: false, role: 'owner' }
  return { ...base, admin: false, role: 'member' }
}

/** A faulted manage action — injects a negative envelope BEFORE the happy default. */
export interface Fault {
  /** the manage action that should fault */
  action: string
  /** HTTP status: 200 (no-oracle, api-key family) | 400 | 403 | 409 | 429 | 500 */
  status: number
  /** body.error; falls back to DEFAULT_ERROR[status] */
  error?: string
  /** succeed for the first N calls of this action, then fault (default 0 = always) */
  afterCalls?: number
}

// Default error bodies mirror the real handler strings (the frozen contract drift
// anchor) so a Fault without an explicit `error` still matches what the live
// backend would write; per-probe specs override `error` where the assertion pins it.
const DEFAULT_ERROR: Record<number, string> = {
  200: 'key not found', // api-key family no-oracle (context_manage.go:1438 / :1671)
  400: 'invalid request', // input validation (charset / prefix injection)
  403: 'admin key required', // tier gate (context_manage.go:325/351)
  409: 'cannot remove the last active owner of the tenant', // last-owner guard
  429: 'tenant scope quota exceeded', // max_scopes/max_keys — FE-render only (RF-1)
  500: 'internal error',
}

/** Tenant context threaded into manageFixture so auto-prefix + scope-list are deterministic. */
interface FixtureCtx {
  id: string
  slug: string
  name: string
  home: string
  read: string[]
  /** explicit scope-list/scope-overview override (fresh tenant = []); undefined = default. */
  scopes?: Record<string, unknown>[]
}

/** The tenant's OWN scopes as ScopeOverview rows (counts 0, like handleScopeList). */
function tenantScopeRows(ctx: FixtureCtx): Record<string, unknown>[] {
  if (ctx.scopes) return ctx.scopes
  return ctx.read.map((s) => ({ scope: s, block_count: 0, key_count: 0, tenant_id: ctx.id }))
}

/** StatusResponse (types.ts:233) — admin-only; SSE falls back to this GET poll. */
function statusFixture(): Record<string, unknown> {
  return {
    success: true,
    as_of: '2026-06-29T12:00:00Z',
    health: { status: 'ok', services: { db: 'ok', embed: 'ok', chat: 'ok' } },
    backends: [
      {
        id: 'local-llamacpp',
        name: 'llama.cpp (local)',
        trust: 'full-trust',
        locality: 'local',
        roles: ['chat', 'dream'],
        priority: 10,
        enabled: true,
        effective_state: 'active',
        cooldown_remaining_s: 0,
        consecutive_fails: 0,
        last_ok: '2026-06-29T11:59:30Z',
      },
    ],
    dream: {
      mode: 'on',
      throttle_interval_s: 0,
      pickable_now: 4,
      in_cooldown: 1,
      never_dreamed: 12,
      awaiting_embed: 0,
      incoming_1h: 3,
      incoming_6h: 9,
      next_pending_at: null,
      last_cycle_at: '2026-06-29T11:55:00Z',
    },
    llm_24h: [
      {
        backend: 'llama.cpp (local)',
        pipeline: 'chat',
        calls: 42,
        avg_ms: 1230,
        errors: 0,
        prompt_tokens: 51200,
        completion_tokens: 8800,
      },
    ],
    llm_24h_complete: true,
    gaming: { active: false },
    activity: null,
  }
}

/** SettingView[] (types.ts:37) — a few rows so the reading-mode list has body. */
function settingsFixture(): Record<string, unknown>[] {
  return [
    { key: 'pool.default_block_sensitivity', type: 'string', mutability: 'hot', value: 'internal', source: 'db', default: 'credentials' },
    { key: 'dream.enabled', type: 'bool', mutability: 'hot', value: true, source: 'env', default: false },
    { key: 'server.timezone', type: 'timezone', mutability: 'restart', value: 'Europe/Berlin', source: 'db', default: 'UTC' },
  ]
}

/**
 * SearchResponse (graph/api.ts) — NO success field on the happy path.
 * Tenant-KEYED since PV4 (§4.6 sentinel convention): each tenant's corpus is
 * its own row set and always carries that tenant's sentinel block as the LAST
 * row — if the fixture layer ever served tenant-A rows to a tenant-B session,
 * the foreign sentinel would travel with them and the generated leak probe
 * turns red. A tenant-agnostic fixture could never prove that.
 */
function searchFixture(tenant: TenantKey): Record<string, unknown> {
  const mk = (n: number, cat: string, title: string, sens: string, preview?: string) => ({
    id: `550e8400-e29b-41d4-a716-44665544000${n}`,
    category: cat,
    tags: ['demo', cat],
    title,
    content_preview:
      preview ?? `Lorem ipsum preview for ${title} — enough text to fill a master-detail hit row in the split layout.`,
    content_length: 1840,
    scope: tenant === 'A' ? 'home' : 'globex:home',
    updated_at: '2026-06-28T10:00:00Z',
    created_at: '2026-06-01T08:00:00Z',
    sensitivity: sens,
  })
  const sentinel = mk(
    tenant === 'A' ? 4 : 7,
    'meta',
    SENTINEL[tenant],
    'internal',
    'Tenant-isolation sentinel block — must never render in a foreign tenant session (design 06 §5.6b).',
  )
  const rows =
    tenant === 'A'
      ? [
          mk(1, 'design', 'Core Architecture', 'internal'),
          mk(2, 'reference', 'API Spec', 'public'),
          mk(3, 'learnings', 'Retrieval Findings', 'internal'),
          sentinel,
        ]
      : [mk(5, 'design', 'Globex Onboarding', 'internal'), mk(6, 'reference', 'Globex Runbook', 'public'), sentinel]
  return { count: rows.length, results: rows, next_after: null }
}

// ---- '10k' scale state (design 06 §4.6/§6.2, wave PV4) ----------------------
// Synthetic corpus generator with REAL keyset-cursor semantics: 10 000 items,
// strictly updated_at-DESC with the id tiebreak, paged by the request's limit
// (server clamp 50, server default 10 — context_search.go), resumed via the
// {after_updated, after_id} cursor exactly like store.SearchCursor. Generator
// instead of file: 10k JSON rows do not belong in the repo.

const SCALE_TOTAL = 10_000
const SCALE_BASE_MS = Date.UTC(2026, 5, 28, 10, 0, 0)

function scaleId(i: number): string {
  return `550e8400-e29b-41d4-a716-${String(i).padStart(12, '0')}`
}

function scaleUpdatedAt(i: number): string {
  // Strictly descending, one minute apart — a real keyset ordering.
  return new Date(SCALE_BASE_MS - i * 60_000).toISOString()
}

function scaleRow(i: number): Record<string, unknown> {
  const cats = ['design', 'reference', 'learnings', 'infrastructure', 'decisions']
  return {
    id: scaleId(i),
    category: cats[i % cats.length],
    tags: ['scale', cats[i % cats.length]],
    title: `Scale Block ${String(i).padStart(5, '0')}`,
    content_preview: `Synthetic 10k-state row ${i} — keyset-paged scale corpus (design 06 §6.2).`,
    content_length: 512,
    scope: 'home',
    updated_at: scaleUpdatedAt(i),
    created_at: scaleUpdatedAt(i),
    sensitivity: 'internal',
  }
}

/** One keyset page of the synthetic 10k corpus (mirrors context_search.go paging). */
function scaleSearchFixture(body: Record<string, unknown> | null): Record<string, unknown> {
  const limit = typeof body?.limit === 'number' ? body.limit : 10 // server default 10
  const pageSize = Math.max(1, Math.min(limit, 50)) // server clamp 50
  const after = body?.after as { after_id?: string } | undefined
  // The cursor's after_id embeds the row index (scaleId) — resume strictly after it.
  const start = after?.after_id ? parseInt(after.after_id.slice(-12), 10) + 1 : 0
  const end = Math.min(start + pageSize, SCALE_TOTAL)
  const results: Record<string, unknown>[] = []
  for (let i = start; i < end; i++) results.push(scaleRow(i))
  return {
    count: results.length,
    results,
    next_after:
      end < SCALE_TOTAL ? { after_updated: scaleUpdatedAt(end - 1), after_id: scaleId(end - 1) } : null,
  }
}

/** OverviewResponse (graph/api.ts) — the cluster map for the canvas-mode graph. */
function overviewFixture(): Record<string, unknown> {
  return {
    success: true,
    params: {},
    nodes: [
      { cluster: 0, size: 12, top_categories: ['design', 'code'], repr_id: '550e8400-e29b-41d4-a716-446655440001', repr_title: 'Architecture', scope_mix: ['home'] },
      { cluster: 1, size: 7, top_categories: ['reference'], repr_id: '550e8400-e29b-41d4-a716-446655440002', repr_title: 'API Spec', scope_mix: ['shared'] },
      { cluster: 2, size: 4, top_categories: ['learnings'], repr_id: '550e8400-e29b-41d4-a716-446655440003', repr_title: 'Findings', scope_mix: ['home'] },
    ],
    edges: [
      [0, 1, 3, 0.8],
      [1, 2, 1, 0.4],
    ],
    stats: { nodes: 3, edges: 2, truncated: false, computed_at: '2026-06-29T12:00:00Z', elapsed_ms: 42 },
  }
}

/** EgoResponse (graph/api.ts) — focused ego graph (deep-link ?focus=…). */
function egoFixture(): Record<string, unknown> {
  return {
    success: true,
    focus: '550e8400-e29b-41d4-a716-446655440001',
    params: { hops: 2 },
    rels: ['references', 'links_to', 'supersedes'],
    nodes: [
      { id: '550e8400-e29b-41d4-a716-446655440001', title: 'Architecture', category: 'design', scope: 'home', degree: 5, hop: 0, created_at: '2026-06-01T08:00:00Z' },
      { id: '550e8400-e29b-41d4-a716-446655440002', title: 'API Spec', category: 'reference', scope: 'home', degree: 2, hop: 1, created_at: '2026-06-02T09:30:00Z' },
      { id: '550e8400-e29b-41d4-a716-446655440003', title: 'Findings', category: 'learnings', scope: 'home', degree: 1, hop: 1, created_at: '2026-06-03T10:15:00Z' },
    ],
    edges: [
      [0, 1, 0, 0.95],
      [0, 2, 1, 0.87],
    ],
    stats: { nodes: 3, edges: 2, truncated: false, elapsed_ms: 28 },
  }
}

/** ApiKeyView[] (types.ts:568) for the tenant key table; first row = own key. */
function apiKeysFixture(): Record<string, unknown>[] {
  return [
    { id: '0190000000007000800000000000ke7', label: 'smoke-key', home_scope: 'home', allowed_scopes: ['home', 'shared'], active: true, last_used_at: '2026-06-29T11:00:00Z', created_at: '2026-05-01T08:00:00Z', tenant_role: 'owner' },
    { id: '0190000000007000800000000000ke8', label: 'ci-runner', home_scope: 'home', allowed_scopes: ['home'], active: true, created_at: '2026-06-10T08:00:00Z', tenant_role: 'member' },
  ]
}

/** POST /api/manage dispatch — action-keyed, mirrors the handler envelopes. */
function manageFixture(
  action: string | undefined,
  _role: Role,
  ctx: FixtureCtx,
  data: Record<string, unknown> | undefined,
): Record<string, unknown> {
  switch (action) {
    case 'list-categories':
      return { success: true, categories: [
        { category: 'design', count: 12 },
        { category: 'reference', count: 7 },
        { category: 'learnings', count: 4 },
      ] }
    case 'list-meta': {
      // Tenant-keyed like searchFixture (§4.6): the A–Z nav index carries the
      // OWN tenant's rows + sentinel, never a foreign tenant's.
      const tkey: TenantKey = ctx.slug === TENANTS.A.slug ? 'A' : 'B'
      const rows =
        tkey === 'A'
          ? [
              { id: '550e8400-e29b-41d4-a716-446655440001', category: 'design', title: 'Core Architecture', tags: ['demo'], scope: 'home', updated_at: '2026-06-28T10:00:00Z' },
              { id: '550e8400-e29b-41d4-a716-446655440002', category: 'reference', title: 'API Spec', tags: ['demo'], scope: 'home', updated_at: '2026-06-27T10:00:00Z' },
              { id: '550e8400-e29b-41d4-a716-446655440004', category: 'meta', title: SENTINEL.A, tags: ['sentinel'], scope: 'home', updated_at: '2026-06-26T10:00:00Z' },
            ]
          : [
              { id: '550e8400-e29b-41d4-a716-446655440005', category: 'design', title: 'Globex Onboarding', tags: ['demo'], scope: 'globex:home', updated_at: '2026-06-28T10:00:00Z' },
              { id: '550e8400-e29b-41d4-a716-446655440007', category: 'meta', title: SENTINEL.B, tags: ['sentinel'], scope: 'globex:home', updated_at: '2026-06-26T10:00:00Z' },
            ]
      return { success: true, blocks: rows }
    }
    case 'get':
      return { success: true, block: {
        id: '550e8400-e29b-41d4-a716-446655440001',
        category: 'design',
        tags: ['demo', 'design'],
        title: 'Core Architecture',
        content: 'The ctx graph is the first true canvas-first surface. This mock body fills the reading-measure prose column so the master-detail and floating-window line length stays bounded.',
        scope: 'home',
        sensitivity: 'internal',
        created_at: '2026-06-01T08:00:00Z',
        updated_at: '2026-06-28T10:00:00Z',
      } }
    case 'api-key-list':
      return { success: true, keys: apiKeysFixture() }
    case 'api-key-create':
      return {
        success: true,
        id: '0190000000007000800000000000ke9',
        label: 'new-key',
        home_scope: 'home',
        allowed_scopes: ['home'],
        // Plaintext shown exactly once — the reveal-once dialog must surface it
        // and never persist it (design 05 §6).
        api_key: 'ctx_sk_TESTKEY_reveal_once_do_not_persist',
      }
    case 'api-key-delete':
      return { success: true, deleted: '0190000000007000800000000000ke8' }
    case 'tenant-quota-get':
      return {
        success: true,
        quota: {
          scope: 'home',
          enabled: true,
          daily_cost_usd: 5,
          monthly_cost_usd: 100,
          daily_calls: 1000,
          on_exceed: 'external_off',
        },
      }
    case 'tenant-list':
      return { success: true, tenants: [
        { id: '550e8400-e29b-41d4-a716-446655440aaa', slug: 'acme', display_name: 'Acme Corp', status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' },
      ] }
    case 'tenant-get':
      return { success: true, tenant: { id: '550e8400-e29b-41d4-a716-446655440aaa', slug: 'acme', display_name: 'Acme Corp', status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' } }
    case 'tenant-quota-set':
      return { success: true, quota: { scope: 'home', enabled: true, daily_cost_usd: 9, monthly_cost_usd: 100, daily_calls: 1000, on_exceed: 'external_off' } }
    case 'scope-overview':
      // server-admin global landscape (unscoped). An explicit opts.scopes override
      // (fresh tenant = []) wins; otherwise the existing 3-row default — keeping the
      // 34 legacy specs (A0-FE scope-map) unchanged.
      return { success: true, scopes: ctx.scopes ?? [
        { scope: 'home', block_count: 128, key_count: 3, tenant_id: '550e8400-e29b-41d4-a716-446655440aaa' },
        { scope: 'shared', block_count: 42, key_count: 8, tenant_id: '550e8400-e29b-41d4-a716-446655440aaa' },
        { scope: 'legacy', block_count: 7, key_count: 1, tenant_id: null },
      ] }
    // --- Self-service frozen contract (design 06 §1/§2.5, re-verified against the
    // real handlers tenant_manage.go + context_manage.go — the committed shapes). ---
    case 'scope-create': {
      // ScopeCreateResult (types.ts:675): SLIM { success, scope, tenant_id } — `scope`
      // is the FULL server-built '<slug>:<name>'. The prefix uses the TARGET tenant's
      // slug (ctx.slug), never a hardcoded 'acme' nor the naive concat of any data.name.
      const name = (data?.name as string) ?? 'research'
      return { success: true, scope: `${ctx.slug}:${name}`, tenant_id: ctx.id }
    }
    case 'scope-list':
      // ScopeOverviewListResponse, tenant-scoped (handleScopeList): server-side
      // filtered onto ar.TenantID, counts 0 by design. Re-list source after scope-create.
      return { success: true, scopes: tenantScopeRows(ctx) }
    case 'api-key-update':
      // ApiKeyUpdateResult (types.ts:652): { success, key: ApiKeyView } — the re-read
      // row carrying the new tenant_role/active (handleApiKeyUpdate 200 path).
      return { success: true, key: {
        id: (data?.id as string) ?? '0190000000007000800000000000ke8',
        label: 'ci-runner',
        home_scope: ctx.home,
        allowed_scopes: [ctx.home],
        active: (data?.active as boolean | undefined) ?? true,
        last_used_at: '2026-06-29T11:00:00Z',
        created_at: '2026-06-10T08:00:00Z',
        tenant_role: (data?.tenant_role as string | undefined) ?? 'member',
      } }
    case 'tenant-create': {
      // TenantCreateResult (types.ts:692): FLAT compound { success, tenant, scope,
      // owner_key_id, owner_key } — the handler ALWAYS mints the owner key (K10), so
      // owner_key is unconditionally present (matches the required-field frozen type).
      // scope = '<slug>:main'; owner_key is the reveal-once plaintext, never persisted.
      const slug = (data?.slug as string) ?? 'globex'
      const displayName = (data?.display_name as string) ?? 'Globex Inc'
      return {
        success: true,
        tenant: { id: '550e8400-e29b-41d4-a716-446655440ccc', slug, display_name: displayName, status: 'active', created_at: '2026-06-30T12:00:00Z', updated_at: '2026-06-30T12:00:00Z', max_scopes: 25, max_keys: 50 },
        scope: `${slug}:main`,
        owner_key_id: '0190000000007000800000000000ow1',
        owner_key: 'ctx_sk_TESTOWNER_reveal_once_do_not_persist',
      }
    }
    case 'tenant-usage-get':
      // TenantUsageResponse (types.ts:723): { success, usage } — structural usage +
      // limits, pinned to ctx.id (handleTenantUsageGet pins non-server-admin → ar.TenantID).
      return { success: true, usage: {
        tenant_id: ctx.id,
        max_scopes: 25,
        max_keys: 50,
        scope_count: tenantScopeRows(ctx).length,
        key_count: 2,
      } }
    case 'tenant-limit-set': {
      // TenantResponse (types.ts) — { success, tenant } echoing the stored row with
      // the patched caps (handleTenantLimitSet re-reads). null = unlimited per dimension.
      const ms = data && 'max_scopes' in data ? (data.max_scopes as number | null) : 25
      const mk = data && 'max_keys' in data ? (data.max_keys as number | null) : 50
      return { success: true, tenant: { id: ctx.id, slug: ctx.slug, display_name: ctx.name, status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-30T12:00:00Z', max_scopes: ms, max_keys: mk } }
    }
    case 'backend-list':
      return { success: true, backends: [] }
    // A7 corpus maintenance — start kicks off (running), status reports it
    // finished so the poll loop converges to a terminal state on the next tick.
    case 'blocks-audit-start':
      return { success: true, scope: 'home', pending: 30, by_source: { 'llm-audit': 12 }, run: auditRun(true) }
    case 'blocks-audit-status':
      return { success: true, scope: 'home', pending: 30, by_source: { 'llm-audit': 12 }, run: auditRun(false) }
    case 'blocks-classify-start':
      return { success: true, scope: 'home', by_source: { default: 8 }, run: classifyRun(true) }
    case 'blocks-classify-status':
      return { success: true, scope: 'home', by_source: { default: 8 }, run: classifyRun(false) }
    default:
      // HARD default (design 06 §2.3). Was {success:true} — it absorbed EVERY
      // un-mocked action silently, the single highest false-positive risk (Inventur
      // 06 §1). success:false makes apiFetch throw an ApiError (api.ts:103, even in a
      // 2xx), so a forgotten new action fails LOUDLY instead of passing green.
      return { __unmocked: true, success: false, error: `unmocked manage action: ${action}` }
  }
}

function auditRun(running: boolean): Record<string, unknown> {
  return running
    ? { running: true, dry_run: true, processed: 0, kept_credentials: 0, to_personal: 0, to_internal: 0, no_verdict: 0, discarded: 0, aborted: false }
    : { running: false, dry_run: true, processed: 30, kept_credentials: 2, to_personal: 5, to_internal: 20, no_verdict: 3, discarded: 0, aborted: false, finished_at: '2026-06-29T12:00:00Z' }
}

function classifyRun(running: boolean): Record<string, unknown> {
  return running
    ? { running: true, dry_run: true, scanned: 0, upgraded: 0, discarded: 0, aborted: false }
    : { running: false, dry_run: true, scanned: 40, upgraded: 3, discarded: 0, aborted: false, finished_at: '2026-06-29T12:00:00Z' }
}

/** Empty-corpus variants (N8 empty-state coverage). */
function emptySearchFixture(): Record<string, unknown> {
  return { count: 0, results: [], next_after: null }
}
function emptyOverviewFixture(): Record<string, unknown> {
  return { success: true, params: {}, nodes: [], edges: [], stats: { nodes: 0, edges: 0, truncated: false, computed_at: null, elapsed_ms: 5 } }
}

/** seedSession options — named + exported since PV4 (the PageContract seed builds on it). */
export interface SeedOptions {
  role: Role
  theme: 'light' | 'dark'
  /**
   * PV7 (login contract): install the mocks + theme pref but do NOT plant the
   * sessionStorage key — the app boots to the Login mask instead of the shell.
   * The whoami mock still answers for the canonical KEY, so a login ATTEMPT
   * with the right key succeeds and a wrong key gets the 401 error band
   * (the auth-header branch below).
   */
  anonymous?: boolean
  empty?: boolean
  /** identity tenant (default 'A' = legacy default-tenant shape). */
  tenant?: TenantKey
  /** negative-probe injection — faults a specific manage action (§2.2). */
  faults?: Fault[]
  /** scope-list / scope-overview override (fresh tenant = []). */
  scopes?: Record<string, unknown>[]
  /**
   * Declarative page state (design 06 §4.6, PV4): 'empty' ≙ empty:true,
   * 'error' fails the core reads (500), '10k' serves the synthetic keyset-
   * paged scale corpus on /api/search. Default 'default'.
   */
  state?: SeedState
  /** Capability flags threaded into whoamiFor (S14 — Achse-04 feature gates). */
  capabilities?: Record<string, boolean>
}

/** One recorded /api/** request (mock-call recorder, design 06 §4.1 deny dimension). */
export interface RecordedCall {
  method: string
  path: string
  /** manage action, when the call is POST /api/manage. */
  action?: string
  /** parsed JSON body for POST /api/manage + /api/search (cursor proofs). */
  body?: unknown
  /** request query string incl. leading '?' (workflow GETs carry the list
   * filters + the keyset ?after cursor here — the U05 scale round-trip proof). */
  query?: string
}

/** Handle returned by seedSession — the generated deny/scale tests read `calls`. */
export interface SeededSession {
  calls: RecordedCall[]
}

/**
 * Seed an authenticated session (sessionStorage key, restored on App mount) +
 * a deterministic theme pref (localStorage, read by theme-boot.js before first
 * paint), then install the `/api/**` mocks. Must run BEFORE page.goto().
 * Returns the mock-call recorder handle (every /api/** request is logged) —
 * existing call sites may ignore it (non-breaking).
 */
export async function seedSession(page: Page, opts: SeedOptions): Promise<SeededSession> {
  await page.addInitScript(
    ({ key, theme }) => {
      try {
        if (key !== null) sessionStorage.setItem('ctx.api-key', key)
        localStorage.setItem('ctx.theme', theme)
      } catch {
        /* storage blocked — test will surface it as a render failure */
      }
    },
    { key: opts.anonymous === true ? null : KEY, theme: opts.theme },
  )

  const tkey: TenantKey = opts.tenant ?? 'A'
  const t = TENANTS[tkey]
  const ctx: FixtureCtx = { id: t.id, slug: t.slug, name: t.name, home: t.home, read: t.read, scopes: opts.scopes }
  const state: SeedState = opts.state ?? 'default'
  // 'empty' is the declarative twin of the legacy boolean flag (both stay valid).
  const empty = opts.empty === true || state === 'empty'
  const session: SeededSession = { calls: [] }
  // Per-session, per-action call counter — drives Fault.afterCalls (quota-exhaustion
  // sequences: e.g. first scope-create ok, second → 429).
  const callIndex: Record<string, number> = {}
  // A3b lifecycle is STATEFUL: a tenant-update records the new status (keyed by
  // tenant id) so a following tenant-get / tenant-list reflects the toggle — the
  // suspend→suspended→activate e2e only proves anything if the read paths move.
  const tenantStatus: Record<string, string> = {}

  await page.route('**/health', (route: Route) =>
    route.fulfill({ json: { status: 'ok', services: { db: 'ok', embed: 'ok' } } }),
  )

  await page.route('**/api/**', async (route: Route) => {
    const req = route.request()
    const url = new URL(req.url())
    const path = url.pathname
    const search = url.search
    const method = req.method()

    // Mock-call recorder (PV4): every /api/** request is logged BEFORE dispatch
    // so the generated deny tests can assert admin-call ABSENCE and the scale
    // tests can prove the keyset-cursor round-trip from the actual wire bodies.
    const call: RecordedCall = { method, path }
    // Workflow GETs carry their filters + keyset cursor in the query string
    // (the issue endpoints are GET, not POST) — record it for the U05 scale
    // cursor round-trip proof (the blocks scale proves it from the POST body).
    if (search !== '' && (path === ISSUES_BASE || path.startsWith(`${ISSUES_BASE}/`))) call.query = search
    if (method === 'PUT' && path.startsWith('/api/settings/')) {
      // Settings edit-roundtrip (PV7): the PUT body is the postData proof the
      // /settings primaryFlow asserts on (design 06 §7-PV7).
      try {
        call.body = req.postDataJSON()
      } catch {
        /* non-JSON body — recorded without payload */
      }
    }
    if (method === 'POST' && (path === '/api/manage' || path === '/api/search')) {
      try {
        call.body = req.postDataJSON()
        const a = (call.body as { action?: unknown } | null)?.action
        if (typeof a === 'string') call.action = a
      } catch {
        /* non-JSON body — recorded without payload */
      }
    }
    session.calls.push(call)

    // SSE telemetry stream → abort so StatusPage uses the GET /api/status poll.
    if (path === '/api/events') return route.abort()

    if (path === '/api/whoami') {
      // Auth-header branch (PV7 login contract): whoami authenticates ONLY the
      // canonical fixture key. A login attempt with any other key gets the real
      // handler's 401 envelope — the Fehl-Key path renders the error band and
      // NEVER the shell. Every pre-existing spec seeds KEY, so the happy path
      // is byte-identical for them. 401 with an EXPLICIT key does not fire
      // hooks.onUnauthorized (api.ts:96) — no session teardown side effects.
      const auth = req.headers()['authorization']
      if (auth !== `Bearer ${KEY}`) {
        return route.fulfill({ status: 401, json: { success: false, error: 'invalid or revoked API key' } })
      }
      return route.fulfill({ json: whoamiFor(opts.role, tkey, opts.capabilities) })
    }

    // 'error' state (declarative, §4.6): the core READ surfaces fail with a 500
    // envelope so every page's error band becomes a declarable contract state.
    // whoami stays green — the session must resolve for the shell to mount.
    if (
      state === 'error' &&
      (path === '/api/search' ||
        path === '/api/status' ||
        path === '/api/llmlog' ||
        (path === '/api/settings' && method === 'GET') ||
        path.startsWith('/api/graph/') ||
        path.startsWith('/api/chat/sessions'))
    ) {
      return route.fulfill({ status: 500, json: { success: false, error: 'internal error' } })
    }

    if (path === '/api/status') return route.fulfill({ json: statusFixture() })
    if (path === '/api/llmlog') return route.fulfill({ json: { success: true, entries: [] } })
    if (path === '/api/settings' && method === 'GET') return route.fulfill({ json: { success: true, settings: settingsFixture() } })
    if (path.startsWith('/api/settings/') && method === 'PUT') {
      // SettingPutResponse (types.ts:55) — echoes the written value with
      // source 'db' and the previous value/source from the catalog fixture
      // (mirrors HandlePut's re-read). The PV7 /settings primaryFlow proves
      // the postData on the recorded call, the UI proves the applied echo.
      const key = decodeURIComponent(path.slice('/api/settings/'.length))
      const prev = settingsFixture().find((s) => s.key === key)
      const value = (call.body as { value?: unknown } | undefined)?.value
      return route.fulfill({
        json: {
          success: true,
          key,
          value,
          source: 'db',
          previous: { value: prev?.value ?? null, source: prev?.source ?? 'default' },
          warnings: [],
        },
      })
    }
    if (path === '/api/secrets' && method === 'GET') return route.fulfill({ json: { success: true, secrets: [] } })
    if (path.startsWith('/api/graph/overview')) return route.fulfill({ json: empty ? emptyOverviewFixture() : overviewFixture() })
    if (path.startsWith('/api/graph/ego')) return route.fulfill({ json: egoFixture() })
    if (path === '/api/chat/sessions') return route.fulfill({ json: { success: true, sessions: [] } })
    if (path.startsWith('/api/chat/sessions/')) {
      // Detail default, shape-correct (ChatSessionDetailResponse, chat/types.ts
      // :59): the old startsWith branch answered the LIST shape for a detail
      // GET — never hit before PV7, but a wrong-shape default is exactly the
      // W10 drift the fixtures must not carry. Flows that need real messages
      // override this route per test (later page.route registrations win).
      const id = decodeURIComponent(path.slice('/api/chat/sessions/'.length))
      if (method === 'DELETE') return route.fulfill({ json: { success: true } })
      return route.fulfill({
        json: {
          success: true,
          session: { id, title: 'Conversation', scope: ctx.home, max_sensitivity: 'internal', created_at: '2026-06-29T11:00:00Z', updated_at: '2026-06-29T12:00:00Z' },
          messages: [],
        },
      })
    }
    if (path === '/api/search') {
      if (state === '10k') {
        return route.fulfill({ json: scaleSearchFixture((call.body as Record<string, unknown> | undefined) ?? null) })
      }
      return route.fulfill({ json: empty ? emptySearchFixture() : searchFixture(tkey) })
    }
    if (path === '/api/manage' && method === 'POST') {
      let action: string | undefined
      let data: Record<string, unknown> | undefined
      let id: string | undefined
      let status: string | undefined
      try {
        const body = req.postDataJSON() as
          | { action?: string; data?: Record<string, unknown>; id?: string; status?: string }
          | null
        action = body?.action
        data = body?.data
        id = body?.id // top-level on tenant-get/-update/-delete (tenant_manage.go)
        status = body?.status // top-level on tenant-update (req.Status)
      } catch {
        action = undefined
      }
      // Fault seam (§2.2) — ACTION-BRANCHED: only the targeted action faults; every
      // other action falls through to the happy manageFixture layer, so a blindly
      // fulfilled 4xx can never corrupt a subsequent unrelated manage call. afterCalls
      // lets the first N calls succeed before faulting (sequence/quota probes).
      const fault = (opts.faults ?? []).find((f) => f.action === action)
      if (fault && action) {
        const n = callIndex[action] ?? 0
        callIndex[action] = n + 1
        if (n >= (fault.afterCalls ?? 0)) {
          return route.fulfill({
            status: fault.status,
            json: { success: false, error: fault.error ?? DEFAULT_ERROR[fault.status] ?? 'error' },
          })
        }
      }
      // --- A3b stateful tenant lifecycle (suspend/activate/delete) ---
      // tenant-update records the status; tenant-get/-list below reflect it.
      if (action === 'tenant-update' && typeof id === 'string') {
        if (typeof status === 'string') tenantStatus[id] = status
        const base = manageFixture('tenant-get', opts.role, ctx, undefined) as { tenant: Record<string, unknown> }
        const tenant: Record<string, unknown> = { ...base.tenant, id, status: tenantStatus[id] ?? base.tenant.status }
        if (data && 'display_name' in data) tenant.display_name = data.display_name
        return route.fulfill({ json: { success: true, tenant } })
      }
      if (action === 'tenant-delete' && typeof id === 'string') {
        return route.fulfill({ json: { success: true, deleted: id } })
      }

      const fixture = manageFixture(action, opts.role, ctx, data)
      // Overlay a recorded status override onto the read paths so the toggle shows.
      if (action === 'tenant-get') {
        const t = (fixture as { tenant?: Record<string, unknown> }).tenant
        if (t && typeof t.id === 'string' && tenantStatus[t.id]) t.status = tenantStatus[t.id]
      } else if (action === 'tenant-list') {
        const list = (fixture as { tenants?: Record<string, unknown>[] }).tenants
        if (Array.isArray(list)) {
          for (const t of list) if (typeof t.id === 'string' && tenantStatus[t.id]) t.status = tenantStatus[t.id]
        }
      }
      return route.fulfill({ json: fixture })
    }

    // Workflow namespace (U03): the ISSUES_BASE (/api/project) + TYPES_BASE
    // (/api/types) families answer from the contract-freeze JSONs (Go-golden
    // pinned). workflowMock returns null for paths OUTSIDE the namespace (fall
    // through to the generic catch-all) and its OWN loud 599 for an un-mocked
    // path INSIDE it — the closed N3 benign catch-all can never swallow these.
    const wf = workflowMock(method, path, { search, state, empty })
    if (wf) return route.fulfill({ status: wf.status, json: wf.json })

    // Unmapped /api/** → HARD-FAIL (design 06 §4.6, wave PV2). Was a benign
    // {success:true} — it absorbed every un-mocked endpoint silently (seam S5).
    // Status 599 (non-2xx) fails ALL THREE consumer classes loudly: apiFetch
    // throws via !res.ok (api.ts:99-101); streamTurn throws the pre-stream
    // ApiError (stream.ts:44-48 — a 200 JSON body would pass its gate and
    // resolve as a silently EMPTY turn); SseClient goes to status 'error'
    // (sse.svelte.ts:44) instead of a clean-EOF reconnect loop. 401 is
    // deliberately avoided: it would trigger hooks.onUnauthorized and tear the
    // session down mid-test (api.ts:95-96). Self-tested in meta.spec.ts.
    return route.fulfill({
      status: 599,
      json: { __unmocked: true, success: false, error: `unmocked endpoint: ${method} ${path}` },
    })
  })

  return session
}

/** One named SSE frame for sseRoute. */
export interface SseFrame {
  event: string
  data: unknown
}

/**
 * SSE mock building block (design 06 §4.6, wave PV2): serve a route as a
 * deterministic `text/event-stream` response built from the given frames. The
 * body is delivered atomically; the app's eventsource-parser (stream.ts /
 * sse.svelte.ts) parses the frames sequentially — no chunk timing, no race.
 * Register AFTER seedSession: later page.route registrations take precedence,
 * so this overrides the `/api/**` 599 catch-all (and, for '/api/events', the
 * abort default) exactly where a test needs real frames. Frame SEMANTICS are
 * mock-testable here; transport BEHAVIOUR (reconnect, server timing) stays
 * live-tier domain (PV10).
 */
export async function sseRoute(page: Page, url: string, frames: SseFrame[]): Promise<void> {
  await page.route(url, (route: Route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      body: frames.map((f) => `event: ${f.event}\ndata: ${JSON.stringify(f.data)}\n\n`).join(''),
    }),
  )
}

/** Collect uncaught page exceptions; assert this stays empty after a render. */
export function trackPageErrors(page: Page): string[] {
  const errors: string[] = []
  page.on('pageerror', (err) => errors.push(err.message))
  return errors
}

/**
 * Navigate to an SPA area and wait for the routed content to mount. The smoke
 * runs against the production build (vite preview), whose static hashed chunks
 * load deterministically, so a single goto suffices. The content wait also
 * covers the boot-time landing/guard redirect (N4/N5): after a redirect the
 * target area's component mounts under main.content.
 */
export async function gotoArea(page: Page, path: string): Promise<void> {
  await page.goto(path)
  await page.locator('.shell').waitFor({ state: 'visible', timeout: 10_000 })
  await page.locator('main.content > *').first().waitFor({ state: 'attached', timeout: 10_000 })
}
