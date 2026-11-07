<script lang="ts">
  // Limits card of one consumer (RC-D C6, wave W05-12; design/05 §4.5/§4.6;
  // wire = C1 gateway-limits-get|set|delete): per op the stored server row,
  // the tenant row, the ceiling and the effective values, and the form of the
  // caller's tier (limits-form.ts limitFieldSpec): a tenant-admin's input
  // maximum is the ceiling, there is no "unlimited" control for anyone, an
  // empty field reads "inherits …"; a 400 with `param` shows its message at
  // that field. Save = full replacement of the caller-tier row (empty fields
  // go out as null); "Remove my row" = gateway-limits-delete. The tier is the
  // server's (never sent). Below tenant-admin the area is unreachable (guard).
  import { untrack } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import type { GatewayOp, LimitDimension, LimitTier, LimitValues, LimitsOpView } from '../../lib/api/types'
  import { LimitsModel } from './limits.svelte'
  import { LIMIT_DIMENSIONS, fmtLimit, limitFieldSpec, tenantInputError, unsendableInput } from './limits-form'

  let { consumerId }: { consumerId: string } = $props()

  const model = new LimitsModel()
  const tier = $derived<LimitTier>(session.caps.tier === 'server-admin' ? 'server' : 'tenant')

  $effect(() => {
    const id = consumerId
    untrack(() => void model.load(id))
  })

  /** The form drafts per "<op>.<dim>" (raw input text) and the allow_external choice per op. */
  let drafts = $state<Record<string, string>>({})
  let external = $state<Record<string, '' | 'allow' | 'deny'>>({})
  let saved = $state<GatewayOp | null>(null)

  function rowOf(view: LimitsOpView): LimitValues | null {
    return tier === 'server' ? view.server : view.tenant
  }

  function draftOf(op: GatewayOp, view: LimitsOpView, dim: LimitDimension): string {
    const k = `${op}.${dim}`
    if (k in drafts) return drafts[k] ?? ''
    const v = rowOf(view)?.[dim]
    return v === null || v === undefined ? '' : String(v)
  }

  function externalOf(op: GatewayOp, view: LimitsOpView): '' | 'allow' | 'deny' {
    if (op in external) return external[op] ?? ''
    const v = rowOf(view)?.allow_external
    return v === true ? 'allow' : v === false ? 'deny' : ''
  }

  function clientErrors(op: GatewayOp, view: LimitsOpView): Record<string, string> {
    const out: Record<string, string> = {}
    if (tier !== 'tenant') return out
    for (const d of LIMIT_DIMENSIONS) {
      const e = tenantInputError(view, d, draftOf(op, view, d))
      if (e !== null) out[`${op}.${d}`] = e
    }
    return out
  }

  /** Inputs the wire cannot carry (non-finite) — they block the submit (I4). */
  let unsendable = $state<Record<string, string>>({})

  async function save(op: GatewayOp, view: LimitsOpView): Promise<void> {
    saved = null
    const bad: Record<string, string> = {}
    for (const d of LIMIT_DIMENSIONS) {
      if (unsendableInput(draftOf(op, view, d))) bad[`${op}.${d}`] = 'not a finite number'
    }
    unsendable = bad
    if (Object.keys(bad).length > 0) return
    const values: Partial<LimitValues> = {}
    for (const d of LIMIT_DIMENSIONS) {
      const raw = draftOf(op, view, d).trim()
      values[d] = raw === '' ? null : Number(raw)
    }
    const ext = externalOf(op, view)
    values.allow_external = ext === 'allow' ? true : ext === 'deny' ? false : null
    // the server row's unmetered flag is kept as stored (this form does not edit it)
    values.allow_unmetered_external = tier === 'server' ? (view.server?.allow_unmetered_external ?? null) : null
    try {
      await model.save(op, values)
      for (const d of LIMIT_DIMENSIONS) delete drafts[`${op}.${d}`]
      delete external[op]
      saved = op
    } catch {
      /* the field error / action error is on the model */
    }
  }

  async function remove(op: GatewayOp): Promise<void> {
    saved = null
    try {
      await model.clear(op)
      for (const d of LIMIT_DIMENSIONS) delete drafts[`${op}.${d}`]
      delete external[op]
    } catch {
      /* on the model */
    }
  }

  function effText(view: LimitsOpView, d: LimitDimension): string {
    const e = view.effective
    if (e === undefined) return '—'
    const v = e[d]
    // null in an effective budget is the server's "no limit" (settings floor 0, C1) — never "none"/"0"
    return v === null ? 'no limit' : fmtLimit(d, v)
  }
