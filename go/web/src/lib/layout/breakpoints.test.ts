import { afterEach, describe, expect, it, vi } from 'vitest'
import { SM, MD, LG, below, atLeast, onBelow } from './breakpoints'

function mockMatchMedia(matches: boolean) {
  const listeners = new Set<() => void>()
  const mq = {
    matches,
    addEventListener: (_: string, h: () => void) => listeners.add(h),
    removeEventListener: (_: string, h: () => void) => listeners.delete(h),
  }
  vi.stubGlobal('matchMedia', () => mq)
  return { mq, fire: () => listeners.forEach((h) => h()) }
}

afterEach(() => vi.unstubAllGlobals())

describe('breakpoints', () => {
  it('exposes the canonical px thresholds in ascending order', () => {
    expect(SM).toBe(640)
    expect(MD).toBe(1024)
    expect(LG).toBe(1440)
    expect(SM).toBeLessThan(MD)
    expect(MD).toBeLessThan(LG)
  })

  it('below / atLeast reflect matchMedia', () => {
    mockMatchMedia(true)
    expect(below(MD)).toBe(true)
    expect(atLeast(MD)).toBe(true) // both query the same mocked mq.matches
    mockMatchMedia(false)
    expect(below(MD)).toBe(false)
  })

  it('onBelow fires immediately and on change, and cleans up', () => {
    const { fire } = mockMatchMedia(true)
    const cb = vi.fn()
    const stop = onBelow(SM, cb)
    expect(cb).toHaveBeenCalledWith(true) // immediate
    fire()
    expect(cb).toHaveBeenCalledTimes(2)
    stop()
    fire()
    expect(cb).toHaveBeenCalledTimes(2) // no calls after cleanup
  })
})
