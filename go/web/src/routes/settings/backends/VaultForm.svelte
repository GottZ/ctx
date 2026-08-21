<script module lang="ts">
  import type { SecretRefs } from '../../../lib/backends'

  /**
   * The delete-confirm line. Both reference types END in a 409 since 04-W5 —
   * the server's guard scans settings AND pool rows — so the text names the
   * blocker instead of predicting different outcomes per type. Exported for the
   * unit gate (the ConnState pattern): the branch is behind a click and a
   * server-rendered probe never reaches it.
   */
  export function deleteConfirmText(refs: SecretRefs): string {
    if (refs.settings.length > 0 && refs.backends.length > 0) {
      return 'blocked by settings and backends — delete will 409'
    }
    if (refs.settings.length > 0) return 'blocked by settings — delete will 409'
    if (refs.backends.length > 0) {
      return `blocked by backend ${refs.backends.join(', ')} — delete will 409`
    }
    return 'delete?'
  }
</script>

<script lang="ts">
  // Secrets vault (design §3.5). Write-only: values go in via RedactedInput and
  // are never read back. Each secret shows where it is referenced, BOTH kinds
  // straight from the server's referenced_by (04-W5): settings keys verbatim,
  // backend pool rows under the "backend:" prefix, taken apart by
  // splitSecretRefs. Either kind blocks the DELETE with a 409 — the server's
  // scan is the same one the guard runs, so the confirm text can state the
  // outcome instead of guessing at it. The dangling block lists refs whose
  // secret is missing (the derived "fehlt"), the one direction referenced_by
  // cannot carry.
  import { toApiError } from '../../../lib/api'
  import { splitSecretRefs, type SecretUsage } from '../../../lib/backends'
  import RedactedInput from './RedactedInput.svelte'
  import type { VaultModel } from './vault.svelte'

  let { vault, usage }: { vault: VaultModel; usage: SecretUsage } = $props()

  // mirrors store.ValidSecretName: lowercase, [a-z0-9._-], starts alphanumeric.
  const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,127}$/

  let newName = $state('')
  let formError = $state<string | null>(null)
  let rowError = $state<Record<string, string>>({})
  let confirmDelete = $state<string | null>(null)

  function fmt(iso: string | undefined): string {
    return iso ? iso.slice(0, 16).replace('T', ' ') : ''
  }

  async function createSecret(value: string): Promise<boolean> {
    const name = newName.trim()
    formError = null
    if (!NAME_RE.test(name)) {
      formError = 'invalid name — lowercase letters/digits/._-, must start alphanumeric, max 128 chars'
      return false
    }
    try {
      await vault.put(name, value)
      newName = ''
      return true
    } catch (err) {
      formError = toApiError(err).message
      return false
    }
  }

  async function rotate(name: string, value: string): Promise<boolean> {
    delete rowError[name]
    try {
      await vault.put(name, value)
      return true
    } catch (err) {
      rowError[name] = toApiError(err).message
      return false
    }
  }

  async function remove(name: string): Promise<void> {
    confirmDelete = null
    delete rowError[name]
    try {
      await vault.remove(name)
    } catch (err) {
      rowError[name] = toApiError(err).message
    }
  }
</script>

