// Working-order step of the backend-pool drag (BackendTable.svelte): where the
// held row group goes when the pointer moves over another row group's slot.
// Pure, so vitest pins it without a DOM; the table measures the slots.

/** A row group's layout slot along the drag axis, in pointer coordinates. */
export interface Slot {
  top: number
  height: number
}

/**
 * The working order after a pointer move to `y` over `overId`'s slot while
 * `dragId` is held, or null when the order stays as it is.
 *
 * Row groups differ in height (an expanded test row, profile chips). Swapping
 * as soon as the pointer entered a taller row's slot left the pointer inside
 * the slot that row takes after the swap — "another row" again — so the next
 * move swapped back and the order flipped on every move (#242). The held row
 * therefore takes the target's place only when the pointer lies in the slot it
 * would occupy there: moving up `y < target.top + held.height`, moving down
 * `y >= target.top + target.height − held.height`. Short of that line it moves
 * only up to the target's near side, so a fast move that skipped rows still
 * lands next to the row under the pointer; the band between the two lines
 * keeps whichever order the pointer arrived with.
 *
 * Both positions keep every slot between them contiguous: moving up, the held
 * row's new top is the target's top; moving down, its new bottom is the
 * target's bottom — so `held.height` alone decides where its new slot ends.
 */
export function stepOrder(
  order: readonly string[],
  dragId: string,
  overId: string,
  y: number,
  held: Slot,
  target: Slot,
): string[] | null {
  const from = order.indexOf(dragId)
  const over = order.indexOf(overId)
  if (from < 0 || over < 0 || from === over) return null
  const rest = order.filter((id) => id !== dragId)
  const at = rest.indexOf(overId) // the target's index once the held row is out
  const up = over < from
  const crossed = up ? y < target.top + held.height : y >= target.top + target.height - held.height
  // Crossed: the held row takes the target's place (above it moving up, below
  // it moving down). Not crossed: it goes to the target's near side.
  const below = up ? !crossed : crossed
  const insert = below ? at + 1 : at
  if (insert === from) return null
  rest.splice(insert, 0, dragId)
  return rest
}
