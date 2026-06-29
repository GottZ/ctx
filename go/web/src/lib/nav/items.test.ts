// Nav-contract table (design 06-role-nav.md §4 N3-Gate). Per tier the exact
// item set, the corpus ⊆ tenant-admin ⊆ server-admin containment, the
// corpus → tenant → server grouping order, and loading → []. Caps are derived
// through the real capabilitiesFor (N1 dep) so the contract is tested
// end-to-end, not against hand-built flag combos that could drift.

import { describe, expect, it } from 'vitest'
import type { Capabilities } from '../auth/capabilities'
import { capabilitiesFor } from '../auth/capabilities'
import type { WhoamiResponse } from '../api/types'
import { navItems } from './items'

function whoami(p: Partial<WhoamiResponse> = {}): WhoamiResponse {
  return {
    success: true,
    label: 'a key',
    home_scope: 'private',
    read_scopes: ['private'],
    admin: false,
    tenant_id: '0190000000007000800000000000abcd',
    role: 'member',
    api_key_id: '0190000000007000800000000000ke7',
    tenant_slug: 'default',
    tenant_display_name: 'Default Tenant',
    ...p,
  }
}

const loading = capabilitiesFor(null)
const member = capabilitiesFor(whoami({ admin: false, role: 'member' }))
const tenantAdmin = capabilitiesFor(whoami({ admin: false, role: 'owner' }))
const serverAdmin = capabilitiesFor(whoami({ admin: true, role: 'owner' }))

const hrefs = (caps: Capabilities) => navItems(caps).map((i) => i.href)

const CORPUS = ['/blocks', '/graph', '/chat']
const TENANT = ['/tenant', '/tenant/backends']
const SERVER = ['/admin', '/settings', '/status', '/settings/backends']

describe('navItems', () => {
  it('loading → [] (no flickering rail before the session resolves, R6)', () => {
    expect(navItems(loading)).toEqual([])
  })

  it('member → corpus only', () => {
    expect(hrefs(member)).toEqual(CORPUS)
    expect(navItems(member).every((i) => i.section === 'corpus')).toBe(true)
  })

  it('tenant-admin → corpus + tenant section', () => {
    expect(hrefs(tenantAdmin)).toEqual([...CORPUS, ...TENANT])
  })

  it('server-admin → corpus + tenant + server section', () => {
    expect(hrefs(serverAdmin)).toEqual([...CORPUS, ...TENANT, ...SERVER])
  })

  it('containment: member ⊆ tenant-admin ⊆ server-admin', () => {
    const m = new Set(hrefs(member))
    const t = new Set(hrefs(tenantAdmin))
    const s = new Set(hrefs(serverAdmin))
    for (const h of m) expect(t.has(h)).toBe(true)
    for (const h of t) expect(s.has(h)).toBe(true)
  })

  it('sections are grouped in corpus → tenant → server order', () => {
    const rank = { corpus: 0, tenant: 1, server: 2 }
    const seq = navItems(serverAdmin).map((i) => rank[i.section])
    expect(seq).toEqual([...seq].sort((a, b) => a - b))
  })

  it('parents with a child nav item are exact (avoid double-highlight)', () => {
    const byHref = new Map(navItems(serverAdmin).map((i) => [i.href, i]))
    expect(byHref.get('/settings')?.exact).toBe(true)
    expect(byHref.get('/tenant')?.exact).toBe(true)
    // leaf routes leave exact unset (default startsWith match)
    expect(byHref.get('/settings/backends')?.exact).toBeUndefined()
    expect(byHref.get('/blocks')?.exact).toBeUndefined()
  })

  it('every item carries a non-empty iconKey and a known section', () => {
    for (const item of navItems(serverAdmin)) {
      expect(item.iconKey.length).toBeGreaterThan(0)
      expect(['corpus', 'tenant', 'server']).toContain(item.section)
    }
  })
})
