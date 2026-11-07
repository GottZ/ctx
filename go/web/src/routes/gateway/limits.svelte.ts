// Limits panel state (RC-D C6, wave W05-12, design/05 §4.6 LimitsCard,
// design/06 §4.3 gateway-limits-get|set|clear).
//
// Rules this model owns:
//   - load(consumerId) reads gateway-limits-get (all ops).
//   - save(op, values) / clear(op, fields?) write the CALLER-tier row; no
//     tier ever goes on the wire (the server derives it). A 422 puts the
//     server's fields[] messages on fieldErrors["<op>.<field>"] — the message
//     stands at the field, not as a generic toast — and rethrows; any other
//     refusal lands on actionError.
//   - After a successful write the op's view is replaced by the server's
//     answer (no client-side merge).
//
// RC-D S1: contract stub — load() ends in 'error' (503); save/clear reject
// with the 503 and set nothing.

import { ApiError } from '../../lib/api'
import { clearGatewayLimits, getGatewayLimits, setGatewayLimits } from '../../lib/api/gatewaypanels'
import type { GatewayOp, LimitDimension, LimitValues, LimitsOpView } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

interface LimitsApi {
  get: typeof getGatewayLimits
  set: typeof setGatewayLimits
  clear: typeof clearGatewayLimits
}

const DEFAULT_API: LimitsApi = { get: getGatewayLimits, set: setGatewayLimits, clear: clearGatewayLimits }

export class LimitsModel {
  ops = $state<LimitsOpView[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** 422 field messages keyed "<op>.<field>". */
  fieldErrors = $state<Record<string, string>>({})
  actionError = $state<string | null>(null)
  busyOp = $state<GatewayOp | null>(null)

  readonly #api: LimitsApi
  #consumerId: string | null = null

  constructor(api: Partial<LimitsApi> = {}) {
    this.#api = { ...DEFAULT_API, ...api }
  }

  /** Read the consumer's limits. */
  async load(consumerId: string): Promise<void> {
    this.#consumerId = consumerId
    void this.#api
    this.status = 'error'
    this.loadError = new ApiError(503, 'service_unavailable', 'limits panel: not implemented (RC-D S1 stub)')
  }

  /** Write the caller-tier values of one op. */
  async save(op: GatewayOp, values: Partial<LimitValues>): Promise<void> {
    void op
    void values
    void this.#consumerId
    throw new ApiError(503, 'service_unavailable', 'gateway-limits-set: not implemented (RC-D S1 stub)')
  }

  /** Clear the caller-tier row (or some fields) of one op. */
  async clear(op: GatewayOp, fields?: LimitDimension[]): Promise<void> {
    void op
    void fields
    throw new ApiError(503, 'service_unavailable', 'gateway-limits-clear: not implemented (RC-D S1 stub)')
  }
}
