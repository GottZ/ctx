// Render transformation for a persisted message history (design 06 §3.9). The
// stored messages are a flat sequence (user → assistant[tool_calls] → tool →
// assistant[final]); the UI renders user/assistant bubbles with the tool calls
// of an assistant turn shown as cards, each paired with its tool result by
// tool_call_id. Persisted tool results carry a DIFFERENT shape than the live
// SSE tool_result event (the stored content is the JSON string fed to the
// model), so resultOf() reconstructs a ToolResultEvent-shaped view from it.
//
// Pure + testable — MessageList is then just markup over this list.

import type { ChatMessage, Sensitivity, ToolResultBlock, ToolResultEvent } from './types'

export type RenderItem =
  | { kind: 'user'; key: string; content: string; sensitivity: Sensitivity }
  | { kind: 'assistant'; key: string; content: string; sensitivity: Sensitivity; canceled: boolean }
  | { kind: 'tool'; key: string; name: string; args: Record<string, unknown>; result?: ToolResultEvent }

export function buildRenderItems(messages: ChatMessage[]): RenderItem[] {
  const toolByCallId = new Map<string, ChatMessage>()
  for (const m of messages) {
    if (m.role === 'tool' && m.tool_call_id) toolByCallId.set(m.tool_call_id, m)
  }

  const items: RenderItem[] = []
  for (const m of messages) {
    if (m.role === 'user') {
      items.push({ kind: 'user', key: `u${m.seq}`, content: m.content, sensitivity: m.sensitivity })
    } else if (m.role === 'assistant') {
      if (m.tool_calls) {
        m.tool_calls.forEach((tc, i) => {
          items.push({
            kind: 'tool',
            key: `t${m.seq}-${i}`,
            name: tc.function.name,
            args: parseArgs(tc.function.arguments),
            result: resultOf(toolByCallId.get(tc.id)),
          })
        })
      }
      if (m.content.trim() !== '') {
        items.push({
          kind: 'assistant',
          key: `a${m.seq}`,
          content: m.content,
          sensitivity: m.sensitivity,
          canceled: m.metadata?.canceled === true,
        })
      }
    }
    // role 'tool' is consumed by its assistant turn above.
  }
  return items
}

function parseArgs(raw: string): Record<string, unknown> {
  try {
    const v = JSON.parse(raw) as unknown
    return typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/** Reconstruct a ToolResultEvent-shaped view from a persisted tool message. */
function resultOf(toolMsg?: ChatMessage): ToolResultEvent | undefined {
  if (!toolMsg) return undefined
  let parsed: { blocks?: unknown; count?: unknown; error?: unknown } = {}
  try {
    parsed = JSON.parse(toolMsg.content) as typeof parsed
  } catch {
    /* a non-JSON / errored result — treated as ok:false below */
  }
  const ok = parsed.error === undefined
  const blocks: ToolResultBlock[] = Array.isArray(parsed.blocks)
    ? (parsed.blocks as Array<Record<string, unknown>>).map((b) => ({
        id: String(b.id ?? ''),
        title: String(b.title ?? ''),
        category: String(b.category ?? ''),
        score: typeof b.score === 'number' ? b.score : undefined,
      }))
    : []
  const count = typeof parsed.count === 'number' ? parsed.count : blocks.length
  return {
    iteration: 0,
    id: toolMsg.tool_call_id ?? '',
    name: toolMsg.tool_name ?? '',
    ok,
    duration_ms: toolMsg.duration_ms ?? 0,
    chars: toolMsg.content.length,
    truncated: false,
    summary: ok ? `${count} block${count === 1 ? '' : 's'}` : String(parsed.error ?? 'error'),
    blocks,
  }
}
