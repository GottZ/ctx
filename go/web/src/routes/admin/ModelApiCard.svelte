<script lang="ts">
  // "Model API" card on /admin/tenants/:id (RC-D C6, wave W05-20; design/05
  // §7): the tenant's consumer counter (model-consumer-list {tenant_id} —
  // server-admin only, like this page; model-api-card.ts consumerCountText)
  // and the bridge link /gateway?tenant=<id> (bridge.ts gatewayTenantHref),
  // which opens the consumer list filtered to this tenant.
  import { untrack } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { listConsumers } from '../../lib/api/modelapi'
  import { session } from '../../lib/auth.svelte'
  import { gatewayTenantHref } from '../gateway/bridge'
  import { consumerCountText } from './model-api-card'

  let { tenantId }: { tenantId: string } = $props()

  let count = $state<string | null>(null)
  let error = $state<string | null>(null)
  let gen = 0

  $effect(() => {
    const id = tenantId
    if (session.tier !== 'server-admin' || id === '') return
    untrack(() => void load(id))
  })

  async function load(id: string): Promise<void> {
    const g = ++gen
    count = null
    error = null
    try {
      const page = await listConsumers({ tenant_id: id })
      if (g === gen) count = consumerCountText(page)
    } catch (err) {
      if (g === gen) error = toApiError(err).message
    }
  }
</script>

<section class="card" aria-label="model api">
  <header class="card-head">
    <h2>model api</h2>
    {#if count !== null}<span class="count" data-consumer-count>{count}</span>{/if}
  </header>
  <p class="body">
    {#if error !== null}
      <span class="err" role="status">consumer count unavailable: {error}</span>
    {:else if count === null}
      <span class="state" aria-busy="true">counting consumers…</span>
    {:else}
      <span>active model consumers of this tenant</span>
    {/if}
    <a href={gatewayTenantHref(tenantId)}>Open consumers</a>
  </p>
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .card-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  .body {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: baseline;
    margin: 0;
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .state {
    color: var(--text-faint);
  }
  .err {
    color: var(--warn);
  }
</style>
