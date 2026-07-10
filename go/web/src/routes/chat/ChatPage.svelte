<script lang="ts">
  // Web-chat route (design 06 §F6-C5). Owns the ChatStore, loads the session
  // list on mount, and lays out sidebar + thread + composer. The turn is
  // aborted on unmount / page-hide so navigating away frees the llama.cpp slot
  // (the abort is final — no resume, §3.9).
  import { onMount } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import { ChatStore } from '../../lib/chat/store.svelte'
  import ChatInput from './ChatInput.svelte'
  import MessageList from './MessageList.svelte'
  import SessionSidebar from './SessionSidebar.svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'

  const store = new ChatStore(() => session.csrfToken)

  // Onboarding empty-state (Welle N8): a genuinely fresh corpus — no saved
  // sessions and no draft turn — gets a friendly entry point instead of the bare
  // thread hint. Gated so it never flashes mid-load or over a load error, and it
  // yields to MessageList the instant a draft/turn begins (messages populate).
  const showOnboarding = $derived(
    store.sessions.length === 0 &&
      store.messages.length === 0 &&
      !store.loadingList &&
      !store.streaming &&
      store.loadError === null,
  )

  onMount(() => {
    void store.loadSessions()
    const onHide = (): void => store.abort()
    window.addEventListener('beforeunload', onHide)
    return () => {
      window.removeEventListener('beforeunload', onHide)
      store.abort()
    }
  })
</script>

<!-- S12 (Q3, design 05-§2.5): .area-Wurzel-Konvention angeglichen (S7-Selektor-Kontrakt); chat ist thread-Mode, der reading-.area-Cap greift hier nicht. -->
<div class="chat area">
  <SessionSidebar {store} />
  <section class="conversation">
    {#if store.loadError}
      <div class="load-error" role="alert">
        {store.loadError.message}
        <button type="button" onclick={() => store.loadSessions()}>Retry</button>
      </div>
    {/if}
    {#if showOnboarding}
      <EmptyState
        title="No conversations yet"
        copy="Ask the knowledge store a question in the box below — it can search, browse and read your blocks. Your chats then appear in the sidebar."
      >
        {#snippet cta()}
          <a class="empty-cta" href="/blocks">Browse the corpus →</a>
        {/snippet}
      </EmptyState>
    {:else}
      <MessageList {store} />
    {/if}
    <ChatInput {store} />
  </section>
</div>

<style>
  /* thread mode (design 01-shell-layout §4 S7 + Content-Region-Contract).
     AppShell renders this inside .content[data-mode=thread], a definite-height
     100dvh grid cell; height:100% pins .chat to it (replacing the S3 stopgap
     calc) so the WHOLE region never page-scrolls — only the inner .thread does.
     box-sizing keeps the 1px border inside that 100% → no overflow, no double
     scrollbar. container-type makes .chat the query container for the < sm
     session drawer (region-, not viewport-relative → holds with the rail open or
     closed) and the positioning context that drawer/scrim absolutely-position
     against. */
  .chat {
    display: flex;
    position: relative;
    height: 100%;
    min-height: 0;
    box-sizing: border-box;
    container-type: inline-size;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .conversation {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
  }
  /* Messages read in a centred --measure-prose column (design 01 §1/§7.3). The
     .thread stays the full-width scroller (scrollbar at the pane edge, composer
     divider full width) while its content is gutter-centred to a readable line
     length; only the inline padding of MessageList's .thread is overridden — its
     vertical padding and scroll chain are untouched. Bubble alignment (user
     right / assistant full) is preserved because the column, not each bubble, is
     constrained. */
  .conversation :global(.thread) {
    padding-inline: max(var(--space-3), calc((100% - var(--measure-prose)) / 2));
  }
  /* N8 onboarding CTA — corpus-entry link (mirrors MemberHome's .action). */
  .empty-cta {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--accent);
    text-decoration: none;
  }
  .empty-cta:hover {
    text-decoration: underline;
  }

  .load-error {
    margin: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--danger-dim);
    color: var(--danger);
    border-radius: var(--radius);
    font-size: var(--fs-sm);
    display: flex;
    gap: var(--space-2);
    align-items: center;
  }

  /* < sm (region width, design 01 §6): the session list collapses to a slide-in
     drawer (SessionSidebar owns it) overlaying the conversation. Reserve room at
     the top of the conversation so the floating session toggle never sits over
     the first message. */
  @container (max-width: 40rem) {
    .conversation {
      padding-top: var(--space-4);
    }
  }
</style>
