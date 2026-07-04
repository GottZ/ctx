// URL query parse for the workflow surface (design 04 §4 URL-state / §4.1.5,
// wave U04). `?scope=<repo-scope>` is the FIRST url-carried filter: it is what
// makes an issue-list / board deep link reproducible across many repos (without
// it a shared link would land on the wrong project). U04 ships the PARSE only —
// the ProjectPicker UI + the write-back to the URL land in U05. Kept out of the
// component as a pure function so vitest asserts it in a plain node environment.

/**
 * The `scope` filter from a `location.search` string, or `null` when the param
 * is absent or blank. Never throws on a malformed query (URLSearchParams is
 * lenient); a whitespace-only value is treated as absent.
 */
export function parseScopeParam(search: string): string | null {
  const raw = new URLSearchParams(search).get('scope')
  const scope = raw?.trim()
  return scope ? scope : null
}
