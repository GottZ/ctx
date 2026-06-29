<script lang="ts">
  // Left navigation rail (design 01-shell-layout §3/§4 S3) — replaces the old
  // topbar. Renders the wordmark, the role-gated nav list (grouped corpus →
  // tenant → server) and the identity footer (label / read-only indicator /
  // logout). One component serves both the persistent desktop rail (AppShell
  // grid column) and the off-canvas mobile drawer (NavDrawer mounts it in its
  // panel, forced `expanded`) — it fills its container width either way.
  //
  // a11y (§3 contract): the active item carries aria-current="page" (not just a
  // colour class); when collapsed to icons every item + the footer controls keep
  // an accessible name via aria-label. The Footer-Slots region is where TH4
  // (ThemeToggle) and N7 (IdentityBadge) mount later — left structurally present.
  import Wordmark from './Wordmark.svelte'
  import { session } from './lib/auth.svelte'
  import { route } from './router'
  import { visibleNav } from './lib/layout/nav'
  import type { NavItem } from './lib/layout/nav'

  let {
    expanded = true,
    onToggle,
    onNavigate,
  }: {
    /** Labels + group headings shown when true; icon-only (with aria-labels) when false. */
    expanded?: boolean
    /** Desktop collapse/expand pin. Absent in the drawer (always expanded there). */
    onToggle?: () => void
    /** Fired on any nav click so the drawer can close itself after navigating. */
    onNavigate?: () => void
  } = $props()

  const items = $derived(visibleNav(session))

  // Section headings + render order. navItems already emits items in this order;
  // we only need the labels and to skip empty sections per tier.
  const SECTIONS = [
    { key: 'corpus', label: 'Corpus' },
    { key: 'tenant', label: 'Tenant' },
    { key: 'server', label: 'Server' },
  ] as const

  function inSection(section: string): NavItem[] {
    return items.filter((i) => i.section === section)
  }

  // Active state resolved synchronously off the reactive pathname (mirrors the
  // server reserved-prefix match). `exact` parents (e.g. /settings, /tenant)
  // match only their own path so a child route doesn't double-highlight them.
  function isItemActive(item: NavItem): boolean {
    const p = route.pathname
    return item.exact ? p === item.href : p === item.href || p.startsWith(`${item.href}/`)
  }

  const readOnly = $derived(session.homeScope === '')
</script>

