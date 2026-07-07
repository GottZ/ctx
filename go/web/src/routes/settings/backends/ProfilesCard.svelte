<script lang="ts">
  // Abschaltprofile-Karte (092, Web-UX U01-W6; design/01 §4.6 + AM-5/AM-7). Sitzt
  // über der Backend-Tabelle. Jedes Profil zeigt VOR jedem Klick, welche Backends
  // es trifft (Member-Chips mit sichtbaren Namen, nicht erst auf Hover) und welche
  // Rollen dadurch ausfallen — der Erstnutzer-Maßstab der Achse. Aktiv-Switch im
  // Stil des enabled-Switch der Tabelle; Create/Edit/Delete als schlanke Inline-
  // Form (name/label/description), kein zweiter Dialog. Ein Role-Blackout beim
  // Aktivieren wird über einen Klartext-Zwischenschritt bestätigt (alertdialog,
  // Fokus wandert auf „Trotzdem aktivieren", Escape bricht ab + Fokus zurück am
  // Switch, Fokus-Falle) — kein natives confirm() (§5.1/§5.5).
  //
  // AM-5 VOLL: server-admin schaltet/CRUDet alle Profile (_global-Profile treffen
  // physische Hosts, wirken in jede Tenant-Chain); tenant-admin sieht _global-
  // Profile read-only (Betroffenheit) und CRUDet/schaltet ausschließlich EIGENE
  // (tenant-scoped) Profile. Die Schreib-Rechte hier spiegeln die Server-Regel
  // (backendWritableByCaller): server-admin überall, sonst scope == homeScope.
  // Der Server bleibt autoritativ (er 404t/422t sonst). AM-7: neue Texte sagen
  // ausschließlich „Eject" — kein Alt-Wording im UI dieser Karte.
  import { session } from '../../../lib/auth.svelte'
  import type { DisableProfileView } from '../../../lib/api/profiles'
  import { profileKey, type ProfilesModel } from './profiles.svelte'

  let { profiles }: { profiles: ProfilesModel } = $props()

  // Operator-Scope physischer Hosts (config.go „gates a physical GPU host").
  const GLOBAL_SCOPE = '_global'

  // Any admin-or-up viewer may create a profile (server-admin → _global,
  // tenant-admin → own scope, forced server-side); the card only mounts for
  // such a viewer (BackendsPage self-gate).
  const canCreate = $derived(session.admin || session.caps.viewTenantBackends)

  /** Server-parity write predicate: server-admin everywhere, else own scope. */
  function writable(p: DisableProfileView): boolean {
    return session.admin || p.scope === session.homeScope
  }

  function displayLabel(p: DisableProfileView): string {
    return p.label !== '' ? p.label : p.name
  }

  // Impact line, German per design §4.6. embed is NOT surfaced as a raw role
  // name — embed_degraded becomes a plain sentence instead (§4.4).
  function affectedText(p: DisableProfileView): string {
    const n = p.impact.backends.length
    let s = `trifft ${n} ${n === 1 ? 'Backend' : 'Backends'}`
    if (p.impact.roles_affected.length > 0) s += ` · Rollen: ${p.impact.roles_affected.join(', ')}`
    return s
  }

  function darkRoles(p: DisableProfileView): string[] {
    return p.impact.roles_blacked_out.filter((r) => r !== 'embed')
  }

  // --- Blackout confirm step (§5.1) ------------------------------------------
  interface BlackoutConfirm {
    name: string
    scope: string
    label: string
    roles: string[]
    embedDegraded: boolean
    trigger: HTMLElement | null
  }
  let confirm = $state<BlackoutConfirm | null>(null)

  /**
   * Focus-managed alertdialog trap (§5.5): moves focus onto the confirm button,
   * traps Tab between the two actions, Escape cancels, and on teardown returns
   * focus to the switch that opened it. use:action so the wiring lives with the
   * node's lifetime — no manual mount/unmount bookkeeping.
   */
  function blackoutTrap(node: HTMLElement, trigger: HTMLElement | null) {
    const buttons = (): HTMLButtonElement[] => Array.from(node.querySelectorAll('button'))
    // Confirm is the LAST button ("Trotzdem aktivieren") — focus lands there.
    buttons().at(-1)?.focus()
    function onKey(e: KeyboardEvent): void {
      if (e.key === 'Escape') {
        e.preventDefault()
        cancelBlackout()
        return
      }
      if (e.key !== 'Tab') return
      const b = buttons()
      if (b.length === 0) return
      const first = b[0]
      const last = b[b.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    node.addEventListener('keydown', onKey)
    return {
      destroy(): void {
        node.removeEventListener('keydown', onKey)
        trigger?.focus()
      },
    }
  }

  async function onSwitch(p: DisableProfileView, e: Event & { currentTarget: HTMLInputElement }): Promise<void> {
    const input = e.currentTarget
    const desired = input.checked
    // Revert the visual to server truth immediately — the reload (or the confirm
    // path) drives the real flip. Prevents a checked-but-unwritten limbo.
    input.checked = p.active
    if (!desired) {
      try {
        await profiles.setActive(p.name, p.scope, false)
      } catch {
        /* profiles.actionError carries it */
      }
      return
    }
    try {
      const impact = await profiles.probeActivate(p.name, p.scope)
      if (impact.roles_blacked_out.length > 0) {
        confirm = {
          name: p.name,
          scope: p.scope,
          label: displayLabel(p),
          roles: impact.roles_blacked_out.filter((r) => r !== 'embed'),
          embedDegraded: impact.embed_degraded === true,
          trigger: input,
        }
      } else {
        await profiles.setActive(p.name, p.scope, true)
      }
    } catch {
      /* profiles.actionError carries it */
    }
  }

  async function acceptBlackout(): Promise<void> {
    if (!confirm) return
    const c = confirm
    confirm = null // tears down the trap → focus returns to the switch
    try {
      await profiles.setActive(c.name, c.scope, true, true)
    } catch {
      /* profiles.actionError carries it */
    }
  }

  function cancelBlackout(): void {
    confirm = null // trap teardown returns focus to the switch
  }

  // --- Inline create/edit form (§4.6: name/label/description only) -----------
  interface ProfileForm {
    mode: 'create' | 'edit'
    name: string
    scope: string
    label: string
    description: string
  }
  let form = $state<ProfileForm | null>(null)
  let formError = $state<string | null>(null)

  function openCreate(): void {
    form = { mode: 'create', name: '', scope: '', label: '', description: '' }
    formError = null
  }
  function openEdit(p: DisableProfileView): void {
    form = { mode: 'edit', name: p.name, scope: p.scope, label: p.label, description: p.description }
    formError = null
  }
  function closeForm(): void {
    form = null
    formError = null
  }

  async function submitForm(): Promise<void> {
    if (!form) return
    const f = form
    if (f.mode === 'create' && f.name.trim() === '') {
      formError = 'name erforderlich'
      return
    }
    formError = null
    try {
      if (f.mode === 'create') {
        // No scope sent — the server forces the tenant-admin's own scope and
        // defaults a server-admin to _global (backendCreateScope).
        await profiles.create({ name: f.name.trim(), label: f.label, description: f.description })
      } else {
        await profiles.update(f.name, f.scope, { label: f.label, description: f.description })
      }
      form = null
    } catch {
      formError = profiles.actionError
    }
  }

  // --- Inline delete confirm (mirrors the table's delete pattern) ------------
  let confirmDeleteKey = $state<string | null>(null)
  async function doDelete(p: DisableProfileView): Promise<void> {
    confirmDeleteKey = null
    try {
      await profiles.remove(p.name, p.scope)
    } catch {
      /* profiles.actionError carries it */
    }
  }
</script>

<section class="card" aria-label="Abschaltprofile">
  <header>
    <h2>abschaltprofile</h2>
    <span class="count">{profiles.profiles.length}</span>
    {#if canCreate}
      <button type="button" class="new" onclick={openCreate}>+ neues profil</button>
    {/if}
  </header>

  {#if profiles.actionError}
    <p class="problem error" role="alert">{profiles.actionError}</p>
  {/if}

  {#if form}
    <form class="edit-form" onsubmit={(e) => { e.preventDefault(); void submitForm() }}>
      <div class="row">
        <label class="field">
          <span class="lbl">name {form.mode === 'edit' ? '(fest)' : '(slug)'}</span>
          <input
            type="text"
            spellcheck="false"
            placeholder="z. B. gpu-wartung"
            disabled={form.mode === 'edit'}
            bind:value={form.name}
          />
        </label>
        <label class="field">
          <span class="lbl">label</span>
          <input type="text" placeholder="Anzeigename, z. B. GPU-Wartung" bind:value={form.label} />
        </label>
      </div>
      <label class="field">
        <span class="lbl">beschreibung</span>
        <input type="text" placeholder="Erstnutzer-Hinweis: was schaltet dieses Profil ab?" bind:value={form.description} />
      </label>
      {#if formError}<p class="problem error" role="alert">{formError}</p>{/if}
      <div class="form-actions">
        <button type="button" class="ghost" onclick={closeForm}>abbrechen</button>
        <button type="submit">{form.mode === 'create' ? 'anlegen' : 'speichern'}</button>
      </div>
    </form>
  {/if}

  {#if profiles.profiles.length === 0}
    <p class="empty">keine profile — lege eins an, um GPU-Hosts gebündelt für Wartung abzuschalten</p>
  {:else}
    <ul class="profiles">
      {#each profiles.profiles as p (profileKey(p.scope, p.name))}
        {@const key = profileKey(p.scope, p.name)}
        {@const busy = profiles.busyKey === key}
        {@const canWrite = writable(p)}
        {@const dark = darkRoles(p)}
        <li class="profile" class:on={p.active}>
          <div class="head">
            <div class="titles">
              <span class="plabel">{displayLabel(p)}</span>
              <span class="pmeta">{p.name}{p.scope !== GLOBAL_SCOPE ? ` · ${p.scope}` : ''}{p.reserved ? ' · reserviert' : ''}</span>
            </div>
            <div class="ctrl">
              {#if canWrite}
                <label class="switch" title="aktiv">
                  <input
                    type="checkbox"
                    aria-label={`${displayLabel(p)} aktiv`}
                    checked={p.active}
                    disabled={busy || (confirm?.name === p.name && confirm?.scope === p.scope)}
                    onchange={(e) => void onSwitch(p, e)}
                  />
                </label>
              {:else}
                <span class="ro-state" class:ro-on={p.active}>{p.active ? 'aktiv' : 'inaktiv'}</span>
              {/if}
            </div>
          </div>

          {#if p.description !== ''}<p class="pdesc">{p.description}</p>{/if}

          <div class="members">
            {#if p.impact.backends.length === 0}
              <span class="no-members">keine Backends zugeordnet — im Backend-Dialog zuweisen</span>
            {:else}
              {#each p.impact.backends as m (m.id)}
                <span class="chip" class:chip-off={m.effective_state === 'profile-disabled'} title={m.scope}>{m.name}</span>
              {/each}
            {/if}
          </div>

          <p class="impact">
            {affectedText(p)}{#if dark.length > 0}<span class="dark"> — {dark.join(', ')} {dark.length === 1 ? 'fällt' : 'fallen'} vollständig aus</span>{/if}
          </p>
          {#if p.impact.embed_degraded}
            <p class="impact embed">Embedding fällt aus — neue Blöcke bleiben nur per Volltext auffindbar (kein Datenverlust).</p>
          {/if}

          {#if canWrite}
            <div class="actions">
              <button type="button" class="ghost" onclick={() => openEdit(p)}>bearbeiten</button>
              {#if p.reserved}
                <span class="reserved-note" title="reserviert — die Eject-/Gaming-Alias-Fläche hängt daran">nicht löschbar</span>
              {:else if confirmDeleteKey === key}
                <span class="confirm-del">
                  löschen?
                  <button type="button" class="danger" onclick={() => void doDelete(p)}>ja</button>
                  <button type="button" class="ghost" onclick={() => (confirmDeleteKey = null)}>nein</button>
                </span>
              {:else}
                <button type="button" class="ghost danger-text" onclick={() => (confirmDeleteKey = key)}>löschen</button>
              {/if}
            </div>
          {/if}

          {#if confirm?.name === p.name && confirm?.scope === p.scope}
            <div
              class="blackout"
              role="alertdialog"
              aria-live="assertive"
              aria-label="Rollen-Blackout bestätigen"
              use:blackoutTrap={confirm.trigger}
            >
              <p class="bo-lead">„{confirm.label}" aktivieren nimmt diese Rollen vollständig vom Netz:</p>
              {#if confirm.roles.length > 0}
                <ul class="bo-roles">{#each confirm.roles as r (r)}<li>{r}</li>{/each}</ul>
              {/if}
              {#if confirm.embedDegraded}
                <p class="bo-embed">Embedding fällt aus — neue Blöcke bleiben nur per Volltext auffindbar (kein Datenverlust).</p>
              {/if}
              <div class="bo-actions">
                <button type="button" class="ghost" onclick={cancelBlackout}>abbrechen</button>
                <button type="button" class="danger" onclick={() => void acceptBlackout()}>Trotzdem aktivieren</button>
              </div>
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
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
  .new {
    margin-left: auto;
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  .profiles {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .profile {
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .profile:last-child {
    border-bottom: none;
  }
  .head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .titles {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .plabel {
    font-weight: var(--fw-semibold);
    font-size: var(--fs-sm);
  }
  .pmeta {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-faint);
  }
  .ctrl {
    margin-left: auto;
  }
  .switch input {
    accent-color: var(--accent);
  }
  .ro-state {
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .ro-state.ro-on {
    color: var(--warn);
  }
  .pdesc {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .members {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .chip {
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
  }
  .chip-off {
    border-color: var(--warn);
    color: var(--warn);
  }
  .no-members {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .impact {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .impact .dark {
    color: var(--warn);
  }
  .impact.embed {
    color: var(--warn);
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    flex-wrap: wrap;
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
  .confirm-del {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .reserved-note {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .blackout {
    margin-top: var(--space-1);
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .bo-lead {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .bo-roles {
    margin: 0;
    padding-left: var(--space-3);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .bo-embed {
    margin: 0;
    font-size: var(--fs-2xs);
    color: var(--warn);
  }
  .bo-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-1);
  }
  .edit-form {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  .edit-form .row {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex: 1;
    min-width: 0;
  }
  .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .field input {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .field input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .form-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  .problem {
    margin: var(--space-2) var(--space-3) 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid;
  }
  .problem.error {
    color: var(--danger);
    border-color: var(--danger);
    background: var(--danger-dim);
  }
</style>
