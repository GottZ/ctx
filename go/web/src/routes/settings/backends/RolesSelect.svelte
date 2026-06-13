<script lang="ts">
  // roles multi-select (design §3.5): the core roles as a checkbox group plus a
  // free-text line for proxy roles (allowed; the server warns on non-core). The
  // F3 role model is a LIST per backend (herbert-chat carries 5). Controlled:
  // value in, onchange out — the dialog owns the draft.
  import { CORE_ROLES } from '../../../lib/backends'

  let {
    value,
    disabled = false,
    onchange,
  }: { value: string[]; disabled?: boolean; onchange: (roles: string[]) => void } = $props()

  const coreSet: readonly string[] = CORE_ROLES
  const customRoles = $derived(value.filter((r) => !coreSet.includes(r)))
  let custom = $state('')

  function toggle(role: string, on: boolean): void {
    onchange(on ? [...value, role] : value.filter((r) => r !== role))
  }

  function addCustom(): void {
    const r = custom.trim()
    custom = ''
    if (r === '' || value.includes(r)) return
    onchange([...value, r])
  }
</script>

<div class="roles" class:disabled>
  <div class="grid">
    {#each CORE_ROLES as role (role)}
      <label class="role">
        <input
          type="checkbox"
          {disabled}
          checked={value.includes(role)}
          onchange={(e) => toggle(role, e.currentTarget.checked)}
        />
        <span>{role}</span>
      </label>
    {/each}
  </div>

  {#if customRoles.length > 0}
    <div class="chips">
      {#each customRoles as role (role)}
        <span class="chip">
          {role}
          <button type="button" title="remove proxy role" {disabled} onclick={() => toggle(role, false)}>×</button>
        </span>
      {/each}
    </div>
  {/if}

  <div class="add">
    <input
      type="text"
      placeholder="add a proxy role…"
      spellcheck="false"
      {disabled}
      bind:value={custom}
      onkeydown={(e) => {
        if (e.key === 'Enter') {
          e.preventDefault()
          addCustom()
        }
      }}
    />
    <button type="button" {disabled} onclick={addCustom}>add</button>
  </div>
</div>

<style>
  .roles {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(8rem, 1fr));
    gap: var(--space-1) var(--space-2);
  }
  .role {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    color: var(--text-dim);
  }
  .role input {
    accent-color: var(--accent);
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    border: 1px solid var(--accent);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--accent);
  }
  .chip button {
    background: transparent;
    border: none;
    color: inherit;
    cursor: pointer;
    padding: 0;
    font-size: 0.9rem;
    line-height: 1;
  }
  .add {
    display: flex;
    gap: var(--space-2);
  }
  .add input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: var(--space-1) var(--space-2);
  }
  .add button {
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-2);
  }
  .disabled {
    opacity: 0.6;
  }
</style>
