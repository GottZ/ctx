<script lang="ts">
  // Backend pool + secrets vault editor (design 04-§3.5, W5). Admin-gated: every
  // backend-* manage action and the secrets API answer 403 for a non-admin key,
  // so a read-only key gets the banner, not a doomed request. Loads the backend
  // list, the secret metadata and the settings list (the last only to derive the
  // "fehlt" status of sensitive secret_refs — its failure degrades the join, not
  // the page).
  import { onMount } from 'svelte'
  import { listSettings } from '../../../lib/api/settings'
  import type { SettingView } from '../../../lib/api/types'
  import { secretUsage } from '../../../lib/backends'
  import { session } from '../../../lib/auth.svelte'
  import { PoolModel } from './pool.svelte'
  import { VaultModel } from './vault.svelte'
  import BackendTable from './BackendTable.svelte'
  import VaultForm from './VaultForm.svelte'

  const pool = new PoolModel()
  const vault = new VaultModel()
  let settings = $state<SettingView[]>([])

  const usage = $derived(secretUsage(vault.secrets, pool.backends, settings))
  const knownSecrets = $derived(new Set(vault.secrets.map((s) => s.name)))

  async function loadSettings(): Promise<void> {
    // Only feeds the dangling-ref join; a failure must not break the editor.
    try {
      settings = (await listSettings()).settings
    } catch {
      settings = []
    }
  }

  onMount(() => {
    if (!session.admin) return
    void pool.load()
    void vault.load()
    void loadSettings()
  })
</script>

<section class="area">
  <header>
    <div class="crumb"><a href="/settings">Settings</a> / backends</div>
    <h1>Backend pool &amp; vault</h1>
    <p class="sub">
      provider backends, trust tier and API-key secrets — all server-side admin-gated; the pool drives LLM routing and
      failover
    </p>
  </header>

  {#if !session.admin}
    <p class="banner" role="status">
      read-only key — the backend pool and vault are admin-gated (the server answers 403); sign in with an admin key to
      manage them.
    </p>
  {:else}
    {#if pool.status === 'loading' || pool.status === 'idle'}
      <p class="state" aria-busy="true">loading backend pool…</p>
    {:else if pool.status === 'error'}
      <div class="error" role="alert">
        <p>{pool.loadError?.message}</p>
        {#if pool.loadError?.requestId}<p class="request-id">request {pool.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void pool.reload()}>Retry</button>
      </div>
    {:else}
      <BackendTable {pool} {knownSecrets} />
    {/if}

    {#if vault.status === 'loading' || vault.status === 'idle'}
      <p class="state" aria-busy="true">loading vault…</p>
    {:else if vault.status === 'error'}
      <div class="error" role="alert">
        <p>vault unavailable: {vault.loadError?.message}</p>
        <button type="button" onclick={() => void vault.reload()}>Retry</button>
      </div>
    {:else}
      <VaultForm {vault} {usage} />
    {/if}
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  .crumb {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-faint);
  }
  .crumb a {
    color: var(--text-dim);
  }
  h1 {
    margin: var(--space-1) 0 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: var(--fs-sm);
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }
</style>
