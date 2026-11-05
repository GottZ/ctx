// Pure create-dialog logic of the /gateway area (design/05 §4.6 component
// ConsumerCreateDialog, §4.5 field-tier UX, wave W05-06):
//   G4 — the offer picker mirrors modelalias.Offers (internal/modelalias/
//        registry.go Offers + winner): the aliases visible to the consumer's
//        scope, a tenant alias in place of the _global alias of the same name
//        — shadowing by PRESENCE, enabled or not — and of those only the
//        enabled ones; no scope ⇒ _global only; the spec sends alias IDS.
//   Lead decision 1 — the scope options: a tenant-admin is offered only the
//        scopes whose aliases model-alias-list hands it (its home scope), a
//        server-admin every scope of the chosen tenant.
//   G3 — the spec carries no `kind` (structurally: the builder has no field).
// Plus expiry options against key_max_lifetime_s, the CIDR form check, the
// sensitivity tier mirror, the deactivate/revoke typed tail and the
// cancelled_inflight toast text.

import { describe, expect, it } from 'vitest'
import type { ConsumerView, ModelAliasView } from '../../lib/api/types'
import { canConfirm } from '../../lib/components/ConfirmDialog.svelte'
import { canReactivate, deactivateTyped, hintTail } from './rows'
import {
  buildCreateSpec,
  expiryOptions,
  expirySpec,
  offeredAliases,
  parseCidrs,
  scopeOptions,
  sensitivityOptions,
  templateModels,
  TEMPLATES,
  type MintFormInput,
} from './mint'
import { cancelledText } from './format'

function alias(id: string, scope: string, name: string, kind: string, enabled = true): ModelAliasView {
  return {
    id,
    scope,
    name,
    kind,
    role: 'chat',
    sensitivity: 'public',
    policy: {},
    description: '',
    enabled,
    created_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-01T00:00:00Z',
    writable: false,
  }
}

// The world the e2e mock serves too: _global chat/embed/draft/legacy(disabled),
// scope s1 chat (shadows) + draft (disabled, shadows by presence), scope s2 embed.
const G_CHAT = alias('g-chat', '_global', 'chat', 'chat')
const G_EMBED = alias('g-embed', '_global', 'embed', 'embedding')
const G_DRAFT = alias('g-draft', '_global', 'draft', 'chat')
const G_LEGACY = alias('g-legacy', '_global', 'legacy', 'chat', false)
const S1_CHAT = alias('s1-chat', 's1', 'chat', 'chat')
const S1_DRAFT = alias('s1-draft', 's1', 'draft', 'chat', false)
const S2_EMBED = alias('s2-embed', 's2', 'embed', 'embedding')
const WORLD = [G_CHAT, G_DRAFT, G_EMBED, G_LEGACY, S1_CHAT, S1_DRAFT, S2_EMBED]

const ids = (xs: ModelAliasView[]) => xs.map((a) => a.id)

describe('offer picker mirrors modelalias.Offers (G4, V-0102-h)', () => {
  it('no scope ⇒ only the enabled _global aliases, sorted by name', () => {
    expect(ids(offeredAliases(WORLD, null))).toEqual(['g-chat', 'g-draft', 'g-embed'])
  })

  it('scope s1 ⇒ the tenant chat in place of the _global chat; a disabled tenant alias shadows and is not offered', () => {
    expect(ids(offeredAliases(WORLD, 's1'))).toEqual(['s1-chat', 'g-embed'])
  })

  it('scope s2 ⇒ its own embed in place of the _global embed; another scope never leaks in', () => {
    expect(ids(offeredAliases(WORLD, 's2'))).toEqual(['g-chat', 'g-draft', 's2-embed'])
  })

  it('a disabled alias is never offered; an empty or reserved scope sees _global only', () => {
    for (const scope of [null, '', '_global', '_system']) {
      const got = ids(offeredAliases(WORLD, scope))
      expect(got).not.toContain('g-legacy')
      expect(got).toEqual(['g-chat', 'g-draft', 'g-embed'])
    }
  })

  it('a scope without aliases of its own is offered the _global set', () => {
    expect(ids(offeredAliases(WORLD, 'other'))).toEqual(['g-chat', 'g-draft', 'g-embed'])
  })
})

