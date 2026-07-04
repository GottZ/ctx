// Board DnD action plumbing (design 04 §4.5, wave U08). The real drag behaviour
// is proven in the browser (e2e board-dnd.spec.ts) — this DOM-free suite pins the
// two contract properties the actions guarantee regardless of the library:
//   1. adapter=null ⇒ NOTHING is registered (the writable gate at the action
//      layer — a read-only board has no drop targets / no grips, §5.3);
//   2. a recycled card (issueId/from change, SAME adapter) UPDATES the live
//      handle instead of re-registering (recycling-fest — the spike's whole
//      failure class for candidate A, spike-dnd.md Q2).
// A fake adapter records the calls; no real HTMLElement is touched (the actions
// only pass the node through to the adapter).

import { describe, expect, it, vi } from 'vitest'
import { cardDrag, columnDrop, type BoardDndAdapter, type CardHandle } from './dnd'

const NODE = {} as unknown as HTMLElement

function fakeAdapter(): { adapter: BoardDndAdapter; handle: CardHandle; columnCleanup: () => void } {
  const handle: CardHandle = { update: vi.fn(), destroy: vi.fn() }
  const columnCleanup = vi.fn()
  const adapter: BoardDndAdapter = {
    attachCard: vi.fn(() => handle),
    attachColumn: vi.fn(() => columnCleanup),
    ondragstart: vi.fn(),
    ondrop: vi.fn(),
    destroy: vi.fn(),
  }
  return { adapter, handle, columnCleanup }
}

describe('cardDrag action', () => {
  it('adapter=null registers nothing (read-only gate)', () => {
    const { adapter } = fakeAdapter()
    const action = cardDrag(NODE, { adapter: null, issueId: 'i1', from: 'open' })
    expect(adapter.attachCard).not.toHaveBeenCalled()
    action.destroy()
  })

  it('registers a draggable and UPDATES on recycle (same adapter, new identity)', () => {
    const { adapter, handle } = fakeAdapter()
    const action = cardDrag(NODE, { adapter, issueId: 'i1', from: 'open' })
    expect(adapter.attachCard).toHaveBeenCalledTimes(1)
    expect(adapter.attachCard).toHaveBeenCalledWith(NODE, { issueId: 'i1', from: 'open' })
    // Recycle into a different card: NO re-registration, just update().
    action.update({ adapter, issueId: 'i2', from: 'review' })
    expect(adapter.attachCard).toHaveBeenCalledTimes(1)
    expect(handle.update).toHaveBeenCalledWith({ issueId: 'i2', from: 'review' })
    action.destroy()
    expect(handle.destroy).toHaveBeenCalledTimes(1)
  })

  it('toggling to a null adapter tears the handle down (board turned read-only)', () => {
    const { adapter, handle } = fakeAdapter()
    const action = cardDrag(NODE, { adapter, issueId: 'i1', from: 'open' })
    action.update({ adapter: null, issueId: 'i1', from: 'open' })
    expect(handle.destroy).toHaveBeenCalledTimes(1)
    // Turning it back on re-attaches.
    action.update({ adapter, issueId: 'i1', from: 'open' })
    expect(adapter.attachCard).toHaveBeenCalledTimes(2)
    action.destroy()
  })
})

describe('columnDrop action', () => {
  it('adapter=null registers no drop target', () => {
    const { adapter } = fakeAdapter()
    const action = columnDrop(NODE, { adapter: null, statusId: 'open' })
    expect(adapter.attachColumn).not.toHaveBeenCalled()
    action.destroy()
  })

  it('registers a drop target and cleans up on destroy', () => {
    const { adapter, columnCleanup } = fakeAdapter()
    const action = columnDrop(NODE, { adapter, statusId: 'open' })
    expect(adapter.attachColumn).toHaveBeenCalledWith(NODE, 'open')
    action.destroy()
    expect(columnCleanup).toHaveBeenCalledTimes(1)
  })

  it('re-attaches when the adapter toggles (read-only ⇄ writable)', () => {
    const { adapter, columnCleanup } = fakeAdapter()
    const action = columnDrop(NODE, { adapter: null, statusId: 'open' })
    action.update({ adapter, statusId: 'open' })
    expect(adapter.attachColumn).toHaveBeenCalledTimes(1)
    action.update({ adapter: null, statusId: 'open' })
    expect(columnCleanup).toHaveBeenCalledTimes(1)
    action.destroy()
  })
})