</script>

<section class="card" aria-label="limits">
  <header class="card-head">
    <h2>limits</h2>
    <span class="count">{tier === 'server' ? 'server tier' : 'tenant tier — narrowing only'}</span>
  </header>
  {#if model.status === 'idle' || model.status === 'loading'}
    <p class="state" aria-busy="true">loading limits…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert">
      <p>{model.loadError?.message ?? 'limits unavailable'}</p>
      <button type="button" onclick={() => void model.load(consumerId)}>Retry</button>
    </div>
  {:else}
    {#if model.actionError}<p class="action-error" role="alert">{model.actionError}</p>{/if}
    {#each model.ops as { op, view } (op)}
      {@const local = clientErrors(op, view)}
      <!-- novalidate: the max attribute is the ceiling hint (and the spinbutton's
           bound), not a submit blocker — the server decides and its 400 stands
           at the field -->
      <form
        class="op"
        aria-label="{op} limits"
        novalidate
        onsubmit={(e) => {
          // the client check is a hint, the server decides (design/05 §3.3:
          // client gates are UX) — its 400 then stands at the field instead
          e.preventDefault()
          void save(op, view)
        }}
      >
        <h3>{op}</h3>
        <div class="grid">
          {#each LIMIT_DIMENSIONS as d (d)}
            {@const spec = limitFieldSpec(view, d, tier)}
            {@const key = `${op}.${d}`}
            {@const err = unsendable[key] ?? model.fieldErrors[key] ?? local[key]}
            <div class="field">
              <label for="lim-{consumerId}-{key}">{op} {d}</label>
              <input
                id="lim-{consumerId}-{key}"
                type="number"
                min={d === 'monthly_cost_usd' ? '0.00000001' : '1'}
                step={d === 'monthly_cost_usd' ? 'any' : '1'}
                max={spec.max === null ? undefined : String(spec.max)}
                disabled={spec.disabled || model.busyOp === op}
                placeholder={spec.emptyMeans}
                aria-invalid={err !== undefined}
                aria-describedby="lim-{consumerId}-{key}-eff"
                value={draftOf(op, view, d)}
                oninput={(e) => (drafts[key] = e.currentTarget.value)}
              />
              <span class="eff" id="lim-{consumerId}-{key}-eff">
                effective {effText(view, d)}{#if tier === 'tenant' && spec.max !== null}
                  · max {fmtLimit(d, spec.max)}{/if}
              </span>
              {#if err !== undefined}<span class="field-error" data-field-error={key} role="alert">{err}</span>{/if}
            </div>
          {/each}
          <div class="field">
            <label for="lim-{consumerId}-{op}.allow_external">{op} external backends</label>
            <select
              id="lim-{consumerId}-{op}.allow_external"
              disabled={model.busyOp === op}
              value={externalOf(op, view)}
              onchange={(e) => (external[op] = e.currentTarget.value as '' | 'allow' | 'deny')}
            >
              <option value="">inherit</option>
              {#if tier === 'server'}<option value="allow">allow</option>{/if}
              <option value="deny">deny</option>
            </select>
            {#if model.fieldErrors[`${op}.allow_external`]}
              <span class="field-error" data-field-error="{op}.allow_external" role="alert"
                >{model.fieldErrors[`${op}.allow_external`]}</span
              >
            {/if}
          </div>
        </div>
        <div class="actions">
          <button type="submit" class="primary" disabled={model.busyOp === op}>Save {op} limits</button>
          {#if rowOf(view) !== null}
            <button type="button" disabled={model.busyOp === op} onclick={() => void remove(op)}
              >Remove my {op} row</button
            >
          {/if}
          {#if saved === op}<span class="ok" role="status">saved</span>{/if}
        </div>
      </form>
    {/each}
  {/if}
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .card-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2,
  h3 {
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
  .state {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    margin: var(--space-2) var(--space-3);
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p,
  .action-error,
  .field-error {
    margin: 0;
    color: var(--danger);
    font-size: var(--fs-xs);
  }
  .action-error {
    padding: var(--space-2) var(--space-3);
  }
  .op {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(12rem, 1fr));
    gap: var(--space-2);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--fs-sm);
  }
  .field label {
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .eff {
    color: var(--text-faint);
    font-size: var(--fs-xs);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
  }
  .ok {
    color: var(--ok);
    font-size: var(--fs-sm);
  }
</style>