describe('scope options (Lead decision 1: picker never offers what the server would refuse)', () => {
  const tenantScopes = ['home', 'shared', 'research']

  it('tenant-admin: only its home scope — the only tenant scope model-alias-list shows it', () => {
    expect(scopeOptions({ serverAdmin: false, tenantScopes, homeScope: 'home' })).toEqual(['home'])
  })

  it('tenant-admin: a home scope the tenant does not list is not offered', () => {
    expect(scopeOptions({ serverAdmin: false, tenantScopes, homeScope: 'elsewhere' })).toEqual([])
    expect(scopeOptions({ serverAdmin: false, tenantScopes, homeScope: null })).toEqual([])
  })

  it('server-admin: every scope of the chosen tenant (reserved names dropped)', () => {
    expect(scopeOptions({ serverAdmin: true, tenantScopes: [...tenantScopes, '_system'], homeScope: 'home' })).toEqual([
      'home',
      'shared',
      'research',
    ])
  })
})

describe('templates (UI presets, E-05-3)', () => {
  it('App · Chat / App · Embedding preselect the offered aliases of their kind', () => {
    expect(TEMPLATES.map((t) => t.label)).toEqual(['App · Chat', 'App · Embedding'])
    const offered = offeredAliases(WORLD, 's1')
    expect(templateModels(offered, 'chat')).toEqual(['s1-chat'])
    expect(templateModels(offered, 'embedding')).toEqual(['g-embed'])
    expect(TEMPLATES.every((t) => t.expiryDays === 365)).toBe(true)
  })
})

describe('expiry against key_max_lifetime_s (design/01 E-01-4)', () => {
  const YEAR = 365 * 86_400

  it('max 365 d: 30/90 computed, 365 = the server default (no expires_at, no skew 400), never not offered', () => {
    const opts = expiryOptions(YEAR)
    expect(opts.map((o) => [o.value, o.disabled])).toEqual([
      ['30', false],
      ['90', false],
      ['365', false],
      ['date', false],
    ])
    expect(expirySpec({ kind: 'days', days: 30 }, YEAR)).toEqual({ expires_in_days: 30 })
    expect(expirySpec({ kind: 'days', days: 365 }, YEAR)).toEqual({})
    expect(expirySpec({ kind: 'never' }, YEAR)).toEqual({ error: 'this server requires an expiry (gateway.key_max_lifetime_app)' })
  })

  it('max 90 d: 365 disabled, 90 is the default', () => {
    const opts = expiryOptions(90 * 86_400)
    expect(opts.find((o) => o.value === '365')?.disabled).toBe(true)
    expect(expirySpec({ kind: 'days', days: 90 }, 90 * 86_400)).toEqual({})
    expect(expirySpec({ kind: 'days', days: 365 }, 90 * 86_400)).toHaveProperty('error')
  })

  it('max 0 (no ceiling): "never expires" offered and sends no expiry; presets are computed', () => {
    const opts = expiryOptions(0)
    expect(opts.map((o) => o.value)).toEqual(['30', '90', '365', 'date', 'never'])
    expect(expirySpec({ kind: 'never' }, 0)).toEqual({})
    expect(expirySpec({ kind: 'days', days: 365 }, 0)).toEqual({ expires_in_days: 365 })
  })

  it('a date is the end of that local day, sent as an absolute expires_at; garbage is refused', () => {
    const got = expirySpec({ kind: 'date', date: '2026-12-31' }, YEAR)
    expect(got).toHaveProperty('expires_at')
    expect(new Date((got as { expires_at: string }).expires_at).getTime()).toBe(new Date(2026, 11, 31, 23, 59, 59).getTime())
    expect(expirySpec({ kind: 'date', date: '' }, YEAR)).toEqual({ error: 'pick an expiry date' })
    expect(expirySpec({ kind: 'date', date: '2026-13-40' }, YEAR)).toEqual({ error: 'pick an expiry date' })
  })
})

