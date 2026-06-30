// Playwright smoke-test fixtures (web-redesign visual verification debt, HANDOVER §8).
//
// The SPA is fully driven by `/api/**` calls; this module mocks every endpoint a
// page touches on initial load with deterministic, scope-/role-shaped payloads so
// the SHELL + per-area LAYOUT MODES (S3–S7), THEME (TH3/TH4) and GRAPH (G1–G3)
// render real content — no live backend, no real key. Wire shapes mirror
// src/lib/api/types.ts + src/lib/graph/api.ts + src/lib/api/blocks.ts verbatim
// (the Go golden tests are the drift anchor; these fixtures are NOT a second one).

import type { Page, Route } from '@playwright/test'

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

const KEY = 'smoke-key'

/**
 * WhoamiResponse (types.ts:6) shaped per tier — capabilitiesFor reads admin+role.
 * The optional `tenant` selects the identity (default 'A'); omitting it preserves
 * the exact pre-existing default-tenant shape so the 34 legacy specs do not break.
 */
export function whoamiFor(role: Role, tenant: TenantKey = 'A'): Record<string, unknown> {
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

/** SearchResponse (graph/api.ts) — NO success field on the happy path. */
function searchFixture(): Record<string, unknown> {
  const mk = (n: number, cat: string, title: string, sens: string) => ({
    id: `550e8400-e29b-41d4-a716-44665544000${n}`,
    category: cat,
    tags: ['demo', cat],
    title,
    content_preview: `Lorem ipsum preview for ${title} — enough text to fill a master-detail hit row in the split layout.`,
    content_length: 1840,
    scope: 'home',
    updated_at: '2026-06-28T10:00:00Z',
    created_at: '2026-06-01T08:00:00Z',
    sensitivity: sens,
  })
  return {
    count: 3,
    results: [
      mk(1, 'design', 'Core Architecture', 'internal'),
      mk(2, 'reference', 'API Spec', 'public'),
      mk(3, 'learnings', 'Retrieval Findings', 'internal'),
    ],
    next_after: null,
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
    case 'list-meta':
      return { success: true, blocks: [
        { id: '550e8400-e29b-41d4-a716-446655440001', category: 'design', title: 'Core Architecture', tags: ['demo'], scope: 'home', updated_at: '2026-06-28T10:00:00Z' },
        { id: '550e8400-e29b-41d4-a716-446655440002', category: 'reference', title: 'API Spec', tags: ['demo'], scope: 'home', updated_at: '2026-06-27T10:00:00Z' },
      ] }
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

/**
 * Seed an authenticated session (sessionStorage key, restored on App mount) +
 * a deterministic theme pref (localStorage, read by theme-boot.js before first
 * paint), then install the `/api/**` mocks. Must run BEFORE page.goto().
 */
export async function seedSession(
  page: Page,
  opts: {
    role: Role
    theme: 'light' | 'dark'
    empty?: boolean
    /** identity tenant (default 'A' = legacy default-tenant shape). */
    tenant?: TenantKey
    /** negative-probe injection — faults a specific manage action (§2.2). */
    faults?: Fault[]
    /** scope-list / scope-overview override (fresh tenant = []). */
    scopes?: Record<string, unknown>[]
  },
): Promise<void> {
  await page.addInitScript(
    ({ key, theme }) => {
      try {
        sessionStorage.setItem('ctx.api-key', key)
        localStorage.setItem('ctx.theme', theme)
      } catch {
        /* storage blocked — test will surface it as a render failure */
      }
    },
    { key: KEY, theme: opts.theme },
  )

  const tkey: TenantKey = opts.tenant ?? 'A'
  const t = TENANTS[tkey]
  const ctx: FixtureCtx = { id: t.id, slug: t.slug, name: t.name, home: t.home, read: t.read, scopes: opts.scopes }
  // Per-session, per-action call counter — drives Fault.afterCalls (quota-exhaustion
  // sequences: e.g. first scope-create ok, second → 429).
  const callIndex: Record<string, number> = {}

  await page.route('**/health', (route: Route) =>
    route.fulfill({ json: { status: 'ok', services: { db: 'ok', embed: 'ok' } } }),
  )

  await page.route('**/api/**', async (route: Route) => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    const method = req.method()

    // SSE telemetry stream → abort so StatusPage uses the GET /api/status poll.
    if (path === '/api/events') return route.abort()

    if (path === '/api/whoami') return route.fulfill({ json: whoamiFor(opts.role, tkey) })
    if (path === '/api/status') return route.fulfill({ json: statusFixture() })
    if (path === '/api/llmlog') return route.fulfill({ json: { success: true, entries: [] } })
    if (path === '/api/settings' && method === 'GET') return route.fulfill({ json: { success: true, settings: settingsFixture() } })
    if (path === '/api/secrets' && method === 'GET') return route.fulfill({ json: { success: true, secrets: [] } })
    if (path.startsWith('/api/graph/overview')) return route.fulfill({ json: opts.empty ? emptyOverviewFixture() : overviewFixture() })
    if (path.startsWith('/api/graph/ego')) return route.fulfill({ json: egoFixture() })
    if (path.startsWith('/api/chat/sessions')) return route.fulfill({ json: { success: true, sessions: [] } })
    if (path === '/api/search') return route.fulfill({ json: opts.empty ? emptySearchFixture() : searchFixture() })
    if (path === '/api/manage' && method === 'POST') {
      let action: string | undefined
      let data: Record<string, unknown> | undefined
      try {
        const body = req.postDataJSON() as { action?: string; data?: Record<string, unknown> } | null
        action = body?.action
        data = body?.data
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
      return route.fulfill({ json: manageFixture(action, opts.role, ctx, data) })
    }

    // Unmapped (write paths not hit on initial load): benign success envelope.
    return route.fulfill({ json: { success: true } })
  })
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
