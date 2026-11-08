<script lang="ts">
  // Policy form of a model alias (design/05 §3.3 `policy`; wave W05-13 — RC-D
  // C5): one labelled control per catalog.policyFields(kind) — an integer
  // input for the limits, default/on/off for `vision`, default/off/passthrough
  // for `reasoning`. An empty control REMOVES its key (absent = the server's
  // default), never sets undefined/null, so the canonical comparison of
  // buildAliasUpdate sees no phantom change. Validation is the server's
  // fail-closed parser: a 422 `fields` entry (catalog.fieldErrors) renders at
  // its control (aria-invalid + message) — keyed by the bare key or by
  // `policy.<key>`. The bindable `policy` is replaced, never mutated.
  import type { ModelAliasKind } from '../../lib/api/types'
  import { policyFields } from './catalog'
  import { policyDefaultLabel } from './vision'

  interface Props {
    kind: ModelAliasKind
    policy: Record<string, unknown>
    /** field → message from a 422 */
    errors?: Record<string, string>
    disabled?: boolean
  }
  let { kind, policy = $bindable(), errors = {}, disabled = false }: Props = $props()

  const uid = `ap-${Math.random().toString(36).slice(2, 9)}`
  const fields = $derived(policyFields(kind))

  function put(key: string, value: unknown): void {
    const next = { ...(policy ?? {}) }
    if (value === undefined) delete next[key]
    else next[key] = value
    policy = next
  }

  function intOf(raw: string): number | undefined {
    const t = raw.trim()
    if (t === '') return undefined
    const n = Number(t)
    return Number.isFinite(n) ? n : undefined
  }

  function boolOf(raw: string): boolean | undefined {
    return raw === 'true' ? true : raw === 'false' ? false : undefined
  }

  function shown(key: string): string {
    const v = policy?.[key]
    return v === undefined || v === null ? '' : String(v)
  }

  function errorOf(key: string): string | undefined {
    return errors[key] ?? errors[`policy.${key}`]
  }
</script>

<fieldset class="policy">
  <legend>policy</legend>
  <p class="hint">empty = the server default; the server checks every value.</p>
  <div class="grid">
    {#each fields as f (f.key)}
      {@const id = `${uid}-${f.key.replace(/\./g, '-')}`}
      {@const err = errorOf(f.key)}
      <div class="field">
        <label for={id}>{f.key}</label>
        {#if f.type === 'int'}
          <input
            {id}
            type="number"
            inputmode="numeric"
            min="0"
            step="1"
            placeholder="default"
            value={shown(f.key)}
            {disabled}
            aria-invalid={err ? 'true' : undefined}
            aria-describedby={err ? `${id}-err` : undefined}
            onchange={(e) => put(f.key, intOf(e.currentTarget.value))}
          />
        {:else if f.type === 'bool'}
          <select
            {id}
            value={shown(f.key)}
            {disabled}
            aria-invalid={err ? 'true' : undefined}
            aria-describedby={err ? `${id}-err` : undefined}
            onchange={(e) => put(f.key, boolOf(e.currentTarget.value))}
          >
            <!-- FOLGE-Z Z5: unset vision follows the backend probe (Z2) — say so -->
            <option value="">{policyDefaultLabel(f.key)}</option>
            <option value="true">on</option>
            <option value="false">off</option>
          </select>
        {:else}
          <select
            {id}
            value={shown(f.key)}
            {disabled}
            aria-invalid={err ? 'true' : undefined}
            aria-describedby={err ? `${id}-err` : undefined}
            onchange={(e) => put(f.key, e.currentTarget.value === '' ? undefined : e.currentTarget.value)}
          >
            <option value="">default</option>
            <option value="off">off</option>
            <option value="passthrough">passthrough</option>
          </select>
        {/if}
        {#if err}<span class="field-error" id={`${id}-err`}>{err}</span>{/if}
      </div>
    {/each}
  </div>
</fieldset>

<style>
  fieldset {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: 0;
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  legend,
  label {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  legend {
    padding: 0 var(--space-1);
    text-transform: uppercase;
  }
  .hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(12rem, 1fr));
    gap: var(--space-2) var(--space-3);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  input,
  select {
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  [aria-invalid='true'] {
    border-color: var(--danger);
  }
  .field-error {
    font-size: var(--fs-xs);
    color: var(--danger);
  }
</style>
