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
// Workflow feature flag on (design 04 §4.1.3, U04) — a per-tenant flag, so it
// stacks onto ANY tier. A plain member with the flag gets the workflow section.
const memberWorkflow = capabilitiesFor(whoami({ admin: false, role: 'member', capabilities: { workflow: true } }))
const serverAdminWorkflow = capabilitiesFor(whoami({ admin: true, role: 'owner', capabilities: { workflow: true } }))

const hrefs = (caps: Capabilities) => navItems(caps).map((i) => i.href)

const CORPUS = ['/blocks', '/graph', '/chat']
// U11 (design 04 §4 E04-4): the /tenant/backends route exists now, so the item
// is re-shown in the tenant section (the S11 fail-closed hide is lifted).
const WORKFLOW = ['/issues', '/board']
// Guard W4: /guard (review queue) joins the tenant section, gated on
// viewOpsSurfaces (tenant-admin-or-up).
const TENANT = ['/tenant', '/tenant/backends', '/guard']
const SERVER = ['/admin', '/settings', '/status', '/settings/backends']

describe('navItems', () => {
  it('loading → [] (no flickering rail before the session resolves, R6)', () => {
    expect(navItems(loading)).toEqual([])
  })

  it('member → corpus only', () => {
    expect(hrefs(member)).toEqual(CORPUS)
    expect(navItems(member).every((i) => i.section === 'corpus')).toBe(true)
  })

  it('workflow section hidden without the flag (dark-launch, U04)', () => {
    for (const caps of [member, tenantAdmin, serverAdmin]) {
      expect(navItems(caps).some((i) => i.section === 'workflow')).toBe(false)
    }
  })

  it('workflow flag on → workflow section between corpus and tenant (U04)', () => {
    // Plain member + flag: corpus, then the workflow section, nothing else.
    expect(hrefs(memberWorkflow)).toEqual([...CORPUS, ...WORKFLOW])
    const wf = navItems(memberWorkflow).filter((i) => i.section === 'workflow')
    expect(wf.map((i) => i.href)).toEqual(WORKFLOW)
    // /issues is exact so /issues/:id does not double-highlight it; /board is a leaf.
    const byHref = new Map(navItems(memberWorkflow).map((i) => [i.href, i]))
    expect(byHref.get('/issues')?.exact).toBe(true)
    expect(byHref.get('/board')?.exact).toBeUndefined()
  })

  it('workflow section sits between corpus and tenant/server for a full tier (U04)', () => {
    expect(hrefs(serverAdminWorkflow)).toEqual([...CORPUS, ...WORKFLOW, ...TENANT, ...SERVER])
    const rank = { corpus: 0, workflow: 1, tenant: 2, server: 3 }
    const seq = navItems(serverAdminWorkflow).map((i) => rank[i.section])
    expect(seq).toEqual([...seq].sort((a, b) => a - b))
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
    const rank = { corpus: 0, workflow: 1, tenant: 2, server: 3 }
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
    for (const item of navItems(serverAdminWorkflow)) {
      expect(item.iconKey.length).toBeGreaterThan(0)
      expect(['corpus', 'workflow', 'tenant', 'server']).toContain(item.section)
    }
  })
})