describe('CIDR allowlist form check (server authoritative)', () => {
  it('empty ⇒ null (any source)', () => {
    expect(parseCidrs('  \n ')).toEqual({ cidrs: null, errors: [] })
  })

  it('accepts IPv4/IPv6 prefixes split by comma, space or newline', () => {
    expect(parseCidrs('10.0.0.0/8, 192.168.1.0/24\nfd00::/8')).toEqual({
      cidrs: ['10.0.0.0/8', '192.168.1.0/24', 'fd00::/8'],
      errors: [],
    })
  })

  it('refuses a bare address, a bad octet, a bad length and IPv4 host bits', () => {
    const r = parseCidrs('10.0.0.1 300.1.1.1/8 10.0.0.0/33 10.1.2.3/8 ::1/129 nonsense')
    expect(r.cidrs).toBeNull()
    expect(r.errors).toEqual([
      '"10.0.0.1" needs a prefix length (e.g. 10.0.0.1/32)',
      '"300.1.1.1/8" is not a CIDR prefix',
      '"10.0.0.0/33" is not a CIDR prefix',
      '"10.1.2.3/8" has host bits set (use 10.0.0.0/8)',
      '"::1/129" is not a CIDR prefix',
      '"nonsense" is not a CIDR prefix',
    ])
  })
})

describe('prompt sensitivity tier mirror (§4.5)', () => {
  it('server-admin: every value; tenant-admin: only credentials, the rest disabled "server-admin only"', () => {
    expect(sensitivityOptions(true).map((o) => [o.value, o.disabled])).toEqual([
      ['credentials', false],
      ['personal', false],
      ['internal', false],
      ['public', false],
    ])
    const ta = sensitivityOptions(false)
    expect(ta.map((o) => [o.value, o.disabled])).toEqual([
      ['credentials', false],
      ['personal', true],
      ['internal', true],
      ['public', true],
    ])
    expect(ta.filter((o) => o.disabled).every((o) => o.note === 'server-admin only')).toBe(true)
  })
})

function form(p: Partial<MintFormInput> = {}): MintFormInput {
  return {
    label: 'ci-app',
    description: '',
    tenantId: null,
    scope: null,
    allModels: false,
    models: [],
    offered: offeredAliases(WORLD, p.scope ?? null),
    expiry: { kind: 'days', days: 365 },
    maxLifetimeS: 365 * 86_400,
    sensitivity: 'credentials',
    cidrsText: '',
    ...p,
  }
}

/** The built spec as a plain record (fails loudly on an error result). */
function specOf(r: ReturnType<typeof buildCreateSpec>): Record<string, unknown> {
  if ('error' in r) throw new Error(`unexpected spec error: ${r.error}`)
  return r.spec as unknown as Record<string, unknown>
}

describe('create spec (G3/G4 wire)', () => {
  it('sends alias IDS of offered models only, never kind, the scope and the sensitivity', () => {
    const r = buildCreateSpec(form({ scope: 's1', offered: offeredAliases(WORLD, 's1'), models: ['s1-chat', 'g-chat', 'g-embed'] }))
    expect('error' in r).toBe(false)
    const spec = specOf(r)
    expect(spec).toEqual({ label: 'ci-app', tenant_scope: 's1', models: ['g-embed', 's1-chat'], prompt_sensitivity: 'credentials' })
    expect('kind' in spec).toBe(false)
  })

  it('all offered models ⇒ all_models true and no models list', () => {
    const r = buildCreateSpec(form({ allModels: true, models: ['g-chat'] }))
    expect(specOf(r)).toMatchObject({ all_models: true })
    expect(specOf(r)).not.toHaveProperty('models')
  })

  it('label is required and at most 100 characters; description at most 1000', () => {
    expect(buildCreateSpec(form({ label: '   ' }))).toEqual({ error: 'label is required' })
    expect(buildCreateSpec(form({ label: 'x'.repeat(101) }))).toEqual({ error: 'label must be at most 100 characters' })
    expect(buildCreateSpec(form({ description: 'y'.repeat(1001) }))).toEqual({ error: 'description must be at most 1000 characters' })
  })

  it('a server-admin target tenant rides as tenant_id; CIDRs and a computed expiry are carried', () => {
    const r = buildCreateSpec(form({ tenantId: 't-b', cidrsText: '10.0.0.0/8', expiry: { kind: 'days', days: 30 }, description: 'nightly' }))
    expect(specOf(r)).toEqual({
      label: 'ci-app',
      description: 'nightly',
      tenant_id: 't-b',
      prompt_sensitivity: 'credentials',
      allowed_cidrs: ['10.0.0.0/8'],
      expires_in_days: 30,
    })
  })

  it('a CIDR or expiry error blocks the spec', () => {
    expect(buildCreateSpec(form({ cidrsText: '10.0.0.1' }))).toEqual({ error: '"10.0.0.1" needs a prefix length (e.g. 10.0.0.1/32)' })
    expect(buildCreateSpec(form({ expiry: { kind: 'never' } }))).toHaveProperty('error')
  })
})

