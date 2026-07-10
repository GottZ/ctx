<script lang="ts">
  // Write-confirmation card (F6-C6 D-W6b, design 06 §3.8 pending_action line):
  // the human-in-the-loop mechanism of the chat harness. Rendered for every
  // staged ctx_store result — live from the tool_result event and after a
  // session reload from the persisted tool message (render.ts). Confirm calls
  // POST /api/confirm (Cookie-Session + X-CSRF-Token via apiFetch, OAuth R4);
  // dismiss is CLIENT-side only: the stage simply expires server-side
  // (confirm_ttl) and the retention ticker evicts it — no discard endpoint.
  import { toApiError } from '../../lib/api'
  import { confirmWrite } from '../../lib/chat/api'
  import type { StagedWriteInfo } from '../../lib/chat/types'

  let { staged }: { staged: StagedWriteInfo } = $props()

  type Phase = 'open' | 'confirming' | 'confirmed' | 'dismissed' | 'failed'
  let phase = $state<Phase>('open')
  let resultText = $state('')

  // D-W6c: the card carries both forms — a staged NEW block (op 'store') and
  // a staged field-level update (op 'update', with the hash-bound field list).
  const isUpdate = $derived(staged.op === 'update')

  const expiry = $derived.by(() => {
    if (!staged.expires_at) return 'never expires'
    const t = new Date(staged.expires_at)
    return Number.isNaN(t.getTime()) ? staged.expires_at : `expires ${t.toLocaleString()}`
  })

  async function confirm(): Promise<void> {
    phase = 'confirming'
    try {
      const res = await confirmWrite(staged.payload_hash)
      resultText = `${isUpdate ? 'updated' : 'saved'}: ${res.block.title} (${res.block.id.slice(0, 8)}…)`
      phase = 'confirmed'
    } catch (err) {
      resultText = toApiError(err).message
      phase = 'failed'
    }
  }

  function dismiss(): void {
    phase = 'dismissed'
  }
</script>

<div class="confirm" class:done={phase === 'confirmed'} class:failed={phase === 'failed'}>
  <div class="head">
    <span class="badge">{isUpdate ? 'staged update' : 'staged write'}</span>
    <span class="title">{staged.title}</span>
    <span class="cat">{staged.category}</span>
  </div>
  <div class="meta">
    <span>scope <b>{staged.scope}</b></span>
    {#if staged.sensitivity}<span>sensitivity <b>{staged.sensitivity}</b></span>{/if}
    {#if isUpdate && staged.update_fields?.length}
      <span>changes <b>{staged.update_fields.join(', ')}</b></span>
    {/if}
    {#if staged.content_chars > 0}<span>{staged.content_chars} chars</span>{/if}
    <span class="hash" title={staged.payload_hash}>{staged.payload_hash.slice(0, 12)}…</span>
  </div>
  {#if staged.content_preview}
    <pre class="preview">{staged.content_preview}</pre>
  {/if}

  {#if phase === 'open' || phase === 'confirming'}
    <div class="actions">
      <button class="ok" type="button" disabled={phase === 'confirming'} onclick={confirm}>
        {phase === 'confirming' ? 'confirming…' : isUpdate ? 'Confirm & update' : 'Confirm & save'}
      </button>
      <button class="no" type="button" disabled={phase === 'confirming'} onclick={dismiss}>Dismiss</button>
      <span class="expiry">{expiry}</span>
    </div>
  {:else if phase === 'confirmed'}
    <div class="result ok-text" role="status">✓ {resultText}</div>
  {:else if phase === 'dismissed'}
    <div class="result dim" role="status">dismissed — the stage {expiry === 'never expires' ? 'stays pending' : expiry}</div>
  {:else}
    <div class="result err-text" role="alert">✗ {resultText}</div>
  {/if}
</div>

<style>
  .confirm {
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    background: var(--surface-1);
    margin: var(--space-1) 0;
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .confirm.done {
    border-color: var(--border);
  }
  .confirm.failed {
    border-color: var(--danger-dim);
  }
  .head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--warn);
  }
  .title {
    color: var(--text);
    font-weight: var(--fw-semibold);
  }
  .cat {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
  .meta {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .meta b {
    color: var(--text-dim);
    font-weight: var(--fw-semibold);
  }
  .hash {
    margin-left: auto;
  }
  .preview {
    margin: 0;
    white-space: pre-wrap;
    word-break: break-word;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
    background: var(--surface-0);
    border-radius: var(--radius);
    padding: var(--space-2);
    max-height: 10rem;
    overflow: auto;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .expiry {
    margin-left: auto;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
  .result {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .ok-text {
    color: var(--ok, var(--accent));
  }
  .err-text {
    color: var(--danger);
  }
  .dim {
    color: var(--text-faint);
  }
</style>
