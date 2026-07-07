<script lang="ts">
  // Backend pool + secrets vault editor (design 04-§3.5, W5). Admin-gated: every
  // backend-* manage action and the secrets API answer 403 for a non-admin key,
  // so a read-only key gets the banner, not a doomed request. Loads the backend
  // list, the secret metadata and the settings list (the last only to derive the
  // "fehlt" status of sensitive secret_refs — its failure degrades the join, not
  // the page).
  //
  // U11 (design 04 §4 E04-4, §4.9): the SAME page mounts under two routes with
  // a parametrised self-gate — the SERVER-admin /settings/backends (default) and
  // the TENANT-admin /tenant/backends. The backend-* manage actions are already
  // tenant-admin-tier with a server-side tenant filter (backends_manage.go), so
  // the tenant variant gates on caps.viewTenantBackends and shows only the POOL.
  // The secrets VAULT stays server-admin only (the /api/secrets endpoint is not
  // tenant-scoped), so the tenant variant hides it — fail-closed, never a doomed
  // 403 read. The self-gate + crumb are the ONLY per-variant differences (the
  // pool table + models are shared byte-for-byte).
  import { onMount } from 'svelte'
  import { listSettings } from '../../../lib/api/settings'
  import type { SettingView } from '../../../lib/api/types'
  import { secretUsage } from '../../../lib/backends'
  import { session } from '../../../lib/auth.svelte'
  import { PoolModel } from './pool.svelte'
  import { VaultModel } from './vault.svelte'
  import { ProfilesModel } from './profiles.svelte'
  import BackendTable from './BackendTable.svelte'
  import ProfilesCard from './ProfilesCard.svelte'
  import VaultForm from './VaultForm.svelte'

  // tenantScoped=false → the server-admin /settings/backends surface (pool +
  // vault, gated on session.admin). tenantScoped=true → the tenant-admin
  // /tenant/backends surface (pool only, gated on viewTenantBackends).
  let { tenantScoped = false }: { tenantScoped?: boolean } = $props()

  const pool = new PoolModel()
  const vault = new VaultModel()
  // Disable-profiles (092, U01-W6). Mounts in BOTH variants (AM-5 VOLL): the
  // tenant-admin sees _global profiles read-only + CRUDs/toggles its own. The
  // disable-profile-* actions are tenant-admin-tier with server-side isolation,
  // so the tenant variant issues no doomed 403 read.
  const profiles = new ProfilesModel()
  let settings = $state<SettingView[]>([])

  // Gate: server variant needs the server-global admin flag; tenant variant
  // needs the tenant-admin-or-up capability (the /tenant prefix-guard already
  // redirects a lower tier, guard.ts TIER_GATED — this is the page self-gate).
  const allowed = $derived(tenantScoped ? session.caps.viewTenantBackends : session.admin)

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

  const profileOptions = $derived(profiles.profiles.map((p) => ({ name: p.name, label: p.label })))

  onMount(() => {
    if (!allowed) return
    void pool.load()
    void profiles.load()
    // The vault + its settings join are server-admin only (the /api/secrets +
    // /api/settings endpoints are not tenant-scoped) — never fetched in the
    // tenant variant, so a tenant-admin never issues a doomed 403 read.
    if (!tenantScoped) {
      void vault.load()
      void loadSettings()
    }
  })
</script>

<section class="area">
  <header>
    {#if tenantScoped}
      <div class="crumb"><a href="/tenant">Tenant</a> / backends</div>
      <h1>Backend pool</h1>
      <p class="sub">
        the LLM provider backends available to your tenant — trust tier, roles and priority drive routing and failover
        (server-side tenant-scoped)
      </p>
    {:else}
      <div class="crumb"><a href="/settings">Settings</a> / backends</div>
      <h1>Backend pool &amp; vault</h1>
      <p class="sub">
        provider backends, trust tier and API-key secrets — all server-side admin-gated; the pool drives LLM routing and
        failover
      </p>
    {/if}
  </header>

  {#if !allowed}
    <p class="banner" role="status">
      {#if tenantScoped}
        read-only key — the backend pool is tenant-admin-gated (the server answers 403); sign in with a tenant-admin key
        to manage it.
      {:else}
        read-only key — the backend pool and vault are admin-gated (the server answers 403); sign in with an admin key to
        manage them.
      {/if}
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
      {#if profiles.status === 'error'}
        <div class="error" role="alert">
          <p>Abschaltprofile nicht ladbar: {profiles.loadError?.message}</p>
          <button type="button" onclick={() => void profiles.reload()}>Retry</button>
        </div>
      {:else if profiles.status === 'ready'}
        <ProfilesCard {profiles} />
      {/if}
      <BackendTable {pool} {knownSecrets} {profileOptions} />
    {/if}

    {#if !tenantScoped}
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
