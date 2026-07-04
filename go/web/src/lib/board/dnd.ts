// Board drag-and-drop adapter (design 04 §4.5, wave U08). A THIN contract over
// the library so the board never imports the DnD library directly — the whole
// point of §4.5 is that A (svelte-dnd-action, the proven fallback from the U00
// spike) could replace B (pragmatic-drag-and-drop, the spike winner) without a
// single board-component change. Everything the board touches goes through
// BoardDndAdapter + the two Svelte actions below.
//
// Library = @atlaskit/pragmatic-drag-and-drop 2.0.1 CORE only (U00 decision E11,
// spike-dnd.md). No auto-scroll / hitbox extras: v1 drop is COLUMN-LEVEL (a drop
// anywhere on a column is a status change, E04-3 — no intra-column ordering), so
// the pointer never needs to reach a precise row and the auto-scroll addon is
// not needed. The spike proved core dragTo + a keyboard path headless-green.
//
// Recycling-fest (the spike lesson): a card action registers ONCE and refreshes
// its issue-id via update() when virtua recycles the element into a new card —
// NEVER re-registration on every index shift (that was A's whole failure class).
//
// The keyboard transition is NOT here: it is its own mechanic (the Move dialog
// in BoardPage) because the library ships no keyboard path (spike matrix Q4) and
// §4.5 puts the keyboard path in the adapter CONTRACT, not the library.

import { combine } from '@atlaskit/pragmatic-drag-and-drop/combine'
import {
  draggable,
  dropTargetForElements,
  monitorForElements,
} from '@atlaskit/pragmatic-drag-and-drop/element/adapter'

/** Marker on a draggable's data so a column drop-target / the monitor only reacts
 * to board cards (never a stray external drag). A symbol so it cannot collide
 * with a status id or any wire field. */
const CARD_MARK = Symbol('ctx-board-card')

type DragStartCb = (issueId: string, from: string) => void
type DropCb = (issueId: string, from: string, to: string) => void

/** The board's card-drag arguments; kept in a mutable ref so update() can
 * refresh them on virtua recycling without re-registering the draggable. */
export interface CardDragArgs {
  issueId: string
  from: string
}

/** A live card registration — update() refreshes the recycled identity,
 * destroy() tears the draggable down (Svelte-action shaped). */
export interface CardHandle {
  update(next: CardDragArgs): void
  destroy(): void
}

/** The board DnD contract (§4.5). attachColumn/attachCard return teardown; the
 * two callback registrations drive the optimistic transition in BoardModel. The
 * keyboard path is deliberately absent here — it is the Move dialog (contract,
 * not library). */
export interface BoardDndAdapter {
  attachColumn(el: HTMLElement, statusId: string): () => void
  attachCard(el: HTMLElement, args: CardDragArgs): CardHandle
  ondragstart(cb: DragStartCb): void
  ondrop(cb: DropCb): void
  destroy(): void
}

/** Build the adapter (one per mounted board). A single monitorForElements reads
 * the drop and dispatches to the registered callbacks; columns are drop targets,
 * cards are draggables. */
export function createBoardDnd(): BoardDndAdapter {
  let onStart: DragStartCb | null = null
  let onDropCb: DropCb | null = null

  const isCard = (data: Record<string | symbol, unknown>): boolean => data[CARD_MARK] === true

  const stopMonitor = monitorForElements({
    canMonitor: ({ source }) => isCard(source.data),
    onDragStart: ({ source }) => {
      if (onStart === null) return
      onStart(String(source.data.issueId), String(source.data.from))
    },
    onDrop: ({ source, location }) => {
      // Column-level: the innermost drop target is the column; its data carries
      // the status id. A drop OUTSIDE any column (dropTargets empty) is a no-op.
      const target = location.current.dropTargets[0]
      if (target === undefined || onDropCb === null) return
      const to = target.data.statusId
      if (typeof to !== 'string') return
      onDropCb(String(source.data.issueId), String(source.data.from), to)
    },
  })

  return {
    attachColumn(el, statusId) {
      return dropTargetForElements({
        element: el,
        canDrop: ({ source }) => isCard(source.data),
        getData: () => ({ statusId }),
        // Column highlight during a hover (spike: immune to the virtua
        // pointer-events pause because it is column-level, not row-level).
        onDragEnter: () => el.setAttribute('data-drop-over', ''),
        onDragLeave: () => el.removeAttribute('data-drop-over'),
        onDrop: () => el.removeAttribute('data-drop-over'),
      })
    },
    attachCard(el, args) {
      // Mutable ref: getInitialData reads it at drag time, so update() on a
      // recycled element takes effect without re-registering (spike Q2).
      const ref: CardDragArgs = { issueId: args.issueId, from: args.from }
      const cleanup = combine(
        draggable({
          element: el,
          getInitialData: () => ({ [CARD_MARK]: true, issueId: ref.issueId, from: ref.from }),
          onDragStart: () => el.setAttribute('data-dragging', ''),
          onDrop: () => el.removeAttribute('data-dragging'),
        }),
      )
      return {
        update(next) {
          ref.issueId = next.issueId
          ref.from = next.from
        },
        destroy() {
          cleanup()
        },
      }
    },
    ondragstart(cb) {
      onStart = cb
    },
    ondrop(cb) {
      onDropCb = cb
    },
    destroy() {
      stopMonitor()
    },
  }
}

// --- Svelte actions (use:) so the components stay declarative -----------------
// Both accept a null adapter: when the board is read-only (writable:false, §5.3)
// the page passes adapter=null and NOTHING is registered — no drop target, no
// grip — which is the writable gate expressed at the action layer.

/** Column drop-target action. adapter=null ⇒ the column is not a drop target
 * (read-only board). */
export function columnDrop(
  node: HTMLElement,
  params: { adapter: BoardDndAdapter | null; statusId: string },
): { update(next: { adapter: BoardDndAdapter | null; statusId: string }): void; destroy(): void } {
  let current = params
  let cleanup = params.adapter?.attachColumn(node, params.statusId) ?? null
  return {
    update(next) {
      if (next.adapter === current.adapter && next.statusId === current.statusId) return
      cleanup?.()
      current = next
      cleanup = next.adapter?.attachColumn(node, next.statusId) ?? null
    },
    destroy() {
      cleanup?.()
    },
  }
}

/** Card draggable action. adapter=null ⇒ the card is not draggable (read-only).
 * On a recycled element (issueId/from change, same adapter) it UPDATES the live
 * handle rather than re-registering (recycling-fest). */
export function cardDrag(
  node: HTMLElement,
  params: { adapter: BoardDndAdapter | null } & CardDragArgs,
): { update(next: { adapter: BoardDndAdapter | null } & CardDragArgs): void; destroy(): void } {
  let current = params
  let handle: CardHandle | null =
    params.adapter?.attachCard(node, { issueId: params.issueId, from: params.from }) ?? null
  return {
    update(next) {
      if (next.adapter !== current.adapter) {
        handle?.destroy()
        handle = next.adapter?.attachCard(node, { issueId: next.issueId, from: next.from }) ?? null
      } else {
        handle?.update({ issueId: next.issueId, from: next.from })
      }
      current = next
    },
    destroy() {
      handle?.destroy()
    },
  }
}
