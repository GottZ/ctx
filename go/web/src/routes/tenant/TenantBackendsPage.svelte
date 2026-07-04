<script lang="ts">
  // /tenant/backends route wrapper (design 04 §4 E04-4, wave U11). The E04-4
  // rot-fix: the nav item pointed at /tenant/backends since U04, but no route was
  // registered — a deep link hit NotFound. This wrapper reuses the settings
  // BackendsPage in its tenant-scoped variant (pool only, viewTenantBackends
  // gate), so the tenant-admin backend pool becomes reachable without a second
  // page. Own file (not the areaRoutes lazy import pointing straight at the
  // settings page) so the /tenant/backends chunk is its own entry and the tenant
  // variant is a one-line, greppable seam.
  import BackendsPage from '../settings/backends/BackendsPage.svelte'
</script>

<BackendsPage tenantScoped />
