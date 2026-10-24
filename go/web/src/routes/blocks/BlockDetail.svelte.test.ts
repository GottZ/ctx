// BlockDetail action-row gates (PR #48 review): `graph_visible` must hide ONLY
// the "Open in graph" link. A retrieval-excluded type (checkpoint, system-meta)
// is still editable and deletable in /blocks, so Edit and Delete stay visible.
// The component is server-rendered and asserted on its markup (ConnState test
// pattern) — `mount()` is unavailable in the node-only vitest env.

import { describe, expect, it } from 'vitest'
import { render } from 'svelte/server'
import type { BlockDetail as BlockDetailData } from '../../lib/api/blocks'
import BlockDetail from './BlockDetail.svelte'
import { BlockDeleteModel } from './delete.svelte'
import { BlockDetailModel } from './detail.svelte'

function block(id: string, category: string): BlockDetailData {
  return {
    id,
    category,
    tags: [],
    title: `title-${id}`,
    content: 'body',
    metadata: {},
    scope: 'private',
    created_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-14T00:00:00Z',
  }
}

async function html(category: string, graphVisible: boolean): Promise<string> {
  const model = new BlockDetailModel({
    get: (id: string) => Promise.resolve({ success: true, block: block(id, category), graph_visible: graphVisible }),
  })
  await model.load('b1')
  const deleteModel = new BlockDeleteModel({ del: () => Promise.reject(new Error('unused')) })
  return render(BlockDetail, { props: { model, onedit: () => {}, deleteModel } }).body
}

describe('BlockDetail actions', () => {
  it('excluded type → Edit/Delete visible, graph link absent', async () => {
    const markup = await html('checkpoint', false)
    expect(markup).toMatch(/class="edit[ "]/)
    expect(markup).toMatch(/class="del[ "]/)
    expect(markup).not.toContain('/graph?focus=')
    expect(markup).not.toContain('Open in graph')
  })

  it('graph-visible type → Edit/Delete and the graph link', async () => {
    const markup = await html('learnings', true)
    expect(markup).toMatch(/class="edit[ "]/)
    expect(markup).toMatch(/class="del[ "]/)
    expect(markup).toContain('href="/graph?focus=b1"')
  })
})
