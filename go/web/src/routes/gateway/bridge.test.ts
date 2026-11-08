// bridge.ts gates (RC-D C6 S1, W05-20 — design/05 §7: "Link trägt
// Tenant-Filter, Liste zeigt nur diesen Tenant — rot gegen ignorierten
// Query-Parameter"). Red against the S1 stubs (null / '/gateway').

import { describe, expect, it } from 'vitest'
import { gatewayTenantHref, tenantFilterFromSearch } from './bridge'

const T = '550e8400-e29b-41d4-a716-446655440bbb'

describe('tenantFilterFromSearch (W05-20)', () => {
  it('server-admin: ?tenant=<uuid> becomes the list filter', () => {
    expect(tenantFilterFromSearch(`?tenant=${T}`, true), 'gate W05-20: rot gegen ignorierten Query-Parameter').toBe(T)
    expect(tenantFilterFromSearch(`?x=1&tenant=${T}`, true)).toBe(T)
  })
  it('tenant-admin: the parameter is ignored (tenant_id is server-admin only)', () => {
    expect(tenantFilterFromSearch(`?tenant=${T}`, false)).toBeNull()
  })
  it('absent or malformed ids are ignored', () => {
    for (const s of ['', '?tenant=', '?tenant=abc', '?tenant=%27%20OR%201%3D1', `?tenant=${T}x`])
      expect(tenantFilterFromSearch(s, true), s).toBeNull()
  })
})

describe('gatewayTenantHref (W05-20)', () => {
  it('the bridge link carries the tenant filter', () => {
    expect(gatewayTenantHref(T), 'gate W05-20: Link trägt Tenant-Filter').toBe(`/gateway?tenant=${T}`)
  })
  it('the id is URL-encoded', () => {
    expect(gatewayTenantHref('a b&c')).toBe('/gateway?tenant=a%20b%26c')
  })
})
