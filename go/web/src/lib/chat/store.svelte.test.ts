// Reducer tests for the chat store (design 06 §3.9). applyEvent is the
// SSE-event → state reduction; testing it directly needs no fetch mock and
// pins the turn rendering contract (delta accumulation, tool call+result
// pairing, error vs done).

import { describe, expect, it } from 'vitest'
import { ChatStore } from './store.svelte'

function store(): ChatStore {
  return new ChatStore(() => 'test-key')
}

describe('ChatStore.applyEvent', () => {
  it('learns the session id and accumulates content deltas', () => {
    const s = store()
    s.applyEvent('session', { session_id: '019ec000-aaaa', user_seq: 1 })
    s.applyEvent('backend', { backend: 'herbert-chat', model: 'qwen3.6:27b', trust: 'full-trust', tools_active: true, fallback: false })
    s.applyEvent('delta', { text: 'The dream ' })
    s.applyEvent('delta', { text: 'backoff' })

    expect(s.currentId).toBe('019ec000-aaaa')
    expect(s.liveBackend?.backend).toBe('herbert-chat')
    expect(s.liveBackend?.tools_active).toBe(true)
    expect(s.liveAssistant).toBe('The dream backoff')
  })

  it('pairs a tool_result onto its tool_call by id', () => {
    const s = store()
    s.applyEvent('tool_call', { iteration: 1, id: 'Kx8h', name: 'ctx_query', arguments: { query: 'dream backoff' } })
    expect(s.liveTools).toHaveLength(1)
    expect(s.liveTools[0].arguments).toEqual({ query: 'dream backoff' })
    expect(s.liveTools[0].result).toBeUndefined()

    s.applyEvent('tool_result', { iteration: 1, id: 'Kx8h', name: 'ctx_query', ok: true, duration_ms: 420, chars: 1834, truncated: false, summary: '5 blocks', blocks: [{ id: 'b1', title: 'T', category: 'learnings', score: 0.9 }] })
    expect(s.liveTools).toHaveLength(1)
    expect(s.liveTools[0].result?.ok).toBe(true)
    expect(s.liveTools[0].result?.blocks?.[0].title).toBe('T')
  })

  it('records a done event and leaves no turn error', () => {
    const s = store()
    s.applyEvent('delta', { text: 'answer' })
    s.applyEvent('done', { finish_reason: 'stop', assistant_seq: 4, iterations: 2, total_ms: 9214 })
    expect(s.done?.finish_reason).toBe('stop')
    expect(s.turnError).toBeNull()
  })

  it('surfaces an error event as a turn error (the partial stays in view)', () => {
    const s = store()
    s.applyEvent('delta', { text: 'partial' })
    s.applyEvent('error', { code: 'backend_unavailable', backend: 'herbert-chat', retryable: true })
    expect(s.turnError).toBe('backend_unavailable')
    expect(s.liveAssistant).toBe('partial') // kept, not cleared
  })

  it('ignores unknown / forward-compat events', () => {
    const s = store()
    s.applyEvent('tool_call_start', { iteration: 1, name: 'ctx_query' })
    s.applyEvent('usage', { iteration: 1, prompt_tokens: 10, completion_tokens: 5, draft_accept: 1 })
    s.applyEvent('pending_action', { id: 'x' }) // §3.8 forward-compat
    expect(s.liveAssistant).toBe('')
    expect(s.turnError).toBeNull()
  })
})
