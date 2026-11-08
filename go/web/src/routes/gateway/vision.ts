// FOLGE-Z Z5 — vision without an extra step, made visible (briefings/FOLGE-Z-INDEX.md
// §1 C-SPA, §3 contract; user 2026-11-08: "vision sollte keine extrasteps
// verlangen"). Pure and DOM-free (vision.test.ts).
//
//   - Catalog: an alias's `effective_vision` + `vision_source` (§3; Z2: an
//     unset policy.vision follows the backend probe of the chain) as one badge
//     on chat rows — images are a chat matter (policy key `vision` exists for
//     kind chat only, catalog.ts POLICY_KEYS). A server without the fields
//     reads "images: not reported"; a pair outside the contract keeps the
//     server's verdict but drops the claimed source.
//   - Policy form: the `vision` select's empty option says what the default
//     now means ("as the backend reports").
//   - Pool: a row's `capabilities.vision` (true / false / null = unknown) as a
//     badge, `probed_at` (+ the probe source) as its title; absent = unknown,
//     never "yes".

import type { BackendView, ModelAliasView } from '../../lib/api/types'

export interface VisionBadge {
  text: string
  tone: 'on' | 'off' | 'unknown'
  title: string
}

/** The (effective_vision, vision_source) pairs §3/Z2 can produce, with their words. */
const ALIAS_PAIRS: Readonly<Record<string, { source: string; title: string }>> = {
  'true|backend': { source: 'backend', title: 'policy.vision is unset; an enabled backend in the chain reports image understanding' },
  'true|policy': { source: 'policy', title: 'policy.vision=true forces images on for this alias' },
  'false|policy': { source: 'policy', title: 'policy.vision=false refuses images for this alias' },
  'false|none': { source: 'no image-capable backend', title: 'policy.vision is unset and no enabled backend in the chain reports image understanding' },
}

/** The catalog badge of an alias row; null for kinds without images (embedding, rerank). */
export function aliasVisionBadge(a: ModelAliasView): VisionBadge | null {
  if (a.kind !== 'chat') return null
  const v: unknown = a.effective_vision
  if (typeof v !== 'boolean') {
    return { text: 'images: not reported', tone: 'unknown', title: 'this server does not report whether the alias accepts images' }
  }
  const state = v ? 'on' : 'off'
  const pair = ALIAS_PAIRS[`${String(v)}|${String(a.vision_source)}`]
  if (pair === undefined) {
    return { text: `images: ${state} (source not reported)`, tone: v ? 'on' : 'off', title: `the server reports images ${state} without a known source` }
  }
  return { text: `images: ${state} (${pair.source})`, tone: v ? 'on' : 'off', title: pair.title }
}

/** The text of a policy select's empty option ("the server default"). */
export function policyDefaultLabel(key: string): string {
  return key === 'vision' ? 'default — as the backend reports' : 'default'
}

/** The pool row badge from capabilities (§3); absent capabilities = a server before FOLGE-Z. */
export function backendVisionBadge(b: BackendView): VisionBadge {
  const caps: unknown = b.capabilities
  if (caps === null || typeof caps !== 'object' || Array.isArray(caps)) {
    return { text: 'vision: unknown', tone: 'unknown', title: 'not reported by this server' }
  }
  const c = caps as Record<string, unknown>
  const probed = typeof c.probed_at === 'string' && c.probed_at !== '' ? `probed ${c.probed_at}` : 'never probed'
  const title = typeof c.source === 'string' && c.source !== '' ? `${probed} · ${c.source}` : probed
  if (c.vision === true) return { text: 'vision: yes', tone: 'on', title }
  if (c.vision === false) return { text: 'vision: no', tone: 'off', title }
  return { text: 'vision: unknown', tone: 'unknown', title }
}
