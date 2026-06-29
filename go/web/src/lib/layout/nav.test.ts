// visibleNav contract (design 01-shell-layout §3/§4 S3). The thin shell-side
// wrapper must forward the session's caps to navItems unchanged: per tier the
// exact href set, unknown/empty role degrading to the member set (least
// privilege, forward-compat), and loading → []. Caps are derived through the
// real capabilitiesFor (N1) so the seam is tested end-to-end, not against
// hand-built flag combos. The grouping/ordering/exact contract itself is pinned
// in lib/nav/items.test.ts — this file pins only that visibleNav passes through.

import { describe, expect, it } from 'vitest'
import { capabilitiesFor } from '../auth/capabilities'
import type { WhoamiResponse } from '../api/types'
import { visibleNav } from './nav'

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

const hrefs = (whoamiResponse: WhoamiResponse | null) =>
  visibleNav({ caps: capabilitiesFor(whoamiResponse) }).map((i) => i.href)

const CORPUS = ['/blocks', '/graph', '/chat']
const TENANT = ['/tenant', '/tenant/backends']
const SERVER = ['/admin', '/settings', '/status', '/settings/backends']

describe('visibleNav', () => {
  it('loading (null whoami) → [] (no flickering rail before the session resolves)', () => {
    expect(visibleNav({ caps: capabilitiesFor(null) })).toEqual([])
  })

  it('member → corpus only', () => {
    expect(hrefs(whoami({ admin: false, role: 'member' }))).toEqual(CORPUS)
  })

  it('tenant-admin (owner) → corpus + tenant', () => {
    expect(hrefs(whoami({ admin: false, role: 'owner' }))).toEqual([...CORPUS, ...TENANT])
  })

  it('server-admin (admin flag) → corpus + tenant + server', () => {
    expect(hrefs(whoami({ admin: true, role: 'owner' }))).toEqual([...CORPUS, ...TENANT, ...SERVER])
  })

  it('unknown/empty role degrades to the member set (least privilege)', () => {
    expect(hrefs(whoami({ admin: false, role: 'wizard' }))).toEqual(CORPUS)
    expect(hrefs(whoami({ admin: false, role: '' }))).toEqual(CORPUS)
  })
})
