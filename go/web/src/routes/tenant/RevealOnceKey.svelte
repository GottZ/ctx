<script lang="ts">
  // Show-once plaintext key reveal (design 05-tenant-admin §6/§7.5, Wave TK4) —
  // SECURITY-CRITICAL. `api-key-create` returns the plaintext `api_key` EXACTLY
  // ONCE (only its SHA-256 hash is kept server-side); it is never re-derivable.
  //
  // Hygiene contract: the plaintext lives ONLY in this component's local $state
  // (`secret`), snapshotted once at init. It is NEVER written to KeysModel, the
  // session store, sessionStorage/localStorage, the URL, or the console. Ack OR
  // dismiss wipes it (`secret = ''`) and the parent drops its own copy in onack.
  // The single deliberate exception is the user-initiated Copy button, which
  // puts the value on the system clipboard — an accepted trade-off (it then
  // lives outside the app's control), not extra app-side persistence.
  import { onMount } from 'svelte'

  let { apiKey, onack }: { apiKey: string; onack: () => void } = $props()

  // Snapshot the prop ONCE — the parent can drop its own reference without
  // blanking this display mid-reveal; ack() then wipes this copy too. The
  // initial-value capture is intentional (the prop is stable for the reveal's
  // lifetime; a new key mounts a fresh instance), so the lint is suppressed.
  // svelte-ignore state_referenced_locally
  let secret = $state(apiKey)
  let copied = $state(false)
  let copyFailed = $state(false)

  // $state so the onMount focus reads the committed ref (a11y §6: initial focus
  // on Copy; the revealed value sits in a role=status region for AT).
  let copyBtnEl = $state<HTMLButtonElement>()
  onMount(() => copyBtnEl?.focus())

  async function copy(): Promise<void> {
    copyFailed = false
    try {
      await navigator.clipboard.writeText(secret)
      copied = true
    } catch {
      // Clipboard API can reject (insecure context / denied permission). The
      // value stays selectable in the field, so fall back to a manual hint.
      copyFailed = true
    }
  }

  function ack(): void {
    // Wipe the plaintext from state on acknowledge (design §6: ack clears the
    // value); the parent drops its own reference in onack().
    secret = ''
    copied = false
    onack()
  }
</script>

<div class="reveal">
  <p class="warn-line" role="alert">
    This key is shown <strong>once</strong>. Copy and store it now — it cannot be retrieved again.
  </p>

  <div class="key-region" role="status" aria-live="polite">
    <input
      class="key"
      type="text"
      readonly
      value={secret}
      spellcheck="false"
      aria-label="new api key — plaintext, shown once"
      onfocus={(e) => e.currentTarget.select()}
    />
    <button bind:this={copyBtnEl} type="button" class="copy" onclick={() => void copy()}>
      {copied ? 'copied' : 'copy'}
    </button>
  </div>

  {#if copyFailed}
    <p class="hint warn">Clipboard blocked — select the value above and copy it manually.</p>
  {:else if copied}
    <p class="hint ok">Copied — it now also lives in your system clipboard, outside this app.</p>
  {/if}

  <div class="ack-row">
    <button type="button" class="ack" onclick={ack}>I've stored it — done</button>
  </div>
</div>

<style>
  .reveal {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .warn-line {
    margin: 0;
    font-size: var(--fs-sm);
    line-height: var(--lh-body);
    color: var(--warn);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    padding: var(--space-2);
  }
  .warn-line strong {
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
  }
  .key-region {
    display: flex;
    gap: var(--space-2);
    align-items: stretch;
  }
  .key {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--accent);
    border-radius: var(--radius);
    color: var(--text);
  }
  .copy {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    padding: 0 var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
    white-space: nowrap;
  }
  .copy:hover {
    border-color: var(--accent);
    color: var(--accent);
  }
  .hint {
    margin: 0;
    font-size: var(--fs-xs);
  }
  .hint.ok {
    color: var(--ok);
  }
  .hint.warn {
    color: var(--warn);
  }
  .ack-row {
    display: flex;
    justify-content: flex-end;
  }
  .ack {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  .ack:hover {
    border-color: var(--accent);
  }
</style>
