<script lang="ts">
  import type { BlockDetailModel } from './detail.svelte'
  import type { BlockDeleteModel } from './delete.svelte'
  import { sensitivityBadge } from '../../lib/blocks/sensitivity'
  import Modal from '../../lib/ui/Modal.svelte'

  // Detail panel (block-workbench W3 read; W4 adds an Edit action; W5 a Delete
  // action). The model owns the lazy getBlock load + status; this component is a
  // thin sidebar mirroring the graph DetailSidebar: a meta-<dl>, the full content
  // as a TEXT NODE (never {@html} — repo XSS convention), an "open in graph"
  // deep-link and a close button. Edit + Delete are page-supplied callbacks —
  // only passed when the block is editable (canEdit: scope === home_scope), so a
  // foreign-scope (read-only) block has neither affordance. Delete goes through a
  // shared Modal confirm gate (design 05 §7 Q9 — the native <dialog>+showModal
  // wiring lives in lib/ui/Modal.svelte); the destructive call lives in the
  // injected BlockDeleteModel.
  let {
    model,
    onedit,
    deleteModel,
  }: { model: BlockDetailModel; onedit?: () => void; deleteModel?: BlockDeleteModel } = $props()

  // The confirm dialog is mounted lazily (only while confirming) so Modal's
  // onMount showModal() runs on its own — exactly the prior lazy mount pattern.
  let confirming = $state(false)
  let confirmDialogEl: HTMLDialogElement | undefined = $state()

  function openConfirm(): void {
    if (deleteModel) deleteModel.error = null
    confirming = true
  }

  async function confirmDelete(id: string): Promise<void> {
    const ok = await deleteModel?.remove(id)
    // On success the page closes the panel (model.close); keep the dialog up on
    // failure so model.error stays visible next to the still-open block.
    if (ok) {
      confirmDialogEl?.close()
      confirming = false
    }
  }

  function cancelConfirm(): void {
    confirmDialogEl?.close()
  }
</script>

