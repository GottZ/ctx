// stepOrder gates (#242): the held row group takes another row's place only
// once the pointer lies in the slot it would occupy there, in both directions;
// rows of equal height swap on entry as before; a sweep over a taller row
// changes the order exactly twice (once per line), never on every move; a
// fast move that skipped rows lands next to the row under the pointer.

import { describe, expect, it } from 'vitest'
import { stepOrder, type Slot } from './drag-order'

const TOP = 100

/** Stack the row groups without gaps, the way the table lays them out. */
function layout(order: readonly string[], heights: Record<string, number>): Map<string, Slot> {
  const slots = new Map<string, Slot>()
  let top = TOP
  for (const id of order) {
    slots.set(id, { top, height: heights[id] })
    top += heights[id]
  }
  return slots
}

/** Drive one drag through pointer positions; the order after every move. */
function drive(order: readonly string[], heights: Record<string, number>, dragId: string, ys: readonly number[]): string[] {
  let cur = [...order]
  const seen: string[] = []
  for (const y of ys) {
    const slots = layout(cur, heights)
    const over = [...slots].find(([, s]) => y >= s.top && y < s.top + s.height)?.[0]
    if (over !== undefined && over !== dragId) {
      const next = stepOrder(cur, dragId, over, y, slots.get(dragId) as Slot, slots.get(over) as Slot)
      if (next !== null) cur = next
    }
    seen.push(cur.join(','))
  }
  return seen
}

function range(from: number, to: number): number[] {
  const out: number[] = []
  const step = from <= to ? 1 : -1
  for (let y = from; step > 0 ? y <= to : y >= to; y += step) out.push(y)
  return out
}

/** Indices at which the order differs from the one before (index 0 = first move vs start). */
function changes(start: readonly string[], seen: readonly string[]): number[] {
  const out: number[] = []
  let prev = start.join(',')
  seen.forEach((o, i) => {
    if (o !== prev) out.push(i)
    prev = o
  })
  return out
}

describe('stepOrder — rows of equal height', () => {
  const h = { a: 40, b: 40, c: 40 }

  it('swaps on entry into the neighbour slot, up and down', () => {
    // a [100,140) b [140,180) c [180,220)
    const s = layout(['a', 'b', 'c'], h)
    expect(stepOrder(['a', 'b', 'c'], 'b', 'a', 139, s.get('b') as Slot, s.get('a') as Slot)).toEqual(['b', 'a', 'c'])
    expect(stepOrder(['a', 'b', 'c'], 'b', 'c', 180, s.get('b') as Slot, s.get('c') as Slot)).toEqual(['a', 'c', 'b'])
  })

  it('a jump over several rows lands at the target', () => {
    const order = ['a', 'b', 'c', 'd']
    const s = layout(order, { ...h, d: 40 })
    expect(stepOrder(order, 'd', 'a', 101, s.get('d') as Slot, s.get('a') as Slot)).toEqual(['d', 'a', 'b', 'c'])
    // d [220,260): the pointer just entered it.
    expect(stepOrder(order, 'a', 'd', 221, s.get('a') as Slot, s.get('d') as Slot)).toEqual(['b', 'c', 'd', 'a'])
  })
})