{#snippet icon(key: string)}
  <svg
    class="glyph"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.6"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    {#if key === 'blocks'}
      <path d="M3 7l9-4 9 4-9 4-9-4z" /><path d="M3 12l9 4 9-4" /><path d="M3 16.5l9 4 9-4" />
    {:else if key === 'graph'}
      <circle cx="6" cy="6" r="2.2" /><circle cx="18" cy="9" r="2.2" /><circle
        cx="9.5"
        cy="18"
        r="2.2"
      /><path d="M7.9 7.1l8.2 1.5M8.1 16.1l1-7.8M11.2 17l5-6.6" />
    {:else if key === 'chat'}
      <path d="M4.5 5h15v10H10l-4 3v-3H4.5z" />
    {:else if key === 'keys'}
      <circle cx="8" cy="8" r="3.3" /><path d="M10.4 10.4L20 20M16 16l1.8-1.8M18.2 18.2l1.6-1.6" />
    {:else if key === 'backends'}
      <rect x="3.5" y="4.5" width="17" height="6" rx="1.4" /><rect
        x="3.5"
        y="13.5"
        width="17"
        height="6"
        rx="1.4"
      /><path d="M7 7.5h.01M7 16.5h.01" />
    {:else if key === 'tenants'}
      <path d="M4 20V8.5l6-3.5 6 3.5V20" /><path d="M3 20h18" /><path d="M9.5 20v-4.5h3V20" />
    {:else if key === 'settings'}
      <circle cx="12" cy="12" r="3" /><path
        d="M12 3.5v2.2M12 18.3v2.2M3.5 12h2.2M18.3 12h2.2M5.9 5.9l1.6 1.6M16.5 16.5l1.6 1.6M18.1 5.9l-1.6 1.6M7.5 16.5l-1.6 1.6"
      />
    {:else}
      <path d="M3 12h4l2.5-7 4 15 2.5-8H21" />
    {/if}
  </svg>
{/snippet}

<nav class="rail" class:icon={!expanded} aria-label="Primary">
  <a class="home" href="/status" title="ctx" onclick={() => onNavigate?.()}>
    <Wordmark />
  </a>

  <div class="groups">
    {#each SECTIONS as sec (sec.key)}
      {@const secItems = inSection(sec.key)}
      {#if secItems.length > 0}
        <div class="group" role="group" aria-label={sec.label}>
          {#if expanded}
            <p class="group-label">{sec.label}</p>
          {/if}
          <ul>
            {#each secItems as item (item.href)}
              {@const active = isItemActive(item)}
              <li>
                <a
                  href={item.href}
                  class:is-active={active}
                  aria-current={active ? 'page' : undefined}
                  title={expanded ? undefined : item.label}
                  aria-label={expanded ? undefined : item.label}
                  onclick={() => onNavigate?.()}
                >
                  {@render icon(item.iconKey)}
                  {#if expanded}
                    <span class="label">{item.label}</span>
                  {/if}
                </a>
              </li>
            {/each}
          </ul>
        </div>
      {/if}
    {/each}
  </div>

  <div class="footer">
    <!-- Footer-Slots: TH4 mounts ThemeToggle here, N7 mounts IdentityBadge. -->
    <div class="footer-slots"></div>

    {#if session.label && expanded}
      <span class="key-label" title="API key label">{session.label}</span>
    {/if}

    {#if readOnly}
      <span
        class="badge"
        title="This key has no writable scope (home_scope is empty) — mutating areas render read-only."
        aria-label="Read-only key"
      >
        {expanded ? 'read-only' : 'RO'}
      </span>
    {/if}

    <div class="footer-actions">
      <button
        class="logout"
        type="button"
        onclick={() => session.logout()}
        aria-label="Log out"
        title="Log out"
      >
        {#if expanded}
          Log out
        {:else}
          <svg
            class="glyph"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.6"
            stroke-linecap="round"
            stroke-linejoin="round"
            aria-hidden="true"
          >
            <path d="M14 4h4a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-4" /><path d="M9 16l-4-4 4-4" /><path
              d="M5 12h11"
            />
          </svg>
        {/if}
      </button>

      {#if onToggle}
        <button
          class="collapse"
          type="button"
          onclick={onToggle}
          aria-pressed={!expanded}
          aria-label={expanded ? 'Collapse sidebar' : 'Expand sidebar'}
          title={expanded ? 'Collapse sidebar' : 'Expand sidebar'}
        >
          <svg
            class="glyph"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.6"
            stroke-linecap="round"
            stroke-linejoin="round"
            aria-hidden="true"
          >
            {#if expanded}
              <path d="M15 6l-6 6 6 6" />
            {:else}
              <path d="M9 6l6 6-6 6" />
            {/if}
          </svg>
        </button>
      {/if}
    </div>
  </div>
</nav>

<style>
  .rail {
    height: 100%;
    box-sizing: border-box;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-2);
    background: var(--surface-1);
    border-right: 1px solid var(--border);
    overflow-y: auto;
    overflow-x: hidden;
  }

  .home {
    display: flex;
    align-items: center;
    text-decoration: none;
    font-size: 1.05rem;
    padding: 0 var(--space-2);
    overflow: hidden;
  }
  .rail.icon .home {
    justify-content: center;
    padding: 0;
  }

  .groups {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .group {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .group-label {
    margin: 0;
    padding: 0 var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .rail a:not(.home),
  .footer button {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2);
    color: var(--text-dim);
    text-decoration: none;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    border: 1px solid transparent;
    border-radius: var(--radius);
    background: transparent;
    transition: color 120ms ease, background 120ms ease;
  }
  .rail a:not(.home):hover,
  .footer button:hover {
    color: var(--text);
    background: var(--surface-2);
  }
  .rail a:global(.is-active) {
    color: var(--accent);
    background: var(--accent-dim);
  }

  .rail.icon a:not(.home),
  .rail.icon .footer button {
    justify-content: center;
    padding: var(--space-2) 0;
  }

  .glyph {
    width: 1.15rem;
    height: 1.15rem;
    flex: none;
  }
  .label {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .footer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border-top: 1px solid var(--border);
    padding-top: var(--space-3);
  }
  .footer-slots:empty {
    display: none;
  }
  .key-label {
    font-family: var(--font-mono);
    font-size: 0.8rem;
    color: var(--text-dim);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    padding: 0 var(--space-2);
  }
  .badge {
    align-self: flex-start;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--warn);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    padding: 0.05rem var(--space-2);
    white-space: nowrap;
  }
  .rail.icon .badge {
    align-self: center;
    padding: 0.05rem var(--space-1);
  }
  .footer-actions {
    display: flex;
    gap: var(--space-1);
  }
  .footer-actions .logout {
    flex: 1;
  }
  .rail.icon .footer-actions {
    flex-direction: column;
  }
</style>
