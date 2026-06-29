<script lang="ts">
  // Member landing — the dedicated capability screen (design 06-role-nav.md §3,
  // D2, Welle N6). Explains the caller's corpus footprint in prose (write scope,
  // read scopes, role, tenant) and points to the three corpus surfaces. The
  // landing-resolver flips landingFor(member) → /home in the same commit that
  // registers this route (§94: no NotFound window).
  //
  // Reading-column self-limited: areaMode('/home') === 'reading' (unmapped →
  // reading default), so the shell caps `.area` on --measure-wide and centres it
  // (AppShell §4). No full-bleed; no own magic-number cap (K4 contract).
  //
  // Overwhelmingly declarative — reads the session deriveds reactively, no local
  // $state, so $derived re-runs whenever whoami resolves/changes.
  import { session } from '../lib/auth.svelte'
  import CapabilityCard from '../lib/ui/CapabilityCard.svelte'

  // home_scope === '' is a read-only key (no writable scope) — spell it out
  // rather than render an empty value (mirrors IdentityBadge's readOnly tag).
  const writeScope = $derived(session.homeScope ? session.homeScope : 'none — read-only key')
  const readView = $derived(session.readScopes.length ? session.readScopes.join(', ') : '—')
  const roleView = $derived(session.role ?? '—')
  // UUID is meaningless to humans (R5) — show the display name / slug, never the id.
  const tenantView = $derived(session.tenantDisplayName ?? session.tenantSlug ?? '—')
</script>

<section class="area">
  <header>
    <h1>Welcome{session.label ? `, ${session.label}` : ''}</h1>
    <p class="sub">
      Your corpus access at a glance — what you can read, where your writes land, and where to start.
    </p>
  </header>

  <div class="cards">
    <CapabilityCard
      title="Write scope"
      value={writeScope}
      copy="New blocks you store land in this scope, and only blocks in it are yours to edit."
    />
    <CapabilityCard
      title="Read access"
      value={readView}
      copy="Search, ask and the graph span these scopes."
    />
    <CapabilityCard
      title="Role"
      value={roleView}
      copy="Your role within this tenant. Higher roles unlock tenant and server administration."
    />
    <CapabilityCard
      title="Tenant"
      value={tenantView}
      copy="The tenant your key belongs to; its shared scope is visible to every member."
    />
  </div>

  <div class="group">
    <h2 class="section-title">Start here</h2>
    <div class="cards">
      <CapabilityCard title="Blocks" copy="Browse, search and edit the blocks in your scopes.">
        {#snippet cta()}
          <a class="action" href="/blocks">Browse blocks →</a>
        {/snippet}
      </CapabilityCard>
      <CapabilityCard title="Graph" copy="Explore how your blocks connect.">
        {#snippet cta()}
          <a class="action" href="/graph">Open the graph →</a>
        {/snippet}
      </CapabilityCard>
      <CapabilityCard title="Chat" copy="Ask the corpus a question in natural language.">
        {#snippet cta()}
          <a class="action" href="/chat">Start a chat →</a>
        {/snippet}
      </CapabilityCard>
    </div>
  </div>
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }

  .group {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .section-title {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--text);
  }

  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: var(--space-3);
  }

  .action {
    font-family: var(--font-mono);
    font-size: 0.85rem;
    color: var(--accent);
    text-decoration: none;
  }
  .action:hover {
    text-decoration: underline;
  }
</style>
