// Vault reference-rendering gates (04-W5). Since the sealbox guard unions the
// settings scan with the backend pool, GET /api/secrets ships BOTH reference
// types in one flat referenced_by, backend rows carrying a "backend:" prefix.
// Two things had to follow, and both are pinned here:
//
//   1. the form renders the SERVER's list, split by type — the old client-side
//      api_key_ref join showed the same reference a second time under its own
//      label, so a pool-referenced secret read as two references
//   2. the delete-confirm text follows the reference TYPE and states the real
//      outcome: a backend reference 409s exactly like a settings one (it used
//      to promise "leaves a dead ref", which stopped being true with the guard)
//
// Layer 1 is the pure deleteConfirmText projection; layer 2 server-renders the
// component and asserts its markup — mount() is unavailable in this env (see
// lib/ui/ConnState.svelte.test.ts for the rationale). The confirm branch is
// behind a click and therefore unreachable to a server render; that is why the
// text lives in an exported function rather than inline in the template.

import { describe, expect, it } from 'vitest'
import { render } from 'svelte/server'
import type { SecretMeta } from '../../../lib/api/types'
import type { SecretUsage } from '../../../lib/backends'
import VaultForm, { deleteConfirmText } from './VaultForm.svelte'
import { VaultModel } from './vault.svelte'

const NO_USAGE: SecretUsage = { dangling: [] }

function secret(name: string, referencedBy: string[]): SecretMeta {
  return { name, key_version: 1, created_at: '2026-08-21T03:00:00Z', referenced_by: referencedBy }
}

/** Server-render the vault over a fixed secret list. The model is seeded
 *  directly — no load() runs, so its API binding is never touched. */
function html(secrets: SecretMeta[], usage: SecretUsage = NO_USAGE): string {
  const vault = new VaultModel()
  vault.secrets = secrets
  return render(VaultForm, { props: { vault, usage } }).body
}

/** Readable text: tags and comments stripped, whitespace collapsed. */
function textOf(markup: string): string {
  return markup
    .replace(/<[^>]*>/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
}

function occurrences(haystack: string, needle: string): number {
  return haystack.split(needle).length - 1
}

describe('deleteConfirmText', () => {
  it('names settings when only settings reference the secret', () => {
    expect(deleteConfirmText({ settings: ['chat.api_key'], backends: [] })).toBe(
      'blocked by settings — delete will 409',
    )
  })

  it('names the backend rows — and promises a 409, not a dead ref', () => {
    const text = deleteConfirmText({ settings: [], backends: ['openrouter', 'spark-chat'] })
    expect(text).toBe('blocked by backend openrouter, spark-chat — delete will 409')
    expect(text).not.toContain('dead ref')
  })

  it('names both types when both reference it', () => {
    expect(deleteConfirmText({ settings: ['chat.api_key'], backends: ['openrouter'] })).toBe(
      'blocked by settings and backends — delete will 409',
    )
  })

  it('asks plainly when nothing references it', () => {
    expect(deleteConfirmText({ settings: [], backends: [] })).toBe('delete?')
  })

  it('never claims a settings blocker for a pool-only reference', () => {
    // The old text said "blocked by settings" for every referenced secret —
    // wrong remediation, and the wrong endpoint to send an operator to.
    expect(deleteConfirmText({ settings: [], backends: ['openrouter'] })).not.toContain('settings')
  })
})

describe('VaultForm reference rendering', () => {
  it('splits one referenced_by into settings and backend refs', () => {
    const text = textOf(html([secret('or.key', ['chat.api_key', 'backend:openrouter'])]))
    expect(text).toContain('setting chat.api_key')
    expect(text).toContain('backend openrouter')
    // The prefix is consumed by the split, never rendered raw.
    expect(text).not.toContain('backend:openrouter')
  })

  it('shows a pool reference EXACTLY once (no client-join double entry)', () => {
    const text = textOf(html([secret('or.key', ['backend:openrouter'])]))
    expect(occurrences(text, 'openrouter')).toBe(1)
  })

  it('renders "no references" only for a truly unreferenced secret', () => {
    expect(textOf(html([secret('lonely', [])]))).toContain('no references')
    expect(textOf(html([secret('or.key', ['backend:openrouter'])]))).not.toContain('no references')
  })

  it('still lists dangling refs from the client scan', () => {
    const usage: SecretUsage = {
      dangling: [{ source: 'backend', ref: 'router', secret: 'gone' }],
    }
    const text = textOf(html([secret('or.key', [])], usage))
    expect(text).toContain('backend router')
    expect(text).toContain('gone')
  })
})
