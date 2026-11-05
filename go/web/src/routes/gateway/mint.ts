// Pure logic of the /gateway mutations (design/05 §4.6 ConsumerCreateDialog,
// §4.5 field-tier UX, wave W05-06) — no DOM, no clock, no store, so vitest
// covers it in node (mint.test.ts). The dialog and the row only wire it up.
//
//   - offeredAliases mirrors modelalias.Offers (internal/modelalias/
//     registry.go): the aliases visible to the consumer's scope, a tenant
//     alias in place of the _global alias of the same name — shadowing is
//     decided by PRESENCE, enabled or not (winner) — and of those only the
//     enabled ones. The picker shows nothing else, so it never offers an alias
//     the server's create check refuses (V-0102-h). The residual the list
//     cannot mirror: model-alias-list reads the DB rows, the server judges
//     against its registry snapshot — a row the registry loaded as invalid
//     (listed, but the list carries no invalid flag), the moments after an
//     alias write before the registry reloads, and a failed reload. The server
//     then answers 400 `param: models` and the dialog shows it at the models
//     fieldset (e2e pinned). One source for picker and check would be a
//     server read action "offers for a scope" (handover in the W05-06 report).
//   - scopeOptions (Lead decision 1 of W05-06): a tenant-admin reads the
//     aliases of `_global` + its own HOME scope only (model_alias_manage.go
//     modelAliasReadScopes), so it is offered exactly that scope — for any
//     other scope of its tenant the picker could not know the shadowing
//     tenant aliases. A server-admin reads every alias and gets every scope of
//     the chosen tenant.
//   - The tier rules here are a UI MIRROR of the server (consumerPolicy,
//     consumerCreateRequest.grant); the server stays the gate.

import type { ConsumerCreateSpec, PromptSensitivity } from '../../lib/api/modelapi'
import type { ModelAliasView } from '../../lib/api/types'

/** The catalog scope every consumer sees (backends.GlobalScope). */
export const GLOBAL_SCOPE = '_global'

/** A tenant scope a consumer may carry: non-empty and not a reserved `_` name. */
function isTenantScope(scope: string | null | undefined): scope is string {
  return typeof scope === 'string' && scope !== '' && !scope.startsWith('_')
}

/**
 * The aliases a consumer with `scope` may be granted — modelalias.Offers over
 * the model-alias-list rows, sorted by name. `scope` null / '' / reserved ⇒
 * the _global catalog only.
 */
export function offeredAliases(aliases: readonly ModelAliasView[], scope: string | null): ModelAliasView[] {
  const tenantScope = isTenantScope(scope) ? scope : ''
  const own = new Map<string, ModelAliasView>()
  const global = new Map<string, ModelAliasView>()
  for (const a of aliases) {
    if (a.scope === GLOBAL_SCOPE) global.set(a.name, a)
    else if (tenantScope !== '' && a.scope === tenantScope) own.set(a.name, a)
  }
  const out: ModelAliasView[] = []
  for (const a of own.values()) if (a.enabled) out.push(a)
  for (const [name, a] of global) if (!own.has(name) && a.enabled) out.push(a)
  return out.sort((x, y) => (x.name < y.name ? -1 : x.name > y.name ? 1 : 0))
}

/** Scope options of the create dialog besides "none — global models only" (Lead decision 1). */
export function scopeOptions(opts: { serverAdmin: boolean; tenantScopes: readonly string[]; homeScope: string | null }): string[] {
  const valid = opts.tenantScopes.filter(isTenantScope)
  if (opts.serverAdmin) return valid
  return isTenantScope(opts.homeScope) && valid.includes(opts.homeScope) ? [opts.homeScope] : []
}

/** UI presets (design/05 §4.6, precedent routes/admin/provision.svelte.ts): kind-driven model preselection + 365 d. */
export const TEMPLATES: readonly { id: string; label: string; kind: string; expiryDays: number }[] = [
  { id: 'app-chat', label: 'App · Chat', kind: 'chat', expiryDays: 365 },
  { id: 'app-embedding', label: 'App · Embedding', kind: 'embedding', expiryDays: 365 },
]

/** The offered alias IDS of one kind — what a template preselects. */
export function templateModels(offered: readonly ModelAliasView[], kind: string): string[] {
  return offered.filter((a) => a.kind === kind).map((a) => a.id)
}

// --- expiry (design/01 E-01-4: gateway.key_max_lifetime_app is default AND ceiling, 0 = none) ---

export const EXPIRY_PRESET_DAYS = [30, 90, 365] as const
const DAY_S = 86_400

