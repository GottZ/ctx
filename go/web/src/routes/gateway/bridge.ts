// Server-admin bridge into the consumer list (RC-D C6, wave W05-20, design/05
// §7: "Link trägt Tenant-Filter, Liste zeigt nur diesen Tenant — rot gegen
// ignorierten Query-Parameter"). /admin/tenants/:id links to
// /gateway?tenant=<id>; GatewayPage reads the parameter once on mount and
// puts it on ConsumersModel.filter before the first list read (the W05-05
// hand-over).
//
//   tenantFilterFromSearch(search, serverAdmin)
//     the tenant id of ?tenant= for a server-admin (a UUID only); null for
//     everyone else (the server answers 403 to tenant_id below server-admin,
//     so a tenant-admin's list ignores it by design) and for a malformed id.
//   gatewayTenantHref(id)  the link of the bridge card.

/** A tenant id as the register mints it (UUID, hyphenated). */
const TENANT_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/** The tenant filter carried by the /gateway query string. */
export function tenantFilterFromSearch(search: string, serverAdmin: boolean): string | null {
  if (!serverAdmin) return null
  const id = new URLSearchParams(search).get('tenant')
  return id !== null && TENANT_ID_RE.test(id) ? id : null
}

/** The /gateway link of a tenant (the bridge card's target). */
export function gatewayTenantHref(tenantId: string): string {
  return `/gateway?tenant=${encodeURIComponent(tenantId)}`
}
