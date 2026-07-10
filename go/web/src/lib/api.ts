// Typed fetch wrapper (design 04-§2.4; Cookie-Session seit OAuth R4, 05-W5):
// die httpOnly-Session-Cookies fahren same-origin automatisch mit, state-
// changing Methoden tragen den in-memory CSRF-Synchronizer als X-CSRF-Token
// (design 05 §4.4). Jede Fehlerform wird zu ApiError normalisiert und trägt
// die X-Request-ID, damit Browser-Fehler in den ctxd-Logs greppbar bleiben.
// Ein 401 auf einem Session-Request bekommt EINEN stillen POST /auth/refresh
// + Replay, bevor der unauthorized-Hook zur Login-Maske abräumt.

/** Normalized API failure. `status` 0 means the request never got a response. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string | null
  /**
   * The full `{success:false, …}` envelope on a server error — carries the
   * fields beyond `error`, e.g. the 422 `fields` array (per-field validation)
   * and the 409 `referenced_by` list. null for network/parse failures.
   */
  readonly details: Record<string, unknown> | null

  constructor(
    status: number,
    code: string,
    message: string,
    requestId: string | null = null,
    details: Record<string, unknown> | null = null,
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.details = details
  }
}

/** Wrap any thrown value; ApiError instances pass through unchanged. */
export function toApiError(err: unknown): ApiError {
  if (err instanceof ApiError) return err
  return new ApiError(0, 'internal', err instanceof Error ? err.message : String(err))
}

interface ApiHooks {
  /** Liefert den per-Session CSRF-Synchronizer (X-CSRF-Token bei Mutationen). */
  getCsrfToken: () => string | null
  /** Fired when the cookie session is dead (401 nach gescheitertem Refresh). */
  onUnauthorized: () => void
}

const hooks: ApiHooks = {
  getCsrfToken: () => null,
  onUnauthorized: () => {},
}

/** Wire the session into the client (called once by auth.svelte.ts). */
export function configureApi(next: Partial<ApiHooks>): void {
  Object.assign(hooks, next)
}

export interface ApiFetchOptions {
  /**
   * Explizites Bearer-Key-Override (Key-Probe-Pfade). Ein 401 mit explizitem
   * Key macht weder Refresh noch unauthorized-Hook — der Caller besitzt den
   * Fehler. Header-Credentials sind NIE CSRF-gepflichtig (design 05 §4.4).
   */
  key?: string
  /**
   * Caller besitzt den 401 (kein Refresh-Replay, kein unauthorized-Hook) —
   * die Session-Lifecycle-Calls (login/restore-Probe/logout) setzen das.
   */
  skipRefresh?: boolean
}

/** Cookie-Requests dieser Methoden tragen den X-CSRF-Token-Header (§4.4). */
const csrfMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

// Single-flight-Refresh: konkurrierende 401s teilen sich EINEN POST
// /auth/refresh — die Rotation ist single-use pro Refresh-Token, eine
// parallele Zweit-Rotation würde 03s Reuse-Detection auslösen und die
// ganze Familie killen.
let refreshInflight: Promise<boolean> | null = null

/** POST /auth/refresh (Cookie-authentifiziert). true ⇔ Session rotiert. */
export function refreshSession(): Promise<boolean> {
  refreshInflight ??= (async () => {
    try {
      const res = await fetch('/auth/refresh', { method: 'POST' })
      return res.ok
    } catch {
      return false
    } finally {
      refreshInflight = null
    }
  })()
  return refreshInflight
}

/**
 * Fetch a JSON API endpoint. Throws ApiError for network failures, non-2xx
 * statuses and `{success:false}` envelopes — including those inside an
 * HTTP 200 (heartbeat-path semantics of POST /api/query, design 04-§2.4).
 */
export async function apiFetch<T>(
  path: string,
  init: RequestInit = {},
  opts: ApiFetchOptions = {},
): Promise<T> {
  const headers = new Headers(init.headers)
  if (opts.key !== undefined) {
    if (opts.key !== '') headers.set('Authorization', `Bearer ${opts.key}`)
  } else {
    // Cookie-Pfad: Mutationen tragen den Synchronizer; GET braucht nichts.
    const csrf = hooks.getCsrfToken()
    const method = (init.method ?? 'GET').toUpperCase()
    if (csrf !== null && csrfMethods.has(method)) headers.set('X-CSRF-Token', csrf)
  }
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  // manual = der Caller behandelt den 401 selbst (Probe-/Lifecycle-Pfade).
  const manual = opts.key !== undefined || opts.skipRefresh === true

  let retried = false
  for (;;) {
    let res: Response
    try {
      res = await fetch(path, { ...init, headers })
    } catch (cause) {
      const detail = cause instanceof Error ? cause.message : String(cause)
      throw new ApiError(0, 'network', `ctxd unreachable: ${detail}`)
    }

    const requestId = res.headers.get('X-Request-ID')
    const body = await parseBody(res)
    const serverError = envelopeError(body)
    const details = asRecord(body)

    if (res.status === 401) {
      if (!manual && !retried && (await refreshSession())) {
        retried = true // Cookies wurden unter uns rotiert — EIN stilles Replay
        continue
      }
      if (!manual) hooks.onUnauthorized()
      throw new ApiError(401, 'unauthorized', serverError ?? 'invalid or expired session', requestId, details)
    }
    if (!res.ok) {
      const message = serverError ?? `request failed (HTTP ${res.status})`
      throw new ApiError(res.status, codeFor(res.status), message, requestId, details)
    }
    if (serverError !== null) {
      // success:false inside a 2xx body — surfaced as an error, never as data.
      throw new ApiError(res.status, 'api_error', serverError, requestId, details)
    }
    if (body === undefined) {
      throw new ApiError(res.status, 'invalid_response', 'response was not valid JSON', requestId)
    }
    return body as T
  }
}

/**
 * Parse the body as JSON, tolerating the leading-whitespace keepalive ticks
 * of the heartbeat path (RFC 8259 allows them; JSON.parse skips whitespace).
 * Returns undefined for empty or non-JSON bodies.
 */
async function parseBody(res: Response): Promise<unknown> {
  let text: string
  try {
    text = await res.text()
  } catch {
    return undefined
  }
  if (text.trim() === '') return undefined
  try {
    return JSON.parse(text) as unknown
  } catch {
    return undefined
  }
}

/** A parsed JSON object body, else null (kept on ApiError.details). */
function asRecord(body: unknown): Record<string, unknown> | null {
  return typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : null
}

/** Extract the error of a `{success:false, error}` envelope, else null. */
function envelopeError(body: unknown): string | null {
  if (typeof body !== 'object' || body === null) return null
  const envelope = body as { success?: unknown; error?: unknown }
  if (envelope.success !== false) return null
  return typeof envelope.error === 'string' && envelope.error !== '' ? envelope.error : 'request failed'
}

/** Stable machine class per HTTP status (ApiError.code). */
export function codeFor(status: number): string {
  switch (status) {
    case 400:
      return 'bad_request'
    case 403:
      return 'forbidden'
    case 404:
      return 'not_found'
    case 409:
      return 'conflict'
    case 422:
      return 'validation'
    case 429:
      return 'rate_limited'
    default:
      return status >= 500 ? 'server' : `http_${status}`
  }
}