export type ExpiryChoice = { kind: 'days'; days: number } | { kind: 'date'; date: string } | { kind: 'never' }

export interface ExpiryOption {
  /** '30' | '90' | '365' | 'date' | 'never' */
  value: string
  label: string
  disabled: boolean
  note?: string
}

/**
 * The expiry radio options for a ceiling of `maxLifetimeS` seconds (0 = no
 * ceiling ⇒ "never expires" too; null = the server did not say — a pre-W05-01
 * whoami without model_api: every preset is offered and sent as a computed
 * expiry, "never" is not, the server judges).
 */
export function expiryOptions(maxLifetimeS: number | null): ExpiryOption[] {
  const out: ExpiryOption[] = EXPIRY_PRESET_DAYS.map((d) => {
    const over = maxLifetimeS !== null && maxLifetimeS > 0 && d * DAY_S > maxLifetimeS
    return { value: String(d), label: `${d} days`, disabled: over, ...(over ? { note: 'beyond the server maximum' } : {}) }
  })
  out.push({ value: 'date', label: 'on date', disabled: false })
  if (maxLifetimeS === 0) out.push({ value: 'never', label: 'never expires', disabled: false })
  return out
}

/** The largest preset within the ceiling — the dialog's default choice. */
export function defaultExpiry(maxLifetimeS: number | null): ExpiryChoice {
  const fit = EXPIRY_PRESET_DAYS.filter((d) => maxLifetimeS === null || maxLifetimeS === 0 || d * DAY_S <= maxLifetimeS)
  const days = fit.length > 0 ? fit[fit.length - 1] : undefined
  return days === undefined ? { kind: 'date', date: '' } : { kind: 'days', days }
}

/** A real calendar date as YYYY-MM-DD → its local parts; null otherwise. */
function calendarDate(s: string): [number, number, number] | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s)
  if (!m) return null
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])]
  const probe = new Date(y, mo - 1, d)
  return probe.getFullYear() === y && probe.getMonth() === mo - 1 && probe.getDate() === d ? [y, mo, d] : null
}

/**
 * The expiry part of the create spec. A preset equal to the ceiling sends
 * NOTHING — the server then sets exactly now + max at its own clock, so a
 * client clock running ahead can never push it over the ceiling (400). A
 * shorter preset rides as expires_in_days (turned into expires_at by the API
 * client); a date is the end of that local day; "never" only without a ceiling.
 */
export function expirySpec(
  choice: ExpiryChoice,
  maxLifetimeS: number | null,
): Pick<ConsumerCreateSpec, 'expires_at' | 'expires_in_days'> | { error: string } {
  if (choice.kind === 'never') {
    return maxLifetimeS === 0 ? {} : { error: 'this server requires an expiry (gateway.key_max_lifetime_app)' }
  }
  if (choice.kind === 'date') {
    const parts = calendarDate(choice.date)
    if (parts === null) return { error: 'pick an expiry date' }
    return { expires_at: new Date(parts[0], parts[1] - 1, parts[2], 23, 59, 59).toISOString() }
  }
  if (maxLifetimeS !== null && maxLifetimeS > 0 && choice.days * DAY_S > maxLifetimeS) {
    return { error: `${choice.days} days lie beyond the server maximum of ${Math.floor(maxLifetimeS / DAY_S)} days` }
  }
  if (maxLifetimeS !== null && maxLifetimeS > 0 && choice.days * DAY_S === maxLifetimeS) return {}
  return { expires_in_days: choice.days }
}

// --- network allowlist (client form check; the server normalises and decides) ---

function ipv4(s: string): number[] | null {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s)
  if (!m) return null
  const o = m.slice(1).map(Number)
  return o.every((x) => x <= 255) ? o : null
}

function ipv6(s: string): boolean {
  if (!/^[0-9a-fA-F:.]+$/.test(s) || s.split('::').length > 2) return false
  let groups = 0
  for (const half of s.split('::')) {
    if (half === '') continue
    const parts = half.split(':')
    for (let i = 0; i < parts.length; i++) {
      const p = parts[i]
      if (i === parts.length - 1 && p.includes('.')) {
        if (ipv4(p) === null) return false
        groups += 2
      } else if (/^[0-9a-fA-F]{1,4}$/.test(p)) groups++
      else return false
    }
  }
  return s.includes('::') ? groups < 8 : groups === 8
}

