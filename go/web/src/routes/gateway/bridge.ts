// Server-admin bridge into the consumer list (RC-D C6, wave W05-20, design/05
// §7: "Link trägt Tenant-Filter, Liste zeigt nur diesen Tenant — rot gegen
// ignorierten Query-Parameter"). /admin/tenants/:id links to
// /gateway?tenant=<id>; GatewayPage reads the parameter once on mount and
// hands it to ConsumersModel.setFilter({tenantId}) (the W05-05 hand-over).
//
//   tenantFilterFromSearch(search, serverAdmin)
//     the tenant id of ?tenant= for a server-admin (a UUID only); null for
//     everyone else (the server answers 403 to tenant_id below server-admin,
//     so a tenant-admin's list ignores it by design) and for a malformed id.
//   gatewayTenantHref(id)  the link of the bridge card.
//
// RC-D S1: contract stubs — tenantFilterFromSearch answers null (the list
// stays unfiltered), gatewayTenantHref answers '/gateway' (no filter).

/** The tenant filter carried by the /gateway query string. */
export function tenantFilterFromSearch(search: string, serverAdmin: boolean): string | null {
  void search
  void serverAdmin
  return null
}

/** The /gateway link of a tenant (the bridge card's target). */
export function gatewayTenantHref(tenantId: string): string {
  void tenantId
  return '/gateway'
}
