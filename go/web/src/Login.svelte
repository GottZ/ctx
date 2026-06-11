<script lang="ts">
  import Wordmark from './Wordmark.svelte'
  import { session } from './lib/auth.svelte'
  import { toApiError, type ApiError } from './lib/api'

  let key = $state('')
  let busy = $state(false)
  let error = $state<ApiError | null>(null)

  const submittable = $derived(!busy && key.trim() !== '')

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault()
    if (!submittable) return
    busy = true
    error = null
    try {
      await session.login(key)
      key = ''
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }
</script>

<div class="screen">
  <form class="card" onsubmit={submit}>
    <span class="frame" aria-hidden="true"></span>
    <header>
      <Wordmark />
      <p class="tagline">Your AI's save game.</p>
    </header>

    {#if session.notice}
      <p class="notice" role="status">{session.notice}</p>
    {/if}

    <label class="field-label" for="api-key">API key</label>
    <input
      id="api-key"
      type="password"
      autocomplete="off"
      spellcheck="false"
      placeholder="paste an API key"
      bind:value={key}
      {@attach (node) => node.focus()}
    />

    <button type="submit" disabled={!submittable}>
      {busy ? 'Checking key…' : 'Sign in'}
    </button>

    {#if error}
      <div class="error" role="alert">
        <p>{error.message}</p>
        {#if error.requestId}
          <p class="request-id">request {error.requestId}</p>
        {/if}
      </div>
    {/if}

    <p class="hint">
      The key is held for this tab only (sessionStorage) and sent as a
      <code>Bearer</code> token to <code>/api/*</code>.
    </p>
  </form>
</div>

<style>
  .screen {
    min-height: 100vh;
    display: grid;
    place-items: center;
    padding: var(--space-3);
    background:
      radial-gradient(60rem 40rem at 50% 18%, var(--accent-dim), transparent 60%),
      radial-gradient(rgba(216, 218, 229, 0.035) 1px, transparent 1px);
    background-size:
      auto,
      24px 24px;
  }

  .card {
    position: relative;
    width: min(24rem, 100%);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--surface-1);
    border: 1px solid var(--border);
    padding: var(--space-4);
  }

  /* Corner brackets — card carries the top pair, .frame the bottom pair. */
  .card::before,
  .card::after,
  .frame::before,
  .frame::after {
    content: '';
    position: absolute;
    width: 14px;
    height: 14px;
    border: 1px solid var(--accent);
  }
  .card::before {
    top: -1px;
    left: -1px;
    border-right: none;
    border-bottom: none;
  }
  .card::after {
    top: -1px;
    right: -1px;
    border-left: none;
    border-bottom: none;
  }
  .frame {
    position: absolute;
    inset: 0;
    pointer-events: none;
  }
  .frame::before {
    bottom: -1px;
    left: -1px;
    border-right: none;
    border-top: none;
  }
  .frame::after {
    bottom: -1px;
    right: -1px;
    border-left: none;
    border-top: none;
  }

  .card > :global(*) {
    animation: rise 0.35s ease backwards;
  }
  .card > header {
    animation-delay: 0ms;
  }
  .card > .field-label,
  .card > input {
    animation-delay: 60ms;
  }
  .card > button {
    animation-delay: 120ms;
  }
  @keyframes rise {
    from {
      opacity: 0;
      transform: translateY(6px);
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .card > :global(*) {
      animation: none;
    }
  }

  header {
    margin-bottom: var(--space-2);
    font-size: 1.5rem;
  }
  .tagline {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.9rem;
  }

  .notice {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: 0.85rem;
  }

  .field-label {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }

  input {
    font-family: var(--font-mono);
  }

  button {
    margin-top: var(--space-1);
    border-color: var(--accent);
    color: var(--accent);
    background: transparent;
  }
  button:hover:not(:disabled) {
    background: var(--accent-dim);
  }

  .error {
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: 0.85rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    margin-top: var(--space-1) !important;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim) !important;
  }

  .hint {
    margin: var(--space-2) 0 0;
    color: var(--text-faint);
    font-size: 0.78rem;
    line-height: 1.45;
  }
</style>
