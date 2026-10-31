// Camo image-proxy client (design 07-camo §4.3, D2b). The markdown renderer has
// no signing secret, so it asks the server to sign the foreign image URLs it
// wants to proxy; the server returns a {url → proxy-path} map for the URLs it
// accepts. Non-proxiable URLs (relative, data:, mailto:) and — when the proxy is
// disabled — ALL URLs are simply absent from the map, and the caller keeps its
// placeholder for those. See markdown/camo.ts for the post-render swap.

import { apiFetch } from '../api'

/**
 * The kind of rendered content a sign request comes from. The server signs
 * "assistant" (LLM output, click-to-load) one URL at a time; "issue" content
 * may batch. No origin signs a URL with a query string (GHSA 2026-10-31 §7).
 */
export type CamoOrigin = 'assistant' | 'issue'

/** POST /api/img/sign response: the accepted subset of the requested URLs. */
interface SignResponse {
  signatures: Record<string, string>
}

/**
 * Batch-sign foreign image URLs for the Camo proxy. Returns a map from each
 * accepted input URL to its `/api/img/<sig>?url=…&exp=…` proxy path. On ANY
 * failure — proxy disabled (404), rate limit (429), network — it resolves to an
 * empty map rather than throwing: a failed sign must degrade to the existing
 * placeholder, never break the render. De-duplicates and drops empty inputs.
 */
export async function signImages(urls: string[], origin: CamoOrigin): Promise<Record<string, string>> {
  const unique = [...new Set(urls.filter((u) => u))]
  if (unique.length === 0) return {}
  try {
    const res = await apiFetch<SignResponse>('/api/img/sign', {
      method: 'POST',
      body: JSON.stringify({ urls: unique, origin }),
    })
    return res.signatures ?? {}
  } catch {
    return {}
  }
}
