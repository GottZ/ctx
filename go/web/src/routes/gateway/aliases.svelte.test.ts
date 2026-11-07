// W05-13 — AliasesModel (design/05 §4.6 store table: `enabled` explicit on
// every create; RC-D C5). Gates: load() reads model-alias-list; create()
// passes `enabled` through untouched (false included) and reloads; update()
// forwards the patch by id and never a name / kind; suggestFromPool() asks
// model-alias-preview {role} per pool role and drafts ONLY enabled:false
// aliases — none for an empty role, none for a name the _global catalog
// already holds; a refusal lands on actionError AND rethrows.

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type { ModelAliasCreateSpec, ModelAliasListResponse, ModelAliasPreviewRequest, ModelAliasPreviewResult, ModelAliasUpdateSpec, ModelAliasWriteResult } from '../../lib/api/types'
import { AliasesModel } from './aliases.svelte'
import { aliasOf } from './forms.fixtures'

const G_CHAT = aliasOf('a-g-chat', '_global', 'chat', { writable: false })
const H_CHAT = aliasOf('a-h-chat', 'home', 'chat')
const QWEN = aliasOf('a-g-qwen', '_global', 'qwen38-27b', { writable: true })

function fakeApi(rows = [G_CHAT, H_CHAT], primaries: Record<string, string | null> = { chat: 'qwen38-27b', embed: 'qwen3-embedding-8b', rerank: null }) {
  const list = vi.fn(async (): Promise<ModelAliasListResponse> => ({ success: true, model_aliases: rows, count: rows.length }))
  const create = vi.fn(async (spec: ModelAliasCreateSpec): Promise<ModelAliasWriteResult> => ({
    success: true,
    model_alias: aliasOf(`new-${spec.name}`, spec.scope ?? '_global', spec.name, { enabled: spec.enabled, kind: spec.kind, role: spec.role }),
  }))
  const update = vi.fn(async (id: string, patch: ModelAliasUpdateSpec): Promise<ModelAliasWriteResult> => ({
    success: true,
    model_alias: { ...(rows.find((r) => r.id === id) ?? H_CHAT), ...(patch.enabled !== undefined ? { enabled: patch.enabled } : {}) },
  }))
  const preview = vi.fn(async (req: ModelAliasPreviewRequest): Promise<ModelAliasPreviewResult> => ({
    success: true,
    alias: req.alias ?? null,
    scope: null,
    required_sensitivity: 'credentials',
    chain: [],
    omitted: [],
    primary_model: req.role !== undefined ? (primaries[req.role] ?? null) : null,
  }))
  return { api: { list, create, update, preview }, list, create, update, preview }
}

describe('W05-13 AliasesModel', () => {
  it('W05-13: load() reads the visible catalog through model-alias-list', async () => {
    const f = fakeApi()
    const m = new AliasesModel(f.api)
    await m.load()
    expect(f.list, 'W05-13: one model-alias-list call').toHaveBeenCalledTimes(1)
    expect(m.aliases.map((a) => a.id), 'the rows as the server lists them').toEqual(['a-g-chat', 'a-h-chat'])
    expect(m.status).toBe('ready')
  })

  it('W05-13 gate: create() passes enabled through untouched — false included — and reloads', async () => {
    const f = fakeApi()
    const m = new AliasesModel(f.api)
    await m.load()
    await expect(m.create({ name: 'team-chat', kind: 'chat', role: 'chat', enabled: false }), 'W05-13 gate: the create goes through').resolves.toMatchObject({ success: true })
    expect(f.create.mock.calls[0]?.[0], 'W05-13 gate: enabled:false reaches the api').toEqual({ name: 'team-chat', kind: 'chat', role: 'chat', enabled: false })
    expect(f.list, 'reload after the create').toHaveBeenCalledTimes(2)
  })

  it('W05-13 gate: update() forwards the patch by id and never a name or kind', async () => {
    const f = fakeApi()
    const m = new AliasesModel(f.api)
    await m.load()
    await expect(m.update('a-h-chat', { enabled: false }), 'W05-13 gate: the update goes through').resolves.toMatchObject({ success: true })
    expect(f.update.mock.calls[0], 'W05-13: id + patch').toEqual(['a-h-chat', { enabled: false }])
    const sent = f.update.mock.calls[0]?.[1] as Record<string, unknown>
    for (const k of ['name', 'kind', 'scope', 'tenant_id']) expect(Object.keys(sent), `no ${k}`).not.toContain(k)
  })

  it('W05-13 gate: suggestFromPool drafts enabled:false aliases from the pool primaries, skips empty roles and existing names', async () => {
    const f = fakeApi([G_CHAT, H_CHAT, QWEN])
    const m = new AliasesModel(f.api)
    await m.load()
    const out = await m.suggestFromPool().catch((e: unknown) => {
      throw new Error(`W05-13 gate: suggestFromPool must draft from the pool — it failed: ${String(e)}`)
    })
    expect(f.preview.mock.calls.map((c) => c[0]), 'W05-13: one role preview per pool role').toEqual([{ role: 'chat' }, { role: 'embed' }, { role: 'rerank' }])
    // chat's primary qwen38-27b is already a _global alias ⇒ skipped; rerank has none ⇒ skipped
    expect(f.create.mock.calls.map((c) => c[0]), 'W05-13 gate: only disabled drafts').toEqual([
      { name: 'qwen3-embedding-8b', kind: 'embedding', role: 'embed', enabled: false },
    ])
    for (const c of f.create.mock.calls) expect(c[0].enabled, 'W05-13 gate: never a live draft').toBe(false)
    expect(out.created.map((a) => a.name)).toEqual(['qwen3-embedding-8b'])
    expect(out.skipped.sort(), 'skipped roles').toEqual(['chat', 'rerank'])
  })

  it('W05-13: an empty pool creates nothing (fail-closed)', async () => {
    const f = fakeApi([G_CHAT], { chat: null, embed: null, rerank: null })
    const m = new AliasesModel(f.api)
    await m.load()
    expect(f.list, 'positive control: the catalog was read').toHaveBeenCalledTimes(1)
    const out = await m.suggestFromPool().catch((e: unknown) => {
      throw new Error(`W05-13: an empty pool is no error — suggestFromPool failed: ${String(e)}`)
    })
    expect(f.create, 'nothing created').not.toHaveBeenCalled()
    expect(out).toEqual({ created: [], skipped: ['chat', 'embed', 'rerank'] })
  })

  it('W05-13: a refusal lands on actionError and rethrows; busyId clears', async () => {
    const f = fakeApi()
    f.update.mockRejectedValueOnce(new ApiError(403, 'forbidden', 'admin key required'))
    const m = new AliasesModel(f.api)
    await m.load()
    await expect(m.update('a-h-chat', { description: 'x' }), 'W05-13: the server refusal (403) reaches the dialog').rejects.toMatchObject({ status: 403 })
    expect(f.update, 'positive control: the api was called').toHaveBeenCalledTimes(1)
    expect(m.actionError, 'the refusal text').toBe('admin key required')
    expect(m.busyId).toBeNull()
  })

  it('W05-13: loadPreview keeps the C7 answer per alias id', async () => {
    const f = fakeApi()
    const m = new AliasesModel(f.api)
    await m.load()
    await m.loadPreview('a-h-chat', 'c-ci')
    expect(f.preview.mock.calls[0]?.[0], 'preview request').toEqual({ alias: 'a-h-chat', consumer_id: 'c-ci' })
    expect(m.preview['a-h-chat']?.alias, 'answer stored under the alias id').toBe('a-h-chat')
  })
})