<section class="card" aria-label="secrets vault">
  <header>
    <h2>vault</h2>
    <span class="count">{vault.secrets.length}</span>
    <span class="sub">write-only — values are set here and never shown again</span>
  </header>

  {#if usage.dangling.length > 0}
    <div class="problem warn" role="status">
      <p class="dangling-head">dangling references — these point at a secret that does not exist:</p>
      {#each usage.dangling as d (`${d.source}:${d.ref}`)}
        <p><code>{d.source === 'backend' ? `backend ${d.ref}` : `setting ${d.ref}`}</code> → <strong>{d.secret}</strong> (fehlt)</p>
      {/each}
    </div>
  {/if}

  {#if vault.secrets.length === 0}
    <p class="empty">no secrets yet — add one below (e.g. an OpenRouter API key)</p>
  {:else}
    <ul class="list">
      {#each vault.secrets as s (s.name)}
        {@const refs = splitSecretRefs(s.referenced_by)}
        {@const referenced = refs.settings.length > 0 || refs.backends.length > 0}
        <li class="secret">
          <div class="meta-row">
            <span class="name">{s.name}</span>
            <span class="ver">v{s.key_version}</span>
            <span class="dates">
              set {fmt(s.created_at)}{#if s.rotated_at} · rotated {fmt(s.rotated_at)}{/if}
            </span>
          </div>

          {#if referenced}
            <p class="refs">
              referenced by:
              {#each refs.settings as k (k)}<code class="ref">setting {k}</code>{/each}
              {#each refs.backends as b (b)}<code class="ref">backend {b}</code>{/each}
            </p>
          {:else}
            <p class="refs none">no references</p>
          {/if}

          <div class="row-actions">
            <RedactedInput submitLabel="rotate" busy={vault.busyName === s.name} onsubmit={(v) => rotate(s.name, v)} />
            {#if confirmDelete === s.name}
              <span class="confirm-del">
                {deleteConfirmText(refs)}
                <button type="button" class="danger" onclick={() => void remove(s.name)}>yes</button>
                <button type="button" class="ghost" onclick={() => (confirmDelete = null)}>no</button>
              </span>
            {:else}
              <button type="button" class="ghost danger-text" disabled={vault.busyName === s.name} onclick={() => (confirmDelete = s.name)}>delete</button>
            {/if}
          </div>

          {#if rowError[s.name]}
            <p class="problem error" role="alert">{rowError[s.name]}</p>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}

  <div class="new">
    <span class="new-label">add / set a secret</span>
    <div class="new-row">
      <input
        type="text"
        class="new-name"
        spellcheck="false"
        placeholder="name (e.g. openrouter.key)"
        bind:value={newName}
      />
      <RedactedInput submitLabel="set" busy={vault.busyName === newName.trim()} disabled={newName.trim() === ''} onsubmit={createSecret} />
    </div>
    {#if formError}
      <p class="problem error" role="alert">{formError}</p>
    {/if}
  </div>
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  header {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
    flex-wrap: wrap;
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
  .sub {
    color: var(--text-faint);
    font-size: var(--fs-xs);
  }
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .secret {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--surface-2);
  }
  .meta-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .name {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
  }
  .ver {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-faint);
  }
  .dates {
    margin-left: auto;
    font-size: var(--fs-2xs);
    color: var(--text-faint);
    font-family: var(--font-mono);
  }
  .refs {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    align-items: center;
  }
  .refs.none {
    color: var(--text-faint);
  }
  .ref {
    font-family: var(--font-mono);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .row-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .row-actions :global(.redacted) {
    flex: 1;
    min-width: 14rem;
  }
  .confirm-del {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .ghost {
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
    background: transparent;
  }
  .danger-text {
    color: var(--danger);
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
  }
  .new {
    padding: var(--space-2) var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    border-top: 1px solid var(--border);
  }
  .new-label {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .new-row {
    display: flex;
    gap: var(--space-2);
    align-items: center;
    flex-wrap: wrap;
  }
  .new-name {
    flex: 0 1 14rem;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .new-row :global(.redacted) {
    flex: 1;
    min-width: 14rem;
  }
  .dangling-head {
    font-weight: var(--fw-semibold);
  }
  .problem {
    margin: var(--space-2) var(--space-3) 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid;
  }
  .secret .problem,
  .new .problem {
    margin: var(--space-1) 0 0;
  }
  .problem.error {
    color: var(--danger);
    border-color: var(--danger);
    background: var(--danger-dim);
  }
  .problem.warn {
    color: var(--warn);
    border-color: var(--warn);
    background: transparent;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .problem.warn p {
    margin: 0;
  }
  code {
    font-family: var(--font-mono);
  }
</style>