function cv(p: Partial<ConsumerView> = {}): ConsumerView {
  return {
    id: 'c1',
    tenant_id: 't',
    tenant_slug: 'acme',
    label: 'ci-chat',
    description: '',
    kind: 'app',
    tenant_scope: null,
    all_models: false,
    models: [],
    prompt_sensitivity: 'credentials',
    fair_weight: 1,
    admission_lane: 'bulk',
    allowed_cidrs: null,
    allow_plaintext: false,
    active: true,
    status: 'active',
    last_used_at: null,
    created_at: '2026-06-20T09:00:00Z',
    created_by_label: null,
    keys: [],
    keys_total: 1,
    peer: null,
    ...p,
  }
}

function key(id: string, hint: string, created: string) {
  return { id, hint, status: 'active' as const, created_at: created, expires_at: null, revoked_at: null, rotated_from: null, last_used_at: null }
}

describe('typed confirm tail (design/05 §4.6: the last 4 hint characters)', () => {
  it('hintTail = the last four characters of the hint', () => {
    expect(hintTail('ctxm_…a9Fq')).toBe('a9Fq')
  })

  it('deactivate: the tail of the YOUNGEST live key; no key ⇒ the label', () => {
    const c = cv({ keys: [key('k-old', 'ctxm_…mP2c', '2026-06-18T09:00:00Z'), key('k-new', 'ctxm_…Qe7z', '2026-06-28T09:00:00Z')] })
    expect(deactivateTyped(c)).toBe('Qe7z')
    expect(deactivateTyped(cv({ keys: [], label: 'nightly-embed' }))).toBe('nightly-embed')
  })

  it('a label with surrounding blanks (the server keeps them) is still typeable — slugMatches trims the input', () => {
    const c = cv({ keys: [], label: ' ci-chat  ' })
    expect(deactivateTyped(c)).toBe('ci-chat')
    expect(canConfirm(deactivateTyped(c), ' ci-chat  ')).toBe(true)
    expect(canConfirm(deactivateTyped(c), 'ci-chat')).toBe(true)
  })
})

describe('reactivate is granting (UI mirror of consumerPolicy.allowed)', () => {
  const sa = { serverAdmin: true, mintModelKeys: true }
  const taSS = { serverAdmin: false, mintModelKeys: true }
  const ta = { serverAdmin: false, mintModelKeys: false }

  it('only an inactive consumer; tenant-admin needs self-service and never on a ctx-peer', () => {
    expect(canReactivate(cv({ active: true }), sa)).toBe(false)
    expect(canReactivate(cv({ active: false }), sa)).toBe(true)
    expect(canReactivate(cv({ active: false }), taSS)).toBe(true)
    expect(canReactivate(cv({ active: false }), ta)).toBe(false)
    expect(canReactivate(cv({ active: false, kind: 'ctx-peer' }), taSS)).toBe(false)
    expect(canReactivate(cv({ active: false, kind: 'ctx-peer' }), sa)).toBe(true)
  })
})

describe('cancelled_inflight toast text (V-01-e, B-25, Lead decision 4)', () => {
  it('null ⇒ streams continue; a number ⇒ N running streams ended; never "0 cancelled" for null', () => {
    expect(cancelledText(null)).toBe('Running streams continue until they finish (live cancellation not available yet).')
    expect(cancelledText(undefined)).toBe('Running streams continue until they finish (live cancellation not available yet).')
    expect(cancelledText(3)).toBe('3 running streams ended.')
    expect(cancelledText(1)).toBe('1 running stream ended.')
    expect(cancelledText(0)).toBe('0 running streams ended.')
  })
})
