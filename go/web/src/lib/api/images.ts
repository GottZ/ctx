// Camo image-proxy client (design 07-camo §4.3, D2b). The markdown renderer has
// no signing secret, so it asks the server to sign the foreign image URLs it
// wants to proxy; the server returns a {url → proxy-path} map for the URLs it
// accepts. Non-proxiable URLs (relative, data:, mailto:) and — when the proxy is
// disabled — ALL URLs are simply absent from the map, and the caller keeps its
// placeholder for those. See markdown/camo.ts for the post-render swap.

import { apiFetch } from '../api'

/**
 * The origin of every sign request. Every markdown surface — chat assistant
 * turns, issue bodies, comments — is click-to-load, and a click signs exactly
 * one URL; the server accepts this origin only and signs one URL per request
 * (GHSA 2026-10-31 §7, security review §E). No URL with a query string is
 * signed.
 */
const SIGN_ORIGIN = 'assistant'

/** POST /api/img/sign response: the accepted subset of the requested URLs. */
interface SignResponse {
  signatures: Record<string, string>
}

/**
 * Sign foreign image URLs for the Camo proxy. Returns a map from each
 * accepted input URL to its `/api/img/<sig>?url=…&exp=…` proxy path. On ANY
 * failure — proxy disabled (404), rate limit (429), network — it resolves to an
 * empty map rather than throwing: a failed sign must degrade to the existing
 * placeholder, never break the render. De-duplicates and drops empty inputs.
 */
export async function signImages(urls: string[]): Promise<Record<string, string>> {
  const unique = [...new Set(urls.filter((u) => u))]
  if (unique.length === 0) return {}
  try {
    const res = await apiFetch<SignResponse>('/api/img/sign', {
      method: 'POST',
      body: JSON.stringify({ urls: unique, origin: SIGN_ORIGIN }),
    })
    return res.signatures ?? {}
  } catch {
    return {}
  }
}