describe('stepOrder — a shorter row over a taller one (#242)', () => {
  // a is the taller row (an expanded test row): a [100,180) b [180,225) c [225,270)
  const h = { a: 80, b: 45, c: 45 }

  it('moving up, swaps only above target.top + held.height', () => {
    const s = layout(['a', 'b', 'c'], h)
    const held = s.get('b') as Slot
    const target = s.get('a') as Slot
    expect(stepOrder(['a', 'b', 'c'], 'b', 'a', 179, held, target)).toBeNull()
    expect(stepOrder(['a', 'b', 'c'], 'b', 'a', 145, held, target)).toBeNull()
    expect(stepOrder(['a', 'b', 'c'], 'b', 'a', 144.5, held, target)).toEqual(['b', 'a', 'c'])
  })

  it('moving down, swaps only from target.bottom − held.height', () => {
    // b is held at the top this time: b [100,145) a [145,225) c [225,270)
    const s = layout(['b', 'a', 'c'], h)
    const held = s.get('b') as Slot
    const target = s.get('a') as Slot
    expect(stepOrder(['b', 'a', 'c'], 'b', 'a', 145, held, target)).toBeNull()
    expect(stepOrder(['b', 'a', 'c'], 'b', 'a', 179.5, held, target)).toBeNull()
    expect(stepOrder(['b', 'a', 'c'], 'b', 'a', 180, held, target)).toEqual(['a', 'b', 'c'])
  })

  it('a 1px sweep up over the taller row and back changes the order exactly at the two lines', () => {
    const start = ['a', 'b', 'c']
    const ys = [...range(179, 101), ...range(102, 224)]
    const seen = drive(start, h, 'b', ys)
    // Up: the swap happens at the first y below 100 + 45; down: at 225 − 45.
    expect(changes(start, seen).map((i) => ys[i])).toEqual([144, 180])
    expect(seen[ys.indexOf(144)]).toBe('b,a,c')
    expect(seen.at(-1)).toBe('a,b,c')
  })

  it('a 1px sweep down over a taller row below and back changes the order exactly at the two lines', () => {
    // c is the taller row now: a [100,145) b [145,190) c [190,270)
    const hh = { a: 45, b: 45, c: 80 }
    const start = ['a', 'b', 'c']
    const ys = [...range(191, 269), ...range(268, 146)]
    const seen = drive(start, hh, 'b', ys)
    // Down: the swap at 270 − 45; back up: at the first y below 145 + 45.
    expect(changes(start, seen).map((i) => ys[i])).toEqual([225, 189])
    expect(seen[ys.indexOf(225)]).toBe('a,c,b')
    expect(seen.at(-1)).toBe('a,b,c')
  })

  it('a held row taller than the target swaps on entry and stays', () => {
    const tall = { a: 45, b: 80, c: 45 }
    // a [100,145) b [145,225) c [225,270)
    expect(drive(['a', 'b', 'c'], tall, 'b', [144, 140, 120, 101])).toEqual(['b,a,c', 'b,a,c', 'b,a,c', 'b,a,c'])
    expect(drive(['a', 'b', 'c'], tall, 'b', [225, 240, 269])).toEqual(['a,c,b', 'a,c,b', 'a,c,b'])
  })
})

describe('stepOrder — a fast move that skipped rows', () => {
  it('moving up into the lower part of a taller row lands right below it', () => {
    // a [100,180) c [180,225) b [225,270): b jumps over c into a's lower part.
    const h = { a: 80, b: 45, c: 45 }
    const s = layout(['a', 'c', 'b'], h)
    expect(stepOrder(['a', 'c', 'b'], 'b', 'a', 170, s.get('b') as Slot, s.get('a') as Slot)).toEqual(['a', 'b', 'c'])
    // Already right below it: the order stays until the line.
    const s2 = layout(['a', 'b', 'c'], h)
    expect(stepOrder(['a', 'b', 'c'], 'b', 'a', 170, s2.get('b') as Slot, s2.get('a') as Slot)).toBeNull()
  })

  it('moving down into the upper part of a taller row lands right above it', () => {
    // b [100,145) c [145,190) a [190,270): b jumps over c into a's upper part.
    const h = { a: 80, b: 45, c: 45 }
    const s = layout(['b', 'c', 'a'], h)
    expect(stepOrder(['b', 'c', 'a'], 'b', 'a', 200, s.get('b') as Slot, s.get('a') as Slot)).toEqual(['c', 'b', 'a'])
    const s2 = layout(['c', 'b', 'a'], h)
    expect(stepOrder(['c', 'b', 'a'], 'b', 'a', 200, s2.get('b') as Slot, s2.get('a') as Slot)).toBeNull()
  })
})

describe('stepOrder — no move', () => {
  const s: Slot = { top: 0, height: 10 }

  it('answers null for the held row itself and for ids outside the order', () => {
    expect(stepOrder(['a', 'b'], 'a', 'a', 5, s, s)).toBeNull()
    expect(stepOrder(['a', 'b'], 'x', 'a', 5, s, s)).toBeNull()
    expect(stepOrder(['a', 'b'], 'a', 'x', 5, s, s)).toBeNull()
  })
})
