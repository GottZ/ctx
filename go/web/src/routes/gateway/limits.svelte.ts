// Limits panel state (RC-D C6, wave W05-12, design/05 §4.6 LimitsCard; wire =
// C1 gateway-limits-get|set|delete).
//
// Rules this model owns:
//   - load(consumerId) reads gateway-limits-get (all three ops) for the
//     consumer subject.
//   - save(op, values) writes the CALLER-tier row of one op as a full
//     replacement; clear(op) deletes it. No tier ever goes on the wire (the
//     server derives it). After a successful write the view is re-read from
//     the server (no client-side merge, C1: the write answers the stored row).
//   - A 400 with `param` puts the server's message on fieldErrors
//     ["<op>.<param>"] — the message stands at the field, not as a generic
//     toast — and rethrows; any other refusal lands on actionError and
//     rethrows.

import { toApiError, type ApiError } from '../../lib/api'
import { deleteGatewayLimits, getGatewayLimits, setGatewayLimits } from '../../lib/api/gatewaypanels'
import type { GatewayLimitsView, GatewayOp, LimitValues, LimitsOpView } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'
import { limitFieldErrors } from './limits-form'

interface LimitsApi {
  get: typeof getGatewayLimits
  set: typeof setGatewayLimits
  del: typeof deleteGatewayLimits
}

const DEFAULT_API: LimitsApi = { get: getGatewayLimits, set: setGatewayLimits, del: deleteGatewayLimits }

/** The ops in display order. */
export const LIMIT_OPS: readonly GatewayOp[] = ['chat', 'embed', 'rerank']

export class LimitsModel {
  view = $state<GatewayLimitsView | null>(null)
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** 400 field messages keyed "<op>.<field>". */
  fieldErrors = $state<Record<string, string>>({})
  actionError = $state<string | null>(null)
  busyOp = $state<GatewayOp | null>(null)

  readonly #api: LimitsApi
  #consumerId: string | null = null
  #gen = 0

  constructor(api: Partial<LimitsApi> = {}) {
    this.#api = { ...DEFAULT_API, ...api }
  }

  /** The ops the server answered, in display order. */
  get ops(): { op: GatewayOp; view: LimitsOpView }[] {
    const ops = this.view?.ops ?? {}
    return LIMIT_OPS.flatMap((op) => {
      const v = ops[op]
      return v === undefined ? [] : [{ op, view: v }]
    })
  }

  /** Read the consumer's limits. */
  async load(consumerId: string): Promise<void> {
    this.#consumerId = consumerId
    const gen = ++this.#gen
    if (this.status !== 'ready') this.status = 'loading'
    try {
      const res = await this.#api.get(consumerId)
      if (gen !== this.#gen) return
      this.view = res.limits
      this.loadError = null
      this.status = 'ready'
    } catch (err) {
      if (gen !== this.#gen) return
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  /** Write the caller-tier values of one op (full replacement of that row). */
  async save(op: GatewayOp, values: Partial<LimitValues>): Promise<void> {
    await this.#write(op, (id) => this.#api.set(id, op, values))
  }

  /** Delete the caller-tier row of one op (the consumer then inherits). */
  async clear(op: GatewayOp): Promise<void> {
    await this.#write(op, (id) => this.#api.del(id, op))
  }

  async #write(op: GatewayOp, call: (consumerId: string) => Promise<unknown>): Promise<void> {
    const id = this.#consumerId
    if (id === null) throw new Error('limits not loaded')
    this.busyOp = op
    this.actionError = null
    this.fieldErrors = Object.fromEntries(Object.entries(this.fieldErrors).filter(([k]) => !k.startsWith(`${op}.`)))
    try {
      await call(id)
    } catch (err) {
      const fields = limitFieldErrors(err, op)
      if (Object.keys(fields).length > 0) this.fieldErrors = { ...this.fieldErrors, ...fields }
      else this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyOp = null
    }
    await this.load(id)
  }
}
