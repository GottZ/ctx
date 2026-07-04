// {@html}-Zähl-Meta-Test (design 06 §5.6a, wave PV7; extended U06) — the
// mechanical convention guard: the ENTIRE frontend owns a FROZEN, enumerated set
// of {@html} sinks, each fed exclusively by renderMarkdown (src/lib/markdown —
// markdown-it html:false + DOMPurify + ctx: rewrite).
//
//   - src/lib/markdown/Markdown.svelte — the shared sanitized renderer used by
//     the chat assistant turn, the issue body AND every issue comment (design 04
//     §9.6: one markdown path, not many). U06 introduced this ONE sink so the
//     issue-detail surface renders foreign markdown WITHOUT a second raw sink.
//   - src/routes/chat/MessageBubble.svelte — the chat turn (kept as-is; it wraps
//     the streaming cursor around the same renderMarkdown output).
//
// Why a frozen COUNT and not a lint rule: Achse 03 renders FOREIGN text
// (GitHub issue comments) and MUST take the same renderMarkdown path; a second
// UNSANCTIONED sink added anywhere turns this test red BEFORE a review can
// overlook it. Growing the freeze is a conscious decision (as U06 did): the new
// sink must consume renderMarkdown, and the expected map below is extended in the
// same commit — never as a side effect.
//
// Lives beside the other vitest meta-gates (coverage/a11y) under e2e/contract/
// — src/ is svelte-checked with the app tsconfig (types: svelte + vite/client)
// which rejects node:fs, exactly like the axe.ts JSON-import precedent.
//
// The regex requires whitespace after `{@html` so prose mentions of the
// directive in comments ("never {@html} — repo convention") do not count —
// only a real directive usage `{@html expr}` matches.

import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const SRC_ROOT = fileURLToPath(new URL('../../src', import.meta.url))
const SINK = /\{@html\s/g

/** All *.svelte files under src/, as src-relative POSIX paths. */
function svelteFiles(dir: string, prefix = ''): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const rel = prefix === '' ? entry.name : `${prefix}/${entry.name}`
    if (entry.isDirectory()) out.push(...svelteFiles(join(dir, entry.name), rel))
    else if (entry.name.endsWith('.svelte')) out.push(rel)
  }
  return out
}

describe('{@html} sink freeze (design 06 §5.6a)', () => {
  it('exactly the frozen sinks exist, each rendering through renderMarkdown', () => {
    const found: Record<string, number> = {}
    for (const rel of svelteFiles(SRC_ROOT).sort()) {
      const n = (readFileSync(join(SRC_ROOT, rel), 'utf8').match(SINK) ?? []).length
      if (n > 0) found[rel] = n
    }
    expect(
      found,
      'the {@html} sink set changed — every sink MUST render through lib/markdown renderMarkdown ' +
        '(html:false + DOMPurify + ctx: rewrite, design 06 §5.6a); extend this freeze only together ' +
        'with that proof, never as a side effect',
    ).toEqual({ 'lib/markdown/Markdown.svelte': 1, 'routes/chat/MessageBubble.svelte': 1 })
  })
})
