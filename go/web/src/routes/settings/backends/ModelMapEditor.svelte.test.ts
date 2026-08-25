// ModelMapEditor params-editing gates. Since the generic wire passthrough
// (#39) every model_map params key is meaningful (chat_template_kwargs,
// think, max_tokens, provider knobs), so the web editor edits params as a
// free JSON object per row instead of showing the old read-only "+params
// (edit via CLI)" badge.
//
// Layer 1 pins the pure draft→params projection (parseParamsDraft): only a
// JSON *object* is accepted, empty/{} clears the row's params, and anything
// invalid keeps the last valid state (ok:false). Layer 2 server-renders the
// component — mount() is unavailable in this env (see
// VaultForm.svelte.test.ts for the rationale), so the interactive open/edit
// path lives in the exported function and the render asserts the toggle
// markup replaces the old passive badge.

import { describe, expect, it } from 'vitest'
import { render } from 'svelte/server'
import ModelMapEditor, { parseParamsDraft } from './ModelMapEditor.svelte'

describe('parseParamsDraft', () => {
  it('accepts a JSON object verbatim', () => {
    const res = parseParamsDraft(
      '{"max_tokens":600,"think":false,"chat_template_kwargs":{"enable_thinking":false}}',
    )
    expect(res).toEqual({
      ok: true,
      params: {
        max_tokens: 600,
        think: false,
        chat_template_kwargs: { enable_thinking: false },
      },
    })
  })

  it('clears params on empty or {} drafts', () => {
    expect(parseParamsDraft('')).toEqual({ ok: true, params: undefined })
    expect(parseParamsDraft('   ')).toEqual({ ok: true, params: undefined })
    expect(parseParamsDraft('{}')).toEqual({ ok: true, params: undefined })
  })

  it('rejects non-object JSON and syntax errors without clearing', () => {
    for (const bad of ['[1,2]', '"str"', '42', 'null', '{"unterminated', '{think:false}']) {
      expect(parseParamsDraft(bad)).toEqual({ ok: false })
    }
  })
})

describe('ModelMapEditor markup', () => {
  it('renders a params toggle per row, marked when params are set', () => {
    const { body } = render(ModelMapEditor, {
      props: {
        rows: [
          { role: 'dream', model: 'nemotron', params: { max_tokens: 600 } },
          { role: 'default', model: 'qwen' },
        ],
        onchange: () => {},
      },
    })
    // Both rows offer editing; the row with params carries the filled marker,
    // and the old read-only "edit via CLI" badge is gone.
    expect(body.match(/params-toggle/g)?.length).toBeGreaterThanOrEqual(2)
    expect(body).toContain('params ●')
    expect(body).not.toContain('edit via CLI')
  })
})
