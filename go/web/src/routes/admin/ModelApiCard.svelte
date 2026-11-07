<script lang="ts">
  // "Model API" card on /admin/tenants/:id (RC-D C6, wave W05-20; design/05
  // §7): the tenant's consumer counter (model-consumer-list {tenant_id},
  // model-api-card.ts consumerCountText) and the bridge link
  // /gateway?tenant=<id> (bridge.ts gatewayTenantHref), which opens the
  // consumer list filtered to this tenant. Server-admin only (the page is).
  //
  // RC-D S1: contract stub — renders nothing.
  let { tenantId }: { tenantId: string } = $props()
  // S1: the props are read nowhere yet; the effect keeps them referenced.
  $effect(() => void [tenantId])
</script>