/** One CIDR prefix → null when fine, else the error line. */
function cidrError(entry: string): string | null {
  const slash = entry.indexOf('/')
  if (slash < 0) {
    if (ipv4(entry) !== null) return `"${entry}" needs a prefix length (e.g. ${entry}/32)`
    if (ipv6(entry)) return `"${entry}" needs a prefix length (e.g. ${entry}/128)`
    return `"${entry}" is not a CIDR prefix`
  }
  const addr = entry.slice(0, slash)
  const lenText = entry.slice(slash + 1)
  if (!/^\d{1,3}$/.test(lenText)) return `"${entry}" is not a CIDR prefix`
  const len = Number(lenText)
  const v4 = ipv4(addr)
  if (v4 !== null) {
    if (len > 32) return `"${entry}" is not a CIDR prefix`
    const value = ((v4[0] << 24) | (v4[1] << 16) | (v4[2] << 8) | v4[3]) >>> 0
    const mask = len === 0 ? 0 : (0xffffffff << (32 - len)) >>> 0
    if ((value & mask) >>> 0 !== value) {
      const n = (value & mask) >>> 0
      return `"${entry}" has host bits set (use ${[n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join('.')}/${len})`
    }
    return null
  }
  if (ipv6(addr)) return len > 128 ? `"${entry}" is not a CIDR prefix` : null
  return `"${entry}" is not a CIDR prefix`
}

/**
 * Parse the allowlist field: prefixes split by comma, space or newline.
 * Empty ⇒ cidrs null (any source). Any malformed entry ⇒ cidrs null + the
 * error lines. Host bits are checked for IPv4 here; IPv6 host bits and the
 * canonical spelling are the server's (netip.Prefix, 400 with the masked form).
 */
export function parseCidrs(text: string): { cidrs: string[] | null; errors: string[] } {
  const entries = text.split(/[\s,]+/).filter((e) => e !== '')
  if (entries.length === 0) return { cidrs: null, errors: [] }
  const errors = entries.map(cidrError).filter((e): e is string => e !== null)
  return errors.length > 0 ? { cidrs: null, errors } : { cidrs: entries, errors: [] }
}

// --- prompt sensitivity (field tier: below server-admin only the default `credentials`) ---

export const SENSITIVITIES: readonly PromptSensitivity[] = ['credentials', 'personal', 'internal', 'public']

export function sensitivityOptions(serverAdmin: boolean): { value: PromptSensitivity; disabled: boolean; note?: string }[] {
  return SENSITIVITIES.map((value) =>
    serverAdmin || value === 'credentials' ? { value, disabled: false } : { value, disabled: true, note: 'server-admin only' },
  )
}

// --- the create spec ---

export interface MintFormInput {
  label: string
  description: string
  /** server-admin: the chosen target tenant; null = the caller's own (no tenant_id sent). */
  tenantId: string | null
  /** null = "none — global models only". */
  scope: string | null
  allModels: boolean
  /** Picked alias ids (may hold stale picks — only the offered ones are sent). */
  models: readonly string[]
  /** offeredAliases for `scope`. */
  offered: readonly ModelAliasView[]
  expiry: ExpiryChoice
  /** whoami model_api.key_max_lifetime_s; null = not sent by the server. */
  maxLifetimeS: number | null
  sensitivity: PromptSensitivity
  cidrsText: string
}

/** Build the model-consumer-create spec from the dialog fields — or the first client-side error. */
export function buildCreateSpec(f: MintFormInput): { spec: ConsumerCreateSpec } | { error: string } {
  const label = f.label.trim()
  if (label === '') return { error: 'label is required' }
  if ([...label].length > 100) return { error: 'label must be at most 100 characters' }
  const description = f.description.trim()
  if ([...description].length > 1000) return { error: 'description must be at most 1000 characters' }
  const cidrs = parseCidrs(f.cidrsText)
  if (cidrs.errors.length > 0) return { error: cidrs.errors.join('; ') }
  const expiry = expirySpec(f.expiry, f.maxLifetimeS)
  if ('error' in expiry) return { error: expiry.error }

  const spec: ConsumerCreateSpec = { label, prompt_sensitivity: f.sensitivity }
  if (description !== '') spec.description = description
  if (f.tenantId) spec.tenant_id = f.tenantId
  if (isTenantScope(f.scope)) spec.tenant_scope = f.scope
  if (f.allModels) spec.all_models = true
  else {
    const offered = new Set(f.offered.map((a) => a.id))
    const models = [...new Set(f.models)].filter((id) => offered.has(id)).sort()
    if (models.length > 0) spec.models = models
  }
  if (cidrs.cidrs !== null) spec.allowed_cidrs = cidrs.cidrs
  Object.assign(spec, expiry)
  return { spec }
}
