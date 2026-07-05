// Render-transform tests (design 06 §3.9). buildRenderItems turns a persisted
// message history into the flat render list: user/assistant bubbles, with an
// assistant turn's tool calls hoisted to cards paired with their results.

import { describe, expect, it } from 'vitest'
import { buildRenderItems } from './render'
import type { ChatMessage } from './types'

const userMsg = (seq: number, content: string): ChatMessage => ({ seq, role: 'user', content, sensitivity: 'personal', created_at: '' })

describe('buildRenderItems', () => {
  it('pairs an assistant tool call with its persisted result and keeps the final answer', () => {
    const messages: ChatMessage[] = [
      userMsg(1, 'what is the dream backoff?'),
      {
        seq: 2,
        role: 'assistant',
        content: '',
        sensitivity: 'personal',
        created_at: '',
        tool_calls: [{ id: 'c1', type: 'function', function: { name: 'ctx_query', arguments: '{"query":"dream backoff"}' } }],
      },
      {
        seq: 3,
        role: 'tool',
        content: '{"count":2,"blocks":[{"id":"b1","title":"W49c","category":"learnings","score":0.9},{"id":"b2","title":"X","category":"decisions","score":0.5}]}',
        sensitivity: 'internal',
        tool_call_id: 'c1',
        tool_name: 'ctx_query',
        duration_ms: 420,
        created_at: '',
      },
      { seq: 4, role: 'assistant', content: 'It is exponential.', sensitivity: 'personal', created_at: '' },
    ]

    const items = buildRenderItems(messages)
    expect(items.map((i) => i.kind)).toEqual(['user', 'tool', 'assistant'])

    const tool = items[1]
    if (tool.kind !== 'tool') throw new Error('expected tool item')
    expect(tool.name).toBe('ctx_query')
    expect(tool.args).toEqual({ query: 'dream backoff' })
    expect(tool.result?.ok).toBe(true)
    expect(tool.result?.summary).toBe('2 blocks')
    expect(tool.result?.blocks?.map((b) => b.title)).toEqual(['W49c', 'X'])
    expect(tool.result?.duration_ms).toBe(420)

    const ans = items[2]
    if (ans.kind !== 'assistant') throw new Error('expected assistant item')
    expect(ans.content).toBe('It is exponential.')
  })

  it('marks a canceled partial and renders an errored tool result as not-ok', () => {
    const messages: ChatMessage[] = [
      userMsg(1, 'q'),
      {
        seq: 2,
        role: 'assistant',
        content: '',
        sensitivity: 'personal',
        created_at: '',
        tool_calls: [{ id: 'c1', type: 'function', function: { name: 'ctx_get', arguments: '{"id":"nope"}' } }],
      },
      { seq: 3, role: 'tool', content: '{"error":"block not found"}', sensitivity: 'public', tool_call_id: 'c1', tool_name: 'ctx_get', created_at: '' },
      { seq: 4, role: 'assistant', content: 'partial', sensitivity: 'personal', created_at: '', metadata: { canceled: true } },
    ]
    const items = buildRenderItems(messages)
    const tool = items.find((i) => i.kind === 'tool')
    expect(tool?.kind === 'tool' && tool.result?.ok).toBe(false)
    expect(tool?.kind === 'tool' && tool.result?.summary).toBe('block not found')
    const ans = items.find((i) => i.kind === 'assistant')
    expect(ans?.kind === 'assistant' && ans.canceled).toBe(true)
  })

  it('renders a tools-disabled turn (assistant with content, no tool_calls) as a single bubble', () => {
    const items = buildRenderItems([userMsg(1, 'hi'), { seq: 2, role: 'assistant', content: 'hello', sensitivity: 'personal', created_at: '' }])
    expect(items.map((i) => i.kind)).toEqual(['user', 'assistant'])
  })

  it('reconstructs the staged ConfirmCard payload from a persisted ctx_store result (D-W6b)', () => {
    const staged = {
      payload_hash: 'a'.repeat(64),
      op: 'store',
      scope: 'private',
      category: 'test',
      title: 'T',
      sensitivity: 'personal',
      content_preview: 'p',
      content_chars: 1,
      expires_at: null,
    }
    const messages: ChatMessage[] = [
      userMsg(1, 'save this'),
      {
        seq: 2,
        role: 'assistant',
        content: '',
        sensitivity: 'personal',
        created_at: '',
        tool_calls: [{ id: 'c9', type: 'function', function: { name: 'ctx_store', arguments: '{"title":"T"}' } }],
      },
      {
        seq: 3,
        role: 'tool',
        content: JSON.stringify({ staged, note: 'awaiting confirmation' }),
        sensitivity: 'personal',
        tool_call_id: 'c9',
        tool_name: 'ctx_store',
        created_at: '',
      },
    ]
    const items = buildRenderItems(messages)
    const tool = items.find((i) => i.kind === 'tool')
    if (tool?.kind !== 'tool') throw new Error('expected tool item')
    expect(tool.result?.staged).toEqual(staged)
    expect(tool.result?.summary).toBe('staged — awaiting user confirmation')
    expect(tool.result?.ok).toBe(true)
  })

  it('reconstructs a staged ctx_update payload with the update form fields (D-W6c)', () => {
    const staged = {
      payload_hash: 'b'.repeat(64),
      op: 'update',
      scope: 'private',
      category: 'test',
      title: 'target block',
      sensitivity: '',
      content_preview: '',
      content_chars: 0,
      expires_at: null,
      target_id: '0198aaaa-0000-7000-8000-000000000001',
      update_fields: ['content', 'tags'],
    }
    const messages: ChatMessage[] = [
      userMsg(1, 'update that block'),
      {
        seq: 2,
        role: 'assistant',
        content: '',
        sensitivity: 'personal',
        created_at: '',
        tool_calls: [{ id: 'c10', type: 'function', function: { name: 'ctx_update', arguments: '{"id":"0198aaaa"}' } }],
      },
      {
        seq: 3,
        role: 'tool',
        content: JSON.stringify({ staged, note: 'awaiting confirmation' }),
        sensitivity: 'personal',
        tool_call_id: 'c10',
        tool_name: 'ctx_update',
        created_at: '',
      },
    ]
    const items = buildRenderItems(messages)
    const tool = items.find((i) => i.kind === 'tool')
    if (tool?.kind !== 'tool') throw new Error('expected tool item')
    expect(tool.result?.staged).toEqual(staged)
    expect(tool.result?.staged?.update_fields).toEqual(['content', 'tags'])
    expect(tool.result?.ok).toBe(true)
  })
})
