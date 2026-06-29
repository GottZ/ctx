<script lang="ts" module>
  // Identity footer badge (design 06-role-nav.md §3/§58, Welle N7). Replaces the
  // old read-only-only badge logic that lived inline in the NavRail footer
  // (key-label <span> + a single `read-only` badge). It renders the caller's
  // identity as three machine tags — API-key label, owning tenant, per-tenant
  // role — plus the read-only indicator, each with an accessible name (a11y
  // contract §58: meaning never carried by colour/title alone, App.svelte:45's
  // geerbtes title-only is NOT inherited).
  //
  // Expanded (labelled rail): label + tenant as text, role + read-only as named
  // badges. Collapsed (icon rail): only the compact named badges survive — role
  // initial + `RO` — the text label/tenant drop (too narrow), but the badges
  // keep their full aria-label so the icon rail stays screen-reader-complete.
  //
  // The pure projection (identityView) lives in this module script — no DOM — so
  // it unit-tests in the node-only vitest env (vite.config test.environment),
  // mirroring ThemeToggle.svelte. The body below is the thin token-only shell.

  /** The session fields this badge reads — a structural subset of `Session`
   *  (auth.svelte.ts) so the real singleton passes without an adapter. */
  export interface IdentitySource {
    label: string | null
    role: string | null
    tenantDisplayName: string | null
    tenantSlug: string | null
    homeScope: string | null
  }

  export interface IdentityView {
    /** API-key label, or null when absent/empty (omit). */
    label: string | null
    /** display_name || slug || null — the human tenant name (omit when both empty). */
    tenant: string | null
    /** owner|admin|member, or null for an unknown/empty role (no badge). */
    role: string | null
    /** Single uppercase letter for the collapsed rail (O|A|M), null when no role. */
    roleInitial: string | null
    /** home_scope === '' — the key has no writable scope (R-LEAK degradation). */
    readOnly: boolean
  }

  /** Roles that earn a named badge. An unknown/empty role yields no badge —
   *  forward-compat, mirroring capabilitiesFor's least-privilege degradation
   *  (06 §R3): a future role string renders nothing rather than a wrong tag. */
  export const BADGE_ROLES = ['owner', 'admin', 'member'] as const

  function clean(s: string | null): string | null {
    return s !== null && s !== '' ? s : null
  }

  /** Project a session-like source into the render-ready identity view. Pure. */
  export function identityView(src: IdentitySource): IdentityView {
    const role =
      src.role !== null && (BADGE_ROLES as readonly string[]).includes(src.role) ? src.role : null
    return {
      label: clean(src.label),
      tenant: clean(src.tenantDisplayName) ?? clean(src.tenantSlug),
      role,
      roleInitial: role ? role.charAt(0).toUpperCase() : null,
      readOnly: src.homeScope === '',
    }
  }
</script>

<script lang="ts">
  import { session } from '../auth.svelte'

  let {
    expanded = true,
  }: {
    /** Labels + text identity when true; compact named badges only when false
     *  (collapsed icon rail). Forwarded from NavRail. */
    expanded?: boolean
  } = $props()

  const view = $derived(identityView(session))
  // Skip an empty wrapper entirely (a writable key with an unknown role has
  // nothing to show in the icon rail).
  const hasText = $derived(Boolean(view.label || view.tenant || view.role || view.readOnly))
  const hasBadges = $derived(Boolean(view.role || view.readOnly))
</script>

{#if expanded}
  {#if hasText}
    <div class="identity">
      {#if view.label}
        <span class="key-label" title="API key label">{view.label}</span>
      {/if}
      {#if view.tenant}
        <span class="tenant" title="Tenant">{view.tenant}</span>
      {/if}
      {#if view.role || view.readOnly}
        <div class="badges">
          {#if view.role}
            <span class="badge role" aria-label={`Role: ${view.role}`} title={`Role: ${view.role}`}>
              {view.role}
            </span>
          {/if}
          {#if view.readOnly}
            <span
              class="badge ro"
              aria-label="Read-only key"
              title="This key has no writable scope (home_scope is empty) — mutating areas render read-only."
            >
              read-only
            </span>
          {/if}
        </div>
      {/if}
    </div>
  {/if}
{:else if hasBadges}
  <div class="identity icon">
    {#if view.role}
      <span class="badge role" aria-label={`Role: ${view.role}`} title={`Role: ${view.role}`}>
        {view.roleInitial}
      </span>
    {/if}
    {#if view.readOnly}
      <span
        class="badge ro"
        aria-label="Read-only key"
        title="This key has no writable scope (home_scope is empty) — mutating areas render read-only."
      >
        RO
      </span>
    {/if}
  </div>
{/if}

<style>
  .identity {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .identity.icon {
    align-items: center;
  }

  .key-label,
  .tenant {
    font-family: var(--font-mono);
    font-size: 0.8rem;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    padding: 0 var(--space-2);
  }
  .key-label {
    color: var(--text-dim);
  }
  .tenant {
    color: var(--text-faint);
  }

  .badges {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    padding: 0 var(--space-2);
  }

  .badge {
    align-self: flex-start;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    border: 1px solid currentColor;
    border-radius: var(--radius);
    padding: 0.05rem var(--space-2);
    white-space: nowrap;
  }
  .badge.role {
    color: var(--text-dim);
  }
  .badge.ro {
    color: var(--warn);
  }

  .identity.icon .badge {
    align-self: center;
    padding: 0.05rem var(--space-1);
  }
</style>