<aside class="detail-pane" aria-label="block details">
  <header>
    <h2>{model.block?.title ?? 'Block'}</h2>
    <button class="close" type="button" title="close" onclick={() => model.close()}>×</button>
  </header>

  {#if model.status === 'loading'}
    <p class="state" aria-busy="true">loading…</p>
  {:else if model.status === 'error'}
    <p class="problem" role="alert">{model.loadError?.message ?? 'failed to load block'}</p>
    {#if model.loadError?.requestId}
      <p class="request-id">request {model.loadError.requestId}</p>
    {/if}
  {:else if model.block}
    {@const b = model.block}
    {@const sb = sensitivityBadge(b.sensitivity ?? '')}
    <dl class="meta">
      <dt>id</dt>
      <dd><code>{b.id}</code></dd>
      <dt>category</dt>
      <dd>{b.category}</dd>
      <dt>scope</dt>
      <dd>{b.scope}</dd>
      <dt>sensitivity</dt>
      <dd><span class="badge {sb.tone}">{sb.label}</span></dd>
      {#if b.tags.length > 0}
        <dt>tags</dt>
        <dd>{b.tags.join(', ')}</dd>
      {/if}
      <dt>created</dt>
      <dd>{b.created_at.slice(0, 10)}</dd>
      <dt>updated</dt>
      <dd>{b.updated_at.slice(0, 10)}</dd>
    </dl>

    {#if model.openId}
      <div class="actions">
        <a href={`/graph?focus=${model.openId}`}>Open in graph</a>
        {#if onedit}
          <button type="button" class="edit" onclick={onedit}>Edit</button>
        {/if}
        {#if deleteModel}
          <button type="button" class="del" onclick={openConfirm}>Delete</button>
        {/if}
      </div>
    {/if}

    <!-- Always a text node, never {@html} — repo XSS convention (DetailSidebar). -->
    <pre class="content">{b.content}</pre>
  {/if}

  {#if confirming && deleteModel}
    {@const id = model.openId ?? ''}
    <!-- Destructive confirm gate on the shared Modal shell (design 05 §7 Q9).
         The model holds no dialog state. -->
    <Modal bind:dialogEl={confirmDialogEl} width="24rem" onclose={() => (confirming = false)}>
      <div class="confirm-body">
        <h3>Delete block</h3>
        <p class="confirm-text">Delete this block? This cannot be undone.</p>
        {#if deleteModel.error}
          <p class="problem error" role="alert">{deleteModel.error}</p>
        {/if}
      </div>
      <div class="confirm-actions">
        <button type="button" class="ghost" disabled={deleteModel.busy} onclick={cancelConfirm}>Cancel</button>
        <button type="button" class="danger" disabled={deleteModel.busy} onclick={() => void confirmDelete(id)}>
          {deleteModel.busy ? 'deleting…' : 'Delete'}
        </button>
      </div>
    </Modal>
  {/if}
</aside>

<style>
  /* Detail pane of the blocks master-detail (design 01 §4 S6). Fixed reading
     width via the --detail-w token; bounded to the workbench height by
     BlocksPage's .workbench{align-items:stretch}, so this is its own scroll
     region (overflow-y:auto). The .workbench>.detail-pane rule in BlocksPage
     asserts the same chain from the parent side. */
  .detail-pane {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    width: var(--detail-w);
    max-width: 40vw;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    padding: var(--space-3);
    overflow-y: auto;
  }

  /* ≤sm: the workbench stacks (BlocksPage @container) → the pane goes full-width
     and stops being its own scroller (the stacked workbench owns the single
     scroll). Queries BlocksPage's .area inline-size container. */
  @container (max-width: 40rem) {
    .detail-pane {
      width: 100%;
      max-width: none;
      overflow-y: visible;
    }
  }

  header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
    line-height: var(--lh-heading);
    flex: 1;
  }
  .close {
    padding: 0 var(--space-2);
    font-size: var(--fs-base);
    line-height: var(--lh-heading);
  }

  .meta {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.15rem var(--space-3);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
    align-self: baseline;
  }
  dd {
    margin: 0;
    color: var(--text-dim);
    overflow-wrap: anywhere;
  }
  dd code {
    font-size: var(--fs-2xs);
  }

  /* Sensitivity badge (W6) — tone is a token-driven CSS-class suffix
     (BackendBadge pattern); colours come only from styles/tokens.css. */
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
  }
  .badge.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  .badge.warn {
    border-color: var(--warn);
    color: var(--warn);
  }
  .badge.accent {
    border-color: var(--accent);
    color: var(--accent);
  }
  .badge.ok {
    border-color: var(--ok);
    color: var(--ok);
  }

  .actions {
    display: flex;
    gap: var(--space-2);
  }
  .actions a {
    font-size: var(--fs-sm);
    color: var(--accent);
  }
  .actions .edit,
  .actions .del {
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
  }
  .actions .del {
    border-color: var(--danger);
    color: var(--danger);
  }

  /* Confirm gate body + action row inside the shared Modal shell (Q9). The box
     chrome (width/border/radius/surface/backdrop) is owned by Modal now; only
     the inner layout lives here. .confirm-actions carries what .confirm-dialog
     .actions did before the shell extraction (flex-end row + top divider). */
  .confirm-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
  }
  .confirm-body h3 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .confirm-text {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  .confirm-actions {
    display: flex;
    gap: var(--space-2);
    justify-content: flex-end;
    border-top: 1px solid var(--border);
    padding: var(--space-2) var(--space-3);
  }
  button.ghost {
    background: transparent;
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  .problem.error {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--danger);
    background: var(--danger-dim);
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .problem {
    margin: 0;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .request-id {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }

  /* Block content as a readable column: monospace <pre> wraps at --measure-code
     (~90ch @0.78rem) so a wide pane (e.g. full-width when stacked) never runs
     edge-to-edge (design 01 §1/§7.3). In the 22rem split pane it is naturally
     narrower; the cap only binds once the pane grows. */
  .content {
    margin: 0;
    padding: var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    font-size: var(--fs-xs);
    line-height: var(--lh-body);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-width: var(--measure-code);
  }
</style>
