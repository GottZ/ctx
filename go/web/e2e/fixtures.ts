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
 * scale proofs (§6.2) never need 10k JSON rows in the repo. 'board' / 'board-10k'
 * (U07) serve the multi-column board world + a workflow-config /api/types the
 * freeze fixtures lack (issue-fixtures.ts).
 */
export type SeedState = 'default' | 'empty' | 'error' | '10k' | 'board' | 'board-10k'

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
  capabilities?: Record<string, unknown>,
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
  500: 'Internal server error',
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
    // Disable-profile registry line (U01-W7) — replaces the retired gaming
    // field. The eject profile is the default operator quick-toggle; the rich
    // toggle/blackout scenarios are injected per-test (status-profiles.spec.ts).
    profiles: [{ name: 'eject', scope: '_global', label: 'Eject-Modus', active: false, member_count: 2 }],
    activity: null,
  }
}

/** SettingView[] (types.ts:37) — a few rows so the reading-mode list has body.
 *  The six dream.backoff_* keys feed the BackoffCurve editor (a missing key
 *  would render its "curve paused" notice instead of the curve); description
 *  mirrors the registry keyDescriptions surface. */
function settingsFixture(): Record<string, unknown>[] {
  return [
    { key: 'pool.default_block_sensitivity', type: 'string', mutability: 'hot', value: 'internal', source: 'db', default: 'credentials', description: 'Sensitivity assigned to new blocks without an explicit classification' },
    { key: 'dream.enabled', type: 'bool', mutability: 'hot', value: true, source: 'env', default: false, description: 'Master switch for the background dream pipeline' },
    { key: 'dream.backoff_mode', type: 'string', mutability: 'hot', value: 'exp', source: 'default', default: 'exp', description: 'Curve shaping how the re-dream interval grows with a block eval count' },
    { key: 'dream.backoff_factor', type: 'float', mutability: 'hot', value: 1.6, source: 'default', default: 1.6, description: 'Curve steepness; higher cools mature blocks off faster' },
    { key: 'dream.backoff_grace', type: 'int', mutability: 'hot', value: 1, source: 'db', default: 0, description: 'Free eval cycles before the back-off curve starts growing' },
    { key: 'dream.backoff_cap', type: 'hours', mutability: 'hot', value: '45d', source: 'default', default: '45d', description: 'Ceiling on the re-dream cooldown' },
    { key: 'dream.backoff_min', type: 'hours', mutability: 'hot', value: '12h', source: 'default', default: '12h', description: 'Cooldown floor at eval count 0' },
    { key: 'dream.backoff_inert_offset', type: 'int', mutability: 'hot', value: 2, source: 'db', default: 7, description: 'Extra curve steps applied when a cycle finds no links' },
    { key: 'server.timezone', type: 'timezone', mutability: 'restart', value: 'Europe/Berlin', source: 'db', default: 'UTC', description: 'IANA timezone used to render timestamps' },
    // The superseded chat.host row left with the mechanic in β9 (E11): the key
    // is out of the registry since β8 and the wire field it carried no longer
    // exists, so the fixture can no longer produce the legacy pseudo-group the
    // Stufe-1-Contract asserted. The contract now asserts its ABSENCE
    // (e2e/contract/registry.ts, PV4 settings).
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
    rels: ['topical', 'factual', 'causal', 'recurrent', 'supersedes'],
    nodes: [
      { id: '550e8400-e29b-41d4-a716-446655440001', title: 'Architecture', category: 'design', scope: 'home', degree: 5, hop: 0, created_at: '2026-06-01T08:00:00Z' },
      { id: '550e8400-e29b-41d4-a716-446655440002', title: 'API Spec', category: 'reference', scope: 'home', degree: 2, hop: 1, created_at: '2026-06-02T09:30:00Z' },
      { id: '550e8400-e29b-41d4-a716-446655440003', title: 'Findings', category: 'learnings', scope: 'home', degree: 1, hop: 1, created_at: '2026-06-03T10:15:00Z' },
    ],
    edges: [
      [0, 1, 0, 0.95],
      [0, 2, 1, 0.87],
    ],
    // Structural facts (GA2, additive — old specs unaffected): one collision
    // pair (0→1 also carries a dream edge) + one structural-only edge.
    // GC1: plus a struct↔struct parallel pair (duplicate-of on the SAME 0→1
    // pair, legal per PK source,target,link_class M076) — pins the
    // parallel-index curvature separation (design 03-§4.2).
    struct_rels: ['references', 'duplicate-of'],
    origins: ['system'],
    structural_edges: [
      [0, 1, 0, 0],
      [0, 2, 0, 0],
      [0, 1, 1, 0],
    ],
    stats: { nodes: 3, edges: 2, structural_edges: 3, truncated: false, elapsed_ms: 28 },
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
    // Cross-tenant read grants (U11, tenant_manage.go:511-555). tenant-grant-list
    // returns the grants where THIS tenant is the grantee (the client narrows by
    // grantee_tenant); one deterministic row so the TenantGrantsPanel visual/aria
    // baseline has body. Create echoes the TenantGrant it just made (delete is a
    // top-level-id intercept in the dispatch below, mirroring tenant-delete).
    case 'tenant-grant-list':
      return { success: true, grants: [
        { id: '550e8400-e29b-41d4-a716-4466554400f1', grantee_tenant: ctx.id, granted_scope: 'shared', created_at: '2026-06-20T08:00:00Z', created_by: '0190000000007000800000000000ke7' },
      ] }
    case 'tenant-grant-create':
      return { success: true, grant: {
        id: '550e8400-e29b-41d4-a716-4466554400f2',
        grantee_tenant: (data?.grantee_tenant as string) ?? ctx.id,
        granted_scope: (data?.granted_scope as string) ?? 'other:main',
        created_at: '2026-06-30T12:00:00Z',
        created_by: '0190000000007000800000000000ke7',
      } }
    case 'backend-list':
      return { success: true, backends: [] }
    // Disable-profiles (092, U01-W6). The DEFAULT is an empty registry so the
    // shared backends pages (contract/tenant-backends) render the card's empty
    // state — the rich scenarios (members, role-blackout) are injected per-test
    // in disable-profiles.spec.ts via a route override. The mutating actions
    // echo a benign shape so a stray call never hits the 599 hard-default.
    case 'disable-profile-list':
      return { success: true, profiles: [] }
    case 'disable-profile-toggle':
      return {
        success: true,
        profile: {
          name: (data?.name as string) ?? 'eject',
          scope: (data?.scope as string) ?? '_global',
          label: '',
          description: '',
          active: (data?.active as boolean | undefined) ?? false,
          reserved: false,
        },
        impact: { backends: [], roles_affected: [], roles_blacked_out: [] },
        as_of: '2026-06-29T12:00:00Z',
        note: 'ok',
        ...(data?.dry_run === true ? { dry_run: true } : {}),
      }
    case 'disable-profile-create':
    case 'disable-profile-update':
      return {
        success: true,
        profile: {
          name: (data?.name as string) ?? 'eject',
          scope: (data?.scope as string) ?? '_global',
          label: (data?.label as string) ?? '',
          description: (data?.description as string) ?? '',
          active: false,
          reserved: false,
          impact: { backends: [], roles_affected: [], roles_blacked_out: [] },
        },
        as_of: '2026-06-29T12:00:00Z',
      }
    case 'disable-profile-delete':
      return { success: true, deleted: (data?.name as string) ?? 'eject', as_of: '2026-06-29T12:00:00Z' }
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
    case 'dream-backoff-restamp':
      // DreamBackoffRestampResponse (types.ts) — the settings curve editor
      // fires this once per completed dream.backoff_* save.
      return { success: true, action: 'dream-backoff-restamp', restamped: 5, skipped_transient: 0 }
    case 'dream-stats':
      // DreamStatsResponse (types.ts) — the back-off histogram the DreamTile
      // fetches once per moved last_cycle_at. Counts stay coherent with the
      // statusFixture dream block (17 blocks total, 12 never dreamed).
      return {
        success: true,
        action: 'dream-stats',
        total_blocks: 17,
        dream_checked: 5,
        dream_links: 3,
        coverage_pct: 29.4,
        unchecked: 12,
        pending_recheck: 1,
        backoff: {
          mode: 'exp',
          factor: 1.6,
          grace: 1,
          min_hours: 12,
          cap_hours: 1080,
          inert_offset: 2,
          max_eval_count: 3,
          truncated: false,
          levels: [
            { eval_count: 0, blocks: 12, cooldown_hours: 12 },
            { eval_count: 1, blocks: 3, cooldown_hours: 12 },
            { eval_count: 3, blocks: 2, cooldown_hours: 30.7 },
          ],
        },
      }
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

// ---------------------------------------------------------------------------
// Model API consumers (design/05 §3.2, wave W05-05): model-consumer-list with
// the server semantics of internal/handler/model_consumers_read.go — tenant
// isolation (a tenant-admin sees its own tenant, tenant_id is server-admin
// only ⇒ 403, a member ⇒ 403), the status vocabulary (absent = the four
// statuses of an active consumer, "all", unknown ⇒ 400), q = case-insensitive
// label prefix, newest first, an OPAQUE keyset cursor (base64 of created_at|id,
// the shape of encodeKeysetCursor; malformed ⇒ 400) and the page cap
// gateway.consumer_list_max_limit (100: absent = cap, above = cap, < 1 ⇒ 400).
// Each tenant carries its SENTINEL as a consumer label (leak probe, §5.6b);
// state '10k' swaps in 10 000 synthetic consumers of the session tenant.
// ---------------------------------------------------------------------------

/** The gateway policy object whoami carries (W05-01, design/05 §4.6). */
export function modelApiCapabilities(enabled: boolean, selfService = false): Record<string, unknown> {
  return {
    model_api: {
      enabled,
      self_service: selfService,
      key_max_lifetime_s: 31_536_000,
      rotation_grace_s: 86_400,
      expiry_warn_s: 1_209_600,
    },
  }
}

const CONSUMER_STATUS_VOCAB = ['active', 'expiring', 'rotating', 'no_key', 'inactive'] as const
const CONSUMER_DEFAULT_STATUSES = ['active', 'expiring', 'rotating', 'no_key']
/** gateway.consumer_list_max_limit default (internal/config, W01-5). */
const CONSUMER_LIST_MAX = 100
/** 10k scale corpus size (design 06 §6.2). */
const CONSUMER_SCALE_TOTAL = 10_000

interface MockKey {
  /** stable key id (the per-session world fixes it once, W05-06 revoke) */
  id?: string
  hint: string
  status: string
  created_at: string
  expires_at: string | null
  last_used_at: string | null
}

interface MockConsumer {
  id: string
  tenant: TenantKey
  label: string
  kind: 'app' | 'ctx-peer'
  status: (typeof CONSUMER_STATUS_VOCAB)[number]
  created_at: string
  last_used_at: string | null
  keys: MockKey[]
  keys_total: number
  description?: string
  // policy of a consumer created through the W05-06 mock (defaults otherwise)
  tenant_scope?: string | null
  all_models?: boolean
  models?: string[]
  prompt_sensitivity?: string
  allowed_cidrs?: string[] | null
  /** RC-D C5 (W05-11): absent = bulk (the column default). */
  admission_lane?: string
}

// Server-clock-skew probe (design/05 §5 B-11): the server calls this key
// `active` although its expires_at lies one minute BEFORE the page clock
// (contract FIXED_NOW 2026-06-29T12:00:05Z). The SPA must render the server's
// verdict; a client that judges expiry by its own clock would show
// expired/expiring — the /gateway primaryFlow asserts `active`.
const KEY_A9FQ: MockKey = { hint: 'ctxm_…a9Fq', status: 'active', created_at: '2026-06-20T09:00:00Z', expires_at: '2026-06-29T11:59:00Z', last_used_at: '2026-06-29T11:58:00Z' }

/** The hand-written consumer world: tenant A (acme) five rows, tenant B (globex) two. */
const CONSUMERS: MockConsumer[] = [
  // RC-D C5 (W05-07): ci-chat carries a granted model (_global chat) and a CIDR
  // allowlist, so the edit dialog has something to narrow and to widen.
  { id: '0190000000007000800000000000c0a1', tenant: 'A', label: 'ci-chat', kind: 'app', status: 'active', created_at: '2026-06-20T09:00:00Z', last_used_at: '2026-06-29T11:58:00Z', keys: [KEY_A9FQ], keys_total: 1, description: 'CI pipeline chat completions', models: ['01900000-0000-7000-8000-00000000a001'], allowed_cidrs: ['10.0.0.0/8'] },
  {
    id: '0190000000007000800000000000c0a2', tenant: 'A', label: 'ctx-peer-b', kind: 'ctx-peer', status: 'rotating', created_at: '2026-06-18T09:00:00Z', last_used_at: '2026-06-29T11:59:00Z',
    keys: [
      { hint: 'ctxm_…Qe7z', status: 'active', created_at: '2026-06-28T09:00:00Z', expires_at: '2027-06-28T09:00:00Z', last_used_at: '2026-06-29T11:59:00Z' },
      { hint: 'ctxm_…mP2c', status: 'superseded', created_at: '2026-06-18T09:00:00Z', expires_at: '2026-06-29T21:00:00Z', last_used_at: '2026-06-29T11:40:00Z' },
    ],
    keys_total: 2,
  },
  { id: '0190000000007000800000000000c0a3', tenant: 'A', label: SENTINEL.A, kind: 'app', status: 'expiring', created_at: '2026-06-15T09:00:00Z', last_used_at: null, keys: [{ hint: 'ctxm_…Xk4d', status: 'expiring', created_at: '2026-06-15T09:00:00Z', expires_at: '2026-07-05T09:00:00Z', last_used_at: null }], keys_total: 1 },
  { id: '0190000000007000800000000000c0a4', tenant: 'A', label: 'nightly-embed', kind: 'app', status: 'no_key', created_at: '2026-06-10T09:00:00Z', last_used_at: '2026-06-01T03:00:00Z', keys: [], keys_total: 1 },
  { id: '0190000000007000800000000000c0a5', tenant: 'A', label: 'legacy-app', kind: 'app', status: 'inactive', created_at: '2026-05-01T09:00:00Z', last_used_at: null, keys: [], keys_total: 1 },
  { id: '0190000000007000800000000000c0b1', tenant: 'B', label: 'globex-app', kind: 'app', status: 'active', created_at: '2026-06-22T09:00:00Z', last_used_at: null, keys: [{ hint: 'ctxm_…Gb1x', status: 'active', created_at: '2026-06-22T09:00:00Z', expires_at: null, last_used_at: null }], keys_total: 1 },
  { id: '0190000000007000800000000000c0b2', tenant: 'B', label: SENTINEL.B, kind: 'app', status: 'active', created_at: '2026-06-12T09:00:00Z', last_used_at: null, keys: [{ hint: 'ctxm_…Sb2y', status: 'active', created_at: '2026-06-12T09:00:00Z', expires_at: '2027-06-12T09:00:00Z', last_used_at: null }], keys_total: 1 },
]

/** Synthetic 10k consumer i of a tenant: newest first, the four active statuses in turn. */
function scaleConsumer(tenant: TenantKey, i: number): MockConsumer {
  const created = new Date(Date.UTC(2026, 5, 29, 11, 0, 0) - i * 60_000).toISOString()
  const status = CONSUMER_DEFAULT_STATUSES[i % CONSUMER_DEFAULT_STATUSES.length] as MockConsumer['status']
  const live = status === 'no_key' ? 0 : status === 'rotating' ? 2 : 1
  const keys: MockKey[] = []
  for (let k = 0; k < live; k++) {
    keys.push({ hint: `ctxm_…${String(i % 10_000).padStart(4, '0')}`, status: k === 0 ? (status === 'expiring' ? 'expiring' : 'active') : 'superseded', created_at: created, expires_at: null, last_used_at: null })
  }
  return {
    id: `0190000000007000800000${tenant === 'A' ? 'a' : 'b'}${String(i).padStart(9, '0')}`,
    tenant,
    label: `consumer-${String(i).padStart(5, '0')}`,
    kind: 'app',
    status,
    created_at: created,
    last_used_at: null,
    keys,
    keys_total: live,
  }
}

/**
 * The key id of key n of a consumer (stable per world, W05-06): the FULL
 * consumer id + 'k' + n. Consumer ids are hex, so 'k' never occurs in them and
 * (consumer, n) ↦ key id is injective — a key id names exactly one row. (The
 * first cut dropped the last two id characters, so …c0a1 and …c0a2 both
 * yielded …c0k0 and a revoke hit the first matching row — Copilot on #121.)
 */
function mockKeyId(c: { id: string }, n: number): string {
  return `${c.id}k${n}`
}

function consumerView(c: MockConsumer, serverAdmin: boolean): Record<string, unknown> {
  const t = TENANTS[c.tenant]
  return {
    id: c.id,
    tenant_id: t.id,
    tenant_slug: t.slug,
    label: c.label,
    description: c.description ?? '',
    kind: c.kind,
    tenant_scope: c.tenant_scope ?? null,
    all_models: c.all_models ?? false,
    models: c.models ?? [],
    prompt_sensitivity: c.prompt_sensitivity ?? 'credentials',
    fair_weight: 1,
    admission_lane: c.admission_lane ?? 'bulk',
    allowed_cidrs: c.allowed_cidrs ?? null,
    allow_plaintext: false,
    active: c.status !== 'inactive',
    status: c.status,
    last_used_at: c.last_used_at,
    created_at: c.created_at,
    created_by_label: serverAdmin ? 'smoke-key' : null,
    keys: c.keys.map((k, n) => ({
      id: k.id ?? mockKeyId(c, n),
      hint: k.hint,
      status: k.status,
      created_at: k.created_at,
      expires_at: k.expires_at,
      revoked_at: null,
      rotated_from: null,
      last_used_at: k.last_used_at,
    })),
    keys_total: c.keys_total,
    // peer binding: server-admin only, and only for a bound ctx-peer
    peer: serverAdmin && c.kind === 'ctx-peer' ? { instance_id: '0190000000007000800000000000beef', bound_at: '2026-06-18T10:00:00Z' } : null,
  }
}

// btoa/atob, not Buffer: svelte-check walks this file with the browser
// tsconfig (no node types) via routes/index.test.ts → registry.ts.
function consumerCursor(c: MockConsumer): string {
  return btoa(`${c.created_at}|${c.id}`)
}

/** model-consumer-list mock — returns the HTTP status + envelope the handler would. */
function modelConsumerListFixture(
  role: Role,
  tkey: TenantKey,
  data: Record<string, unknown> | undefined,
  state: SeedState,
  empty: boolean,
  consumers: readonly MockConsumer[] = CONSUMERS,
): { status: number; json: Record<string, unknown> } {
  const bad = (error: string) => ({ status: 400, json: { success: false, error } })
  if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
  const serverAdmin = role === 'server-admin'
  const d = data ?? {}
  // tenant scope of the read: own tenant, or (server-admin) every tenant / the named one
  let tenants: TenantKey[] = [tkey]
  if (typeof d.tenant_id === 'string' && d.tenant_id !== '') {
    if (!serverAdmin) return { status: 403, json: { success: false, error: 'tenant_id is server-admin only' } }
    tenants = (Object.keys(TENANTS) as TenantKey[]).filter((k) => TENANTS[k].id === d.tenant_id)
  } else if (serverAdmin) {
    tenants = ['A', 'B']
  }
  // status set
  let statuses = CONSUMER_DEFAULT_STATUSES
  if (d.status !== undefined) {
    if (!Array.isArray(d.status)) return bad('invalid data: status must be a list')
    const out = new Set<string>()
    for (const s of d.status) {
      if (s === 'all') CONSUMER_STATUS_VOCAB.forEach((v) => out.add(v))
      else if ((CONSUMER_STATUS_VOCAB as readonly unknown[]).includes(s)) out.add(s as string)
      else return bad('status must list values of active, expiring, rotating, no_key, inactive, or all')
    }
    if (out.size > 0) statuses = [...out]
  }
  const q = typeof d.q === 'string' ? d.q.toLowerCase() : ''
  let limit = CONSUMER_LIST_MAX
  if (d.limit !== undefined) {
    if (typeof d.limit !== 'number' || d.limit < 1) return bad('limit must be a positive integer')
    limit = Math.min(d.limit, CONSUMER_LIST_MAX)
  }
  // the world of this session
  let world: MockConsumer[]
  if (empty) world = []
  else if (state === '10k') world = tenants.flatMap((t) => Array.from({ length: CONSUMER_SCALE_TOTAL }, (_, i) => scaleConsumer(t, i)))
  else world = consumers.filter((c) => tenants.includes(c.tenant))
  const rows = world
    .filter((c) => statuses.includes(c.status) && c.label.toLowerCase().startsWith(q))
    .sort((a, b) => {
      // created_at DESC, id DESC (the handler's ORDER BY) — plain code-unit order, no locale
      if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1
      return a.id < b.id ? 1 : a.id > b.id ? -1 : 0
    })
  // keyset: resume strictly after the cursor's row
  let start = 0
  if (typeof d.cursor === 'string' && d.cursor !== '') {
    let raw = ''
    try {
      raw = atob(d.cursor)
    } catch {
      return bad('invalid cursor')
    }
    const [at, id] = raw.split('|')
    if (!at || !id || Number.isNaN(Date.parse(at))) return bad('invalid cursor')
    start = rows.findIndex((c) => c.created_at < at || (c.created_at === at && c.id < id))
    if (start < 0) start = rows.length
  }
  const pageRows = rows.slice(start, start + limit)
  const more = start + limit < rows.length
  return {
    status: 200,
    json: {
      success: true,
      consumers: pageRows.map((c) => consumerView(c, serverAdmin)),
      next_cursor: more ? consumerCursor(pageRows[pageRows.length - 1]) : null,
    },
  }
}

// ---------------------------------------------------------------------------
// Model API mutations (wave W05-06): model-consumer-create / -delete /
// -update {active:true}, model-key-revoke, model-alias-list and scope-list
// for a chosen tenant — STATEFUL per session (a create shows up in the next
// list, a revoke/deactivate changes the row), with the server semantics read
// from internal/handler/model_consumers.go (field set, validation texts,
// tier and field-tier 403s, models validated through Offers, expires_at
// against key_max_lifetime_s, label uniqueness) and model_alias_manage.go
// (a tenant-admin reads _global + its own HOME scope, a server-admin every
// scope). The create answer carries the reveal-once marker as the plaintext.
// ---------------------------------------------------------------------------

/** The plaintext model-consumer-create returns — must surface once and never persist (B-6). */
export const REVEAL_MARKER = 'ctxm_TESTKEY_reveal_once_do_not_persist'

/** The mock server's "now" — the contract page clock (FIXED_NOW, contract.ts). */
const MOCK_NOW_MS = Date.parse('2026-06-29T12:00:05Z')

/**
 * gateway.key_expiry_warn default — 1 209 600 s = 14 d (go/internal/config/
 * config.go:2660 KeyExpiryWarn): a live key expiring within this window reads
 * as `expiring` (consumer and key, model_consumers_read.go consumerStatus /
 * newKeyView).
 */
const MOCK_KEY_EXPIRY_WARN_MS = 1_209_600 * 1000

interface MockAlias {
  id: string
  scope: string
  name: string
  kind: 'chat' | 'embedding' | 'rerank'
  enabled: boolean
  // RC-D C5 (W05-13): the catalog columns; absent = the W05-06 defaults
  // (role = kind, sensitivity public, no description, empty policy)
  role?: string
  sensitivity?: string
  description?: string
  policy?: Record<string, unknown>
}

/**
 * The alias catalog: _global chat/embed/draft + a disabled legacy; tenant A's
 * home scope shadows chat (enabled) and draft (DISABLED — shadowing is by
 * presence, so neither draft is offered to scope home); tenant A's shared
 * scope shadows embed; tenant B's home scope shadows chat.
 */
export const MODEL_ALIASES: readonly MockAlias[] = [
  { id: '01900000-0000-7000-8000-00000000a001', scope: '_global', name: 'chat', kind: 'chat', enabled: true },
  { id: '01900000-0000-7000-8000-00000000a002', scope: '_global', name: 'embed', kind: 'embedding', enabled: true },
  { id: '01900000-0000-7000-8000-00000000a003', scope: '_global', name: 'draft', kind: 'chat', enabled: true },
  { id: '01900000-0000-7000-8000-00000000a004', scope: '_global', name: 'legacy', kind: 'chat', enabled: false },
  { id: '01900000-0000-7000-8000-00000000b001', scope: 'home', name: 'chat', kind: 'chat', enabled: true },
  { id: '01900000-0000-7000-8000-00000000b002', scope: 'home', name: 'draft', kind: 'chat', enabled: false },
  { id: '01900000-0000-7000-8000-00000000c001', scope: 'shared', name: 'embed', kind: 'embedding', enabled: true },
  { id: '01900000-0000-7000-8000-00000000d001', scope: 'globex:home', name: 'chat', kind: 'chat', enabled: true },
  // RC-D C5 (W05-13) — the catalog's tenant-leak sentinels: DISABLED home
  // aliases without a _global namesake, so no offer of the W05-06 picker moves.
  { id: '01900000-0000-7000-8000-00000000b0a1', scope: 'home', name: SENTINEL.A, kind: 'chat', enabled: false, sensitivity: 'credentials' },
  { id: '01900000-0000-7000-8000-00000000d0b1', scope: 'globex:home', name: SENTINEL.B, kind: 'chat', enabled: false, sensitivity: 'credentials' },
  // RC-D C5 (W05-13) — a _global floor above public for the shadow-floor gate
  // (V-02-c); disabled, so it is offered nowhere.
  { id: '01900000-0000-7000-8000-00000000a005', scope: '_global', name: 'vault', kind: 'chat', enabled: false, sensitivity: 'personal' },
]

/** modelalias.Offers over MODEL_ALIASES (registry.go: own names + unshadowed _global names, winner by presence, enabled only). */
function mockOffers(scope: string): MockAlias[] {
  const tenantScope = scope === '' || scope.startsWith('_') ? '' : scope
  const own = new Map(MODEL_ALIASES.filter((a) => tenantScope !== '' && a.scope === tenantScope).map((a) => [a.name, a]))
  const global = new Map(MODEL_ALIASES.filter((a) => a.scope === '_global').map((a) => [a.name, a]))
  const out: MockAlias[] = []
  for (const a of own.values()) if (a.enabled) out.push(a)
  for (const [name, a] of global) if (!own.has(name) && a.enabled) out.push(a)
  return out.sort((x, y) => (x.name < y.name ? -1 : x.name > y.name ? 1 : 0))
}

function aliasView(a: MockAlias, writable: boolean): Record<string, unknown> {
  return {
    id: a.id,
    scope: a.scope,
    name: a.name,
    kind: a.kind,
    role: a.role ?? a.kind,
    sensitivity: a.sensitivity ?? 'public',
    policy: a.policy ?? {},
    description: a.description ?? '',
    enabled: a.enabled,
    created_at: '2026-06-01T08:00:00Z',
    updated_at: '2026-06-01T08:00:00Z',
    writable,
  }
}

/** The fields handleModelConsumerCreate decodes (DisallowUnknownFields). */
const CREATE_FIELDS = new Set([
  'label', 'description', 'tenant_id', 'tenant_scope', 'kind', 'all_models', 'models', 'prompt_sensitivity',
  'fair_weight', 'admission_lane', 'allowed_cidrs', 'allow_plaintext', 'expires_at',
])
const SENSITIVITY_VOCAB = ['credentials', 'personal', 'internal', 'public']
const UUID_RE = /^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$/i
const CIDR_RE = /^(\d{1,3}(\.\d{1,3}){3}\/\d{1,2}|[0-9a-f:]+\/\d{1,3})$/i

type MockAnswer = { status: number; json: Record<string, unknown> }

// RC-D C5 (W05-07/W05-08/W05-13) — the further handler semantics the forms
// meet, read from model_consumers.go (update field set + field tiers,
// model-key-rotate) and model_alias_manage.go (alias writes):
/** The fields handleModelConsumerUpdate decodes besides `id` (kind / tenant_id / expires_at do not exist). */
const UPDATE_FIELDS = new Set([
  'label', 'description', 'tenant_scope', 'all_models', 'models', 'prompt_sensitivity', 'fair_weight',
  'admission_lane', 'allowed_cidrs', 'allow_plaintext', 'active',
])
/** Strictest first — "lower" = a higher index. */
const SENS_RANK: Record<string, number> = { credentials: 0, personal: 1, internal: 2, public: 3 }
/** store.MaxRotationGrace (7 d) in seconds. */
const MAX_ROTATION_GRACE_S = 604_800
/** The plaintext model-key-rotate returns — the SECOND reveal-once path (W05-08, B-6). */
export const ROTATE_MARKER = 'ctxm_TESTKEY_rotated_once_do_not_persist'
/** The alias data fields model_alias_manage.go decodes (DisallowUnknownFields). */
const ALIAS_FIELDS = new Set(['name', 'kind', 'scope', 'tenant_id', 'role', 'sensitivity', 'policy', 'description', 'enabled', 'confirm_sensitivity_downgrade'])
const ALIAS_IMMUTABLE = ['name', 'kind', 'scope', 'tenant_id']
const ALIAS_KINDS = ['chat', 'embedding', 'rerank']
/** Pool.PrimaryModel per role of the mock pool (model-alias-preview {role}, C7 contract). */
export const POOL_PRIMARY: Readonly<Record<string, string>> = { chat: 'qwen38-27b', embed: 'qwen3-embedding-8b' }
/** The previewed chain of the mock pool — one internal row, one external (locality marked). */
const PREVIEW_CHAIN = [
  { backend: 'spark-chat', locality: 'internal', provider_class: 'self_hosted', billing: 'none' },
  { backend: 'openrouter', locality: 'external', provider_class: 'openrouter', billing: 'metered' },
]

/** cidrsNarrow mirror: every prefix of `next` lies inside some prefix of `stored` (IPv4 only — the mock world). */
function mockCidrsNarrow(stored: string[] | null, next: string[] | null): boolean {
  if (next === null) return stored === null
  if (stored === null) return true
  const parse = (c: string): [number, number] | null => {
    const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)\/(\d+)$/.exec(c)
    if (!m) return null
    return [((+m[1] << 24) | (+m[2] << 16) | (+m[3] << 8) | +m[4]) >>> 0, +m[5]]
  }
  const inside = (inner: string, outer: string): boolean => {
    const a = parse(inner)
    const b = parse(outer)
    if (!a || !b || a[1] < b[1]) return false
    const mask = b[1] === 0 ? 0 : (0xffffffff << (32 - b[1])) >>> 0
    return ((a[0] & mask) >>> 0) === ((b[0] & mask) >>> 0)
  }
  return next.every((n) => stored.some((o) => inside(n, o)))
}

/** Per-session state of the W05-06 mutation mock: a mutable copy of CONSUMERS with fixed key ids. */
class GatewayWorld {
  consumers: MockConsumer[]
  /** RC-D C5 (W05-13): the alias catalog of this session — alias writes change it. */
  aliasRows: MockAlias[]
  #seq = 0

  constructor() {
    this.consumers = CONSUMERS.map((c) => ({ ...c, keys: c.keys.map((k, n) => ({ ...k, id: mockKeyId(c, n) })) }))
    this.aliasRows = MODEL_ALIASES.map((a) => ({ ...a }))
  }

  #visible(role: Role, tkey: TenantKey): MockConsumer[] {
    return role === 'server-admin' ? this.consumers : this.consumers.filter((c) => c.tenant === tkey)
  }

  /**
   * Re-derive a mutated row's statuses the way the server does at its clock
   * (model_consumers_read.go consumerStatus + youngestLive + newKeyView, here
   * at MOCK_NOW_MS with MOCK_KEY_EXPIRY_WARN_MS): live = not revoked (a revoked
   * key is already gone from `keys` — the list carries live keys only) and not
   * expired; consumer: inactive stays, 0 live ⇒ no_key, ≥ 2 ⇒ rotating, 1
   * expiring within the warn window ⇒ expiring, else active; key: superseded
   * unless it is the youngest live key, else expiring within the window, else
   * active. Seed rows that no mutation touches keep their declared statuses
   * (the B-11 probe key a9Fq is `active` by a server clock behind the page).
   */
  #restatus(c: MockConsumer): void {
    const expired = (k: MockKey) => k.expires_at !== null && Date.parse(k.expires_at) <= MOCK_NOW_MS
    const soon = (k: MockKey) => k.expires_at !== null && Date.parse(k.expires_at) <= MOCK_NOW_MS + MOCK_KEY_EXPIRY_WARN_MS
    c.keys = c.keys.filter((k) => !expired(k))
    let youngest: MockKey | undefined
    for (const k of c.keys) {
      if (youngest === undefined || k.created_at > youngest.created_at || (k.created_at === youngest.created_at && (k.id ?? '') > (youngest.id ?? ''))) youngest = k
    }
    for (const k of c.keys) k.status = k !== youngest ? 'superseded' : soon(k) ? 'expiring' : 'active'
    if (c.status === 'inactive') return
    if (c.keys.length === 0) c.status = 'no_key'
    else if (c.keys.length >= 2) c.status = 'rotating'
    else c.status = soon(c.keys[0]) ? 'expiring' : 'active'
  }

  create(role: Role, tkey: TenantKey, caps: Record<string, unknown> | undefined, data: Record<string, unknown> | undefined): MockAnswer {
    const bad = (error: string, extra: Record<string, unknown> = {}): MockAnswer => ({ status: 400, json: { success: false, error, ...extra } })
    const forbid = (error = 'admin key required'): MockAnswer => ({ status: 403, json: { success: false, error } })
    if (role === 'member') return forbid()
    const policy = (caps?.model_api ?? {}) as { enabled?: boolean; self_service?: boolean; key_max_lifetime_s?: number }
    const serverAdmin = role === 'server-admin'
    if (!serverAdmin && !(policy.enabled === true && policy.self_service === true)) return forbid()
    const d = data ?? {}
    for (const k of Object.keys(d)) if (!CREATE_FIELDS.has(k)) return bad(`invalid data: json: unknown field "${k}"`)
    const label = typeof d.label === 'string' ? d.label.trim() : ''
    if (label === '') return bad('label is required')
    if ([...label].length > 100) return bad('label must be at most 100 characters')
    if (d.kind !== undefined && d.kind !== 'app' && d.kind !== 'ctx-peer') return bad('kind must be app or ctx-peer')
    if (d.tenant_scope === '') return bad('tenant_scope must not be empty; null means _global backends only')
    const sensitivity = (d.prompt_sensitivity as string | undefined) ?? 'credentials'
    if (!SENSITIVITY_VOCAB.includes(sensitivity)) return bad('prompt_sensitivity must be one of credentials, personal, internal, public')
    let cidrs: string[] | null = null
    if (d.allowed_cidrs !== undefined && d.allowed_cidrs !== null) {
      if (!Array.isArray(d.allowed_cidrs) || d.allowed_cidrs.length === 0) return bad('allowed_cidrs must not be empty; null means any source')
      for (const c of d.allowed_cidrs) if (typeof c !== 'string' || !CIDR_RE.test(c)) return bad(`allowed_cidrs: "${String(c)}" is not a CIDR prefix`)
      cidrs = d.allowed_cidrs as string[]
    }
    // field tiers: server-admin only even with self-service
    if (!serverAdmin && (d.kind === 'ctx-peer' || sensitivity !== 'credentials')) return forbid()
    // tenant binding
    let tenant: TenantKey = tkey
    if (typeof d.tenant_id === 'string' && d.tenant_id !== '') {
      if (!serverAdmin) return forbid('tenant_id is server-admin only')
      const hit = (Object.keys(TENANTS) as TenantKey[]).find((k) => TENANTS[k].id === d.tenant_id)
      if (!hit) return { status: 404, json: { success: false, error: 'tenant not found' } }
      tenant = hit
    }
    const scope = typeof d.tenant_scope === 'string' ? d.tenant_scope : null
    if (scope !== null && !TENANTS[tenant].read.includes(scope)) return forbid('cannot create keys outside your tenant')
    // models: every id must be offered to the consumer's own scope
    const models: string[] = []
    if (d.models !== undefined && d.models !== null) {
      if (!Array.isArray(d.models)) return bad('invalid data: json: cannot unmarshal models', { param: 'models' })
      const offered = new Set(mockOffers(scope ?? '').map((a) => a.id))
      for (const m of d.models) {
        if (typeof m !== 'string' || !UUID_RE.test(m)) {
          return bad(`models: "${String(m)}" is not a model alias id — every entry must be the id of a model alias`, { param: 'models' })
        }
        if (!offered.has(m)) {
          return bad(
            `models: ${m} is not a model alias this consumer can be granted (unknown, disabled, invalid, outside its tenant_scope, or shadowed by a tenant alias of the same name)`,
            { param: 'models' },
          )
        }
        if (!models.includes(m.toLowerCase())) models.push(m.toLowerCase())
      }
      models.sort()
    }
    // expiry against key_max_lifetime_s (keyExpiry)
    const max = policy.key_max_lifetime_s ?? 31_536_000
    let expires: string | null = max > 0 ? new Date(MOCK_NOW_MS + max * 1000).toISOString() : null
    if (d.expires_at !== undefined && d.expires_at !== null) {
      const at = typeof d.expires_at === 'string' ? Date.parse(d.expires_at) : NaN
      if (Number.isNaN(at)) return bad('invalid data: parsing time')
      if (at <= MOCK_NOW_MS) return bad('expires_at must lie in the future')
      if (max > 0 && at > MOCK_NOW_MS + max * 1000) return bad(`expires_at lies beyond gateway.key_max_lifetime_app (${max} s from now)`)
      expires = new Date(at).toISOString()
    }
    if (this.consumers.some((c) => c.tenant === tenant && c.label === label)) {
      return { status: 409, json: { success: false, error: 'a model consumer with this label already exists in the tenant' } }
    }
    this.#seq++
    const created = new Date(MOCK_NOW_MS + this.#seq * 1000).toISOString().replace('.000Z', 'Z')
    const id = `0190000000007000800000000000e${String(this.#seq).padStart(3, '0')}`
    const c: MockConsumer = {
      id,
      tenant,
      label,
      kind: 'app',
      status: 'active',
      created_at: created,
      last_used_at: null,
      keys: [{ id: mockKeyId({ id }, 0), hint: `ctxm_…${REVEAL_MARKER.slice(-4)}`, status: 'active', created_at: created, expires_at: expires, last_used_at: null }],
      keys_total: 1,
      description: typeof d.description === 'string' ? d.description : '',
      tenant_scope: scope,
      all_models: d.all_models === true,
      models,
      prompt_sensitivity: sensitivity,
      allowed_cidrs: cidrs,
    }
    this.#restatus(c)
    this.consumers.unshift(c)
    return { status: 200, json: { success: true, consumer: consumerView(c, serverAdmin), api_key: REVEAL_MARKER } }
  }

  deactivate(role: Role, tkey: TenantKey, data: Record<string, unknown> | undefined, cancelled: number | null): MockAnswer {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const id = typeof data?.id === 'string' ? data.id : ''
    if (id === '') return { status: 400, json: { success: false, error: 'id is required' } }
    const c = this.#visible(role, tkey).find((x) => x.id === id)
    if (!c) return { status: 404, json: { success: false, error: 'model consumer not found' } }
    c.status = 'inactive'
    c.keys = []
    return { status: 200, json: { success: true, cancelled_inflight: cancelled } }
  }

  revoke(role: Role, tkey: TenantKey, data: Record<string, unknown> | undefined, cancelled: number | null): MockAnswer {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const keyId = typeof data?.key_id === 'string' ? data.key_id : ''
    if (keyId === '') return { status: 400, json: { success: false, error: 'key_id is required' } }
    const c = this.#visible(role, tkey).find((x) => x.keys.some((k) => k.id === keyId))
    if (!c) return { status: 404, json: { success: false, error: 'model key not found' } }
    c.keys = c.keys.filter((k) => k.id !== keyId)
    this.#restatus(c)
    return { status: 200, json: { success: true, cancelled_inflight: cancelled } }
  }

  update(role: Role, tkey: TenantKey, caps: Record<string, unknown> | undefined, data: Record<string, unknown> | undefined): MockAnswer {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const d = data ?? {}
    for (const k of Object.keys(d)) if (k !== 'id' && !UPDATE_FIELDS.has(k)) return { status: 400, json: { success: false, error: `unknown field "${k}" (expiry changes need model-key-rotate)` } }
    const id = typeof d.id === 'string' ? d.id : ''
    if (id === '') return { status: 400, json: { success: false, error: 'id is required' } }
    const c = this.#visible(role, tkey).find((x) => x.id === id)
    if (!c) return { status: 404, json: { success: false, error: 'model consumer not found' } }
    // RC-D C5 (W05-07/W05-11): the policy fields with the handler's field tiers
    // — server-admin only: lower the sensitivity, raise the lane bulk → urgent;
    // granting (tenant-admin only with self-service, never on a ctx-peer):
    // widen models / all_models / set or change tenant_scope / widen the CIDR
    // allowlist (null over a list, prefixes outside the stored list).
    const policyKeys = Object.keys(d).filter((k) => k !== 'id' && k !== 'active')
    if (policyKeys.length > 0) {
      const serverAdmin = role === 'server-admin'
      const pol = (caps?.model_api ?? {}) as { enabled?: boolean; self_service?: boolean }
      const mayGrant = serverAdmin || (c.kind !== 'ctx-peer' && pol.enabled === true && pol.self_service === true)
      const forbid: MockAnswer = { status: 403, json: { success: false, error: 'admin key required' } }
      if (d.admission_lane !== undefined && d.admission_lane !== 'urgent' && d.admission_lane !== 'bulk') {
        return { status: 400, json: { success: false, error: 'admission_lane must be urgent or bulk' } }
      }
      if (d.prompt_sensitivity !== undefined && !SENSITIVITY_VOCAB.includes(d.prompt_sensitivity as string)) {
        return { status: 400, json: { success: false, error: 'prompt_sensitivity must be one of credentials, personal, internal, public' } }
      }
      if (d.allowed_cidrs !== undefined && d.allowed_cidrs !== null) {
        if (!Array.isArray(d.allowed_cidrs) || d.allowed_cidrs.length === 0) return { status: 400, json: { success: false, error: 'allowed_cidrs must not be empty; null means any source' } }
        for (const x of d.allowed_cidrs) if (typeof x !== 'string' || !CIDR_RE.test(x)) return { status: 400, json: { success: false, error: `allowed_cidrs: "${String(x)}" is not a CIDR prefix` } }
      }
      const curSens = c.prompt_sensitivity ?? 'credentials'
      const lowersSens = typeof d.prompt_sensitivity === 'string' && SENS_RANK[d.prompt_sensitivity] > SENS_RANK[curSens]
      const curLane = c.admission_lane ?? 'bulk'
      const raisesLane = d.admission_lane === 'urgent' && curLane === 'bulk'
      if (!serverAdmin && (lowersSens || raisesLane)) return forbid
      const curModels = c.models ?? []
      const widensModels = Array.isArray(d.models) && (d.models as string[]).some((m) => !curModels.includes(String(m).toLowerCase()))
      const widensAll = d.all_models === true && c.all_models !== true
      const changesScope = typeof d.tenant_scope === 'string' && d.tenant_scope !== (c.tenant_scope ?? null)
      const widensCidrs = 'allowed_cidrs' in d && !mockCidrsNarrow(c.allowed_cidrs ?? null, (d.allowed_cidrs as string[] | null) ?? null)
      if (!mayGrant && (widensModels || widensAll || changesScope || widensCidrs)) return forbid
      if (typeof d.label === 'string') c.label = d.label.trim()
      if (typeof d.description === 'string') c.description = d.description
      if ('tenant_scope' in d) c.tenant_scope = (d.tenant_scope as string | null) ?? null
      if (typeof d.all_models === 'boolean') c.all_models = d.all_models
      if (Array.isArray(d.models)) c.models = (d.models as string[]).map((m) => m.toLowerCase()).sort()
      if (typeof d.prompt_sensitivity === 'string') c.prompt_sensitivity = d.prompt_sensitivity
      if (typeof d.admission_lane === 'string') c.admission_lane = d.admission_lane
      if ('allowed_cidrs' in d) c.allowed_cidrs = (d.allowed_cidrs as string[] | null) ?? null
    }
    if (d.active === true && c.status === 'inactive') {
      const policy = (caps?.model_api ?? {}) as { enabled?: boolean; self_service?: boolean }
      // reactivating is granting: tenant-admin only with self-service, never on a peer
      if (role !== 'server-admin' && (c.kind === 'ctx-peer' || !(policy.enabled === true && policy.self_service === true))) {
        return { status: 403, json: { success: false, error: 'admin key required' } }
      }
      c.status = 'no_key' // reactivating brings back no key (I-K8)
    } else if (d.active === false) {
      c.status = 'inactive'
      c.keys = []
    }
    return { status: 200, json: { success: true, consumer: consumerView(c, role === 'server-admin') } }
  }

  /**
   * RC-D C5 (W05-08) — model-key-rotate {consumer_id, grace (s), expires_at?}:
   * granting (tenant-admin only with self-service, never on a ctx-peer); grace
   * 0 revokes the predecessor at once, > 0 keeps it live (superseded) until
   * the window ends; answers the new key's view + its plaintext ONCE.
   */
  rotate(role: Role, tkey: TenantKey, caps: Record<string, unknown> | undefined, data: Record<string, unknown> | undefined): MockAnswer {
    const forbid: MockAnswer = { status: 403, json: { success: false, error: 'admin key required' } }
    if (role === 'member') return forbid
    const d = data ?? {}
    for (const k of Object.keys(d)) if (k !== 'consumer_id' && k !== 'grace' && k !== 'expires_at') return { status: 400, json: { success: false, error: `invalid data: json: unknown field "${k}"` } }
    const id = typeof d.consumer_id === 'string' ? d.consumer_id : ''
    if (id === '') return { status: 400, json: { success: false, error: 'consumer_id is required' } }
    const grace = d.grace === undefined ? 86_400 : d.grace
    if (typeof grace !== 'number' || !Number.isInteger(grace) || grace < 0 || grace > MAX_ROTATION_GRACE_S) {
      return { status: 400, json: { success: false, error: `grace must lie between 0 and ${MAX_ROTATION_GRACE_S} seconds` } }
    }
    const c = this.#visible(role, tkey).find((x) => x.id === id)
    if (!c) return { status: 404, json: { success: false, error: 'model consumer not found' } }
    const pol = (caps?.model_api ?? {}) as { enabled?: boolean; self_service?: boolean }
    if (role !== 'server-admin' && (c.kind === 'ctx-peer' || !(pol.enabled === true && pol.self_service === true))) return forbid
    if (c.status === 'inactive') return { status: 409, json: { success: false, error: 'model consumer is not active' } }
    this.#seq++
    const created = new Date(MOCK_NOW_MS + this.#seq * 1000).toISOString().replace('.000Z', 'Z')
    if (grace === 0) c.keys = []
    else c.keys = c.keys.map((k) => ({ ...k, expires_at: new Date(Math.min(MOCK_NOW_MS + grace * 1000, k.expires_at ? Date.parse(k.expires_at) : Infinity)).toISOString() }))
    const key: MockKey = { id: `${c.id}k${c.keys_total}`, hint: `ctxm_…${ROTATE_MARKER.slice(-4)}`, status: 'active', created_at: created, expires_at: new Date(MOCK_NOW_MS + 31_536_000_000).toISOString(), last_used_at: null }
    c.keys = [key, ...c.keys]
    c.keys_total++
    this.#restatus(c)
    const view = (consumerView(c, role === 'server-admin').keys as Record<string, unknown>[]).find((k) => k.id === key.id)
    return { status: 200, json: { success: true, key: view ?? {}, api_key: ROTATE_MARKER } }
  }

  #aliasScopeFor(role: Role, tkey: TenantKey, scope: unknown): string | null {
    const home = TENANTS[tkey].home
    if (role === 'server-admin') return typeof scope === 'string' && scope !== '' ? scope : '_global'
    if (scope === undefined || scope === '' || scope === home) return home
    return null
  }

  /** RC-D C5 (W05-13) — model-alias-create with the handler's checks (MA:323-635). */
  aliasCreate(role: Role, tkey: TenantKey, data: Record<string, unknown> | undefined): MockAnswer {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const d = data ?? {}
    for (const k of Object.keys(d)) if (!ALIAS_FIELDS.has(k)) return { status: 400, json: { success: false, error: `invalid data: unknown field "${k}"` } }
    const fields: { field: string; message: string }[] = []
    for (const k of ['role', 'sensitivity', 'description', 'enabled']) if (k in d && d[k] === null) fields.push({ field: k, message: 'null is no value — omit the key instead' })
    if (typeof d.name !== 'string' || d.name === '') fields.push({ field: 'name', message: 'required' })
    if (typeof d.kind !== 'string' || !ALIAS_KINDS.includes(d.kind)) fields.push({ field: 'kind', message: 'must be chat, embedding or rerank' })
    if (typeof d.role !== 'string' || d.role === '') fields.push({ field: 'role', message: 'required' })
    if (typeof d.enabled !== 'boolean') fields.push({ field: 'enabled', message: 'required — send true or false; a new alias is never live by default' })
    const sens = (d.sensitivity as string | undefined) ?? 'credentials'
    if (!(sens in SENS_RANK)) fields.push({ field: 'sensitivity', message: 'must be public, internal, personal or credentials' })
    if (fields.length > 0) return { status: 422, json: { success: false, error: 'validation failed', fields } }
    if (role !== 'server-admin' && typeof d.tenant_id === 'string') return { status: 403, json: { success: false, error: 'tenant_id is server-admin only' } }
    const scope = this.#aliasScopeFor(role, tkey, d.scope)
    if (scope === null) return { status: 404, json: { success: false, error: 'model alias not found' } }
    const name = d.name as string
    if (scope !== '_global') {
      const floor = this.aliasRows.find((a) => a.scope === '_global' && a.name === name)
      const fs = floor?.sensitivity ?? 'public'
      if (floor && SENS_RANK[sens] > SENS_RANK[fs]) {
        return { status: 422, json: { success: false, error: 'validation failed', fields: [{ field: 'sensitivity', message: `${sens} is below ${fs}, the sensitivity of the _global alias "${name}" this alias shadows — a shadow may redirect, never lower the floor` }] } }
      }
    }
    const warnings: string[] = []
    if (SENS_RANK[sens] > SENS_RANK.credentials) {
      if (role !== 'server-admin') return { status: 403, json: { success: false, error: 'admin key required' } }
      if (d.confirm_sensitivity_downgrade !== true) return { status: 422, json: { success: false, error: `validation: sensitivity: lowering credentials → ${sens} … repeat with "confirm_sensitivity_downgrade": true` } }
      warnings.push(`sensitivity lowered credentials → ${sens} (confirmed)`)
    }
    if (this.aliasRows.some((a) => a.scope === scope && a.name === name)) {
      return { status: 409, json: { success: false, error: 'a model alias of that name already exists in the scope', code: 'constraint' } }
    }
    this.#seq++
    const a: MockAlias = {
      id: `01900000-0000-7000-8000-0000000e${String(this.#seq).padStart(4, '0')}`,
      scope,
      name,
      kind: d.kind as MockAlias['kind'],
      enabled: d.enabled as boolean,
      role: d.role as string,
      sensitivity: sens,
      description: typeof d.description === 'string' ? d.description : '',
      policy: (d.policy as Record<string, unknown> | undefined) ?? {},
    }
    this.aliasRows.push(a)
    return { status: 200, json: { success: true, model_alias: aliasView(a, true), ...(warnings.length > 0 ? { warnings } : {}) } }
  }

  /** RC-D C5 (W05-13) — model-alias-update {id top-level, data}: immutables 400, _global 404 below server-admin. */
  aliasUpdate(role: Role, tkey: TenantKey, id: string | undefined, data: Record<string, unknown> | undefined): MockAnswer {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const d = data ?? {}
    if (typeof id !== 'string' || id === '') return { status: 400, json: { success: false, error: 'id required' } }
    for (const f of ALIAS_IMMUTABLE) {
      if (f in d) return { status: 400, json: { success: false, error: `${f} is immutable — renaming or re-scoping an alias is a new alias: create it and disable this one`, param: f, code: 'immutable_field' } }
    }
    for (const k of Object.keys(d)) if (!ALIAS_FIELDS.has(k)) return { status: 400, json: { success: false, error: `invalid data: unknown field "${k}"` } }
    const home = TENANTS[tkey].home
    const a = this.aliasRows.find((x) => x.id === id && (role === 'server-admin' || x.scope === home))
    if (!a) return { status: 404, json: { success: false, error: 'model alias not found' } }
    const changes = ['role', 'sensitivity', 'policy', 'description', 'enabled'].filter((k) => k in d)
    if (changes.length === 0) return { status: 400, json: { success: false, error: 'nothing to update' } }
    const cur = a.sensitivity ?? 'public'
    const next = (d.sensitivity as string | undefined) ?? cur
    const warnings: string[] = []
    if (SENS_RANK[next] > SENS_RANK[cur]) {
      if (role !== 'server-admin') return { status: 403, json: { success: false, error: 'admin key required' } }
      if (d.confirm_sensitivity_downgrade !== true) return { status: 422, json: { success: false, error: `validation: sensitivity: lowering ${cur} → ${next} … repeat with "confirm_sensitivity_downgrade": true` } }
      warnings.push(`sensitivity lowered ${cur} → ${next} (confirmed)`)
    }
    if (typeof d.role === 'string') a.role = d.role
    if (typeof d.sensitivity === 'string') a.sensitivity = d.sensitivity
    if (typeof d.description === 'string') a.description = d.description
    if (typeof d.enabled === 'boolean') a.enabled = d.enabled
    if (d.policy !== undefined) a.policy = d.policy as Record<string, unknown>
    return { status: 200, json: { success: true, model_alias: aliasView(a, true), ...(warnings.length > 0 ? { warnings } : {}) } }
  }

  /** RC-D C5 — model-alias-preview (C7 contract, mocked): server-admin only; role mode ⇒ primary_model. */
  aliasPreview(role: Role, data: Record<string, unknown> | undefined): MockAnswer {
    if (role !== 'server-admin') return { status: 403, json: { success: false, error: 'admin key required' } }
    const d = data ?? {}
    if (typeof d.role === 'string') {
      return { status: 200, json: { success: true, alias: null, scope: null, required_sensitivity: 'credentials', chain: [], omitted: [], primary_model: POOL_PRIMARY[d.role] ?? null } }
    }
    const a = this.aliasRows.find((x) => x.id === d.alias)
    if (!a) return { status: 404, json: { success: false, error: 'model alias not found' } }
    const sens = a.sensitivity ?? 'public'
    const chain = SENS_RANK[sens] <= SENS_RANK.personal ? PREVIEW_CHAIN.filter((r) => r.locality === 'internal') : PREVIEW_CHAIN
    const omitted = chain.length < PREVIEW_CHAIN.length ? [{ backend: 'openrouter', reason: `sensitivity ${sens} needs trust full` }] : []
    return { status: 200, json: { success: true, alias: a.id, scope: a.scope === '_global' ? null : a.scope, required_sensitivity: sens, chain, omitted, primary_model: null } }
  }

  aliases(role: Role, tkey: TenantKey): MockAnswer {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const home = TENANTS[tkey].home
    const rows =
      role === 'server-admin'
        ? this.aliasRows
        : this.aliasRows.filter((a) => a.scope === '_global' || (home !== '' && !home.startsWith('_') && a.scope === home))
    const sorted = [...rows].sort((a, b) => (a.scope + '\u0000' + a.name < b.scope + '\u0000' + b.name ? -1 : 1))
    return {
      status: 200,
      json: { success: true, model_aliases: sorted.map((a) => aliasView(a, role === 'server-admin' || a.scope === home)), count: sorted.length },
    }
  }
}

/** scope-list for a server-admin's chosen tenant (top-level id, handleScopeList); null = not this branch. */
function scopeListForTenant(role: Role, id: string | undefined): MockAnswer | null {
  if (role !== 'server-admin' || typeof id !== 'string' || id === '') return null
  const hit = (Object.keys(TENANTS) as TenantKey[]).find((k) => TENANTS[k].id === id)
  const rows = hit ? TENANTS[hit].read.map((scope) => ({ scope, block_count: 0, key_count: 0, tenant_id: TENANTS[hit].id })) : []
  return { status: 200, json: { success: true, scopes: rows } }
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
  /** Capability bag threaded into whoamiFor (S14 — Achse-04 feature gates; since
   * W05-05 also the model_api policy object, modelApiCapabilities()). */
  capabilities?: Record<string, unknown>
  /**
   * What model-key-revoke / model-consumer-delete answer as cancelled_inflight
   * (W05-06 toast, V-01-e). Default null — the server's word until W01-8.
   */
  cancelledInflight?: number | null
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
 * Seed an authenticated session (cookie-session shape since R4: the whoami
 * mock answers the header-less probe with the session envelope + csrf_token;
 * `anonymous` seeds boot to the login mask instead) + a deterministic theme
 * pref (localStorage, read by theme-boot.js before first paint), then
 * install the `/auth/*` + `/api/**` mocks. Must run BEFORE page.goto().
 * The raw key touches NO client storage anymore (R4 — sessionStorage seed
 * removed); the login contract runs through the mocked POST /auth/login.
 * Returns the mock-call recorder handle (every /api/** request is logged) —
 * existing call sites may ignore it (non-breaking).
 */
export async function seedSession(page: Page, opts: SeedOptions): Promise<SeededSession> {
  await page.addInitScript(
    ({ theme }) => {
      try {
        localStorage.setItem('ctx.theme', theme)
      } catch {
        /* storage blocked — test will surface it as a render failure */
      }
    },
    { theme: opts.theme },
  )

  // Cookie-session state (R4): logged-in seeds answer the header-less whoami
  // with the session envelope; the login/logout mocks flip this flag so the
  // PV7 login contract and teardown flows stay provable.
  let cookieAuthed = opts.anonymous !== true
  /** The per-session csrf secret whoami + login hand to the SPA (05 §4.4). */
  const CSRF = 'e2e-csrf-secret'

  // Session lifecycle routes (auth_session.go). Login authenticates ONLY the
  // canonical fixture key — a wrong key gets the real handler's uniform 401
  // envelope (sessionError: "authentication failed"). Refresh answers 401
  // unless the seed is authenticated, so the api layer's single-flight
  // refresh path terminates deterministically (no hanging request).
  await page.route('**/auth/login', async (route: Route) => {
    let apiKey = ''
    try {
      apiKey = (route.request().postDataJSON() as { api_key?: string })?.api_key ?? ''
    } catch {
      /* malformed body → stays '' → 401 */
    }
    if (apiKey.trim() === KEY) {
      cookieAuthed = true
      return route.fulfill({ json: { success: true, csrf_token: CSRF } })
    }
    return route.fulfill({ status: 401, json: { success: false, error: 'authentication failed' } })
  })
  await page.route('**/auth/refresh', (route: Route) =>
    cookieAuthed
      ? route.fulfill({ json: { success: true } })
      : route.fulfill({ status: 401, json: { success: false, error: 'authentication failed' } }),
  )
  await page.route('**/auth/logout', (route: Route) => {
    cookieAuthed = false
    return route.fulfill({ json: { success: true } })
  })

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
  // W05-06: the stateful model consumer world of this session.
  const gateway = new GatewayWorld()
  const cancelled = opts.cancelledInflight === undefined ? null : opts.cancelledInflight

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
      // Cookie-session branch (R4): the SPA probes whoami header-less — the
      // ambient cookie is invisible to the mock, so the seed flag decides.
      // Authenticated seeds get the session envelope INCLUDING csrf_token
      // (the 05 §4.4 synchronizer delivery); anonymous seeds get the 401 and
      // boot to the login mask. An EXPLICIT Authorization header (legacy
      // probe shape, opts.key overrides) still authenticates only the
      // canonical KEY — the PV7 error-band contract now lives on the
      // /auth/login mock above.
      const auth = req.headers()['authorization']
      if (auth !== undefined && auth !== `Bearer ${KEY}`) {
        return route.fulfill({ status: 401, json: { success: false, error: 'invalid or revoked API key' } })
      }
      if (auth === undefined && !cookieAuthed) {
        return route.fulfill({ status: 401, json: { error: 'unauthorized' } })
      }
      return route.fulfill({ json: { ...whoamiFor(opts.role, tkey, opts.capabilities), csrf_token: CSRF } })
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
      return route.fulfill({ status: 500, json: { success: false, error: 'Internal server error' } })
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
    // Category-hue overrides (AM-2, U02-W6). GET = resolved sparse map (default
    // EMPTY — every category renders on its hash seed; graph specs stay clean).
    // PUT/DELETE echo the handler envelope; the server derives the scope from
    // auth (never the body, §A5-MT), so the fixture returns the caller's home.
    // Specs that need populated overrides register their OWN page.route AFTER
    // seedSession (later registration wins) — e.g. the graph-render probe.
    if (path.startsWith('/api/graph/category-hues')) {
      const prefix = '/api/graph/category-hues/'
      if (method === 'PUT') {
        const category = decodeURIComponent(path.slice(prefix.length))
        let hue = 0
        try {
          hue = (req.postDataJSON() as { hue?: number } | null)?.hue ?? 0
        } catch {
          /* non-JSON body — hue stays 0 */
        }
        return route.fulfill({ json: { success: true, category, hue, scope: ctx.home } })
      }
      if (method === 'DELETE') {
        const category = decodeURIComponent(path.slice(prefix.length))
        return route.fulfill({ json: { success: true, category, deleted: true, scope: ctx.home } })
      }
      return route.fulfill({ json: { success: true, hues: {} } })
    }
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
      // Model API consumer list (W05-05): role/state-aware, so it is served
      // here rather than in the role-agnostic manageFixture layer.
      if (action === 'model-consumer-list') {
        const r = modelConsumerListFixture(opts.role, tkey, data, state, empty, gateway.consumers)
        return route.fulfill({ status: r.status, json: r.json })
      }
      // Model API mutations + picker reads (W05-06), stateful per session.
      {
        let r: MockAnswer | null = null
        if (action === 'model-consumer-create') r = gateway.create(opts.role, tkey, opts.capabilities, data)
        else if (action === 'model-consumer-delete') r = gateway.deactivate(opts.role, tkey, data, cancelled)
        else if (action === 'model-consumer-update') r = gateway.update(opts.role, tkey, opts.capabilities, data)
        else if (action === 'model-key-revoke') r = gateway.revoke(opts.role, tkey, data, cancelled)
        else if (action === 'model-alias-list') r = gateway.aliases(opts.role, tkey)
        // RC-D C5: rotation (W05-08), alias writes + preview (W05-13)
        else if (action === 'model-key-rotate') r = gateway.rotate(opts.role, tkey, opts.capabilities, data)
        else if (action === 'model-alias-create') r = gateway.aliasCreate(opts.role, tkey, data)
        else if (action === 'model-alias-update') r = gateway.aliasUpdate(opts.role, tkey, id, data)
        else if (action === 'model-alias-preview') r = gateway.aliasPreview(opts.role, data)
        else if (action === 'scope-list') r = scopeListForTenant(opts.role, id)
        if (r !== null) return route.fulfill({ status: r.status, json: r.json })
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
      // tenant-grant-delete carries the grant id top-level (like tenant-delete),
      // so it is echoed here rather than in the data-only manageFixture layer.
      if (action === 'tenant-grant-delete' && typeof id === 'string') {
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
