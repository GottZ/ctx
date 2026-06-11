// Resource<T> gates (design 04-§2.4): three-stage status machine and the
// supersede guard — a stale in-flight load must never clobber a newer one.

import { describe, expect, it } from 'vitest'
import { ApiError } from './api'
import { Resource } from './resource.svelte'

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('Resource', () => {
  it('moves idle → loading → ready with data', async () => {
    const d = deferred<number>()
    const r = new Resource(() => d.promise)
    expect(r.status).toBe('idle')

    const load = r.load()
    expect(r.status).toBe('loading')
    d.resolve(42)
    await load

    expect(r.status).toBe('ready')
    expect(r.data).toBe(42)
    expect(r.error).toBeNull()
  })

  it('captures failures as ApiError', async () => {
    const r = new Resource<number>(() => Promise.reject(new ApiError(403, 'forbidden', 'admin key required')))
    await r.load()
    expect(r.status).toBe('error')
    expect(r.error?.code).toBe('forbidden')
    expect(r.data).toBeNull()
  })

  it('ignores a stale load resolving after a newer one (supersede)', async () => {
    const first = deferred<string>()
    const second = deferred<string>()
    const queue = [first.promise, second.promise]
    const r = new Resource(() => queue.shift() ?? Promise.reject(new Error('exhausted')))

    const l1 = r.load()
    const l2 = r.load()
    second.resolve('new')
    await l2
    expect(r.data).toBe('new')

    first.resolve('stale')
    await l1
    expect(r.data).toBe('new')
    expect(r.status).toBe('ready')
  })

  it('clears a previous error on reload', async () => {
    const queue: Array<() => Promise<number>> = [
      () => Promise.reject(new ApiError(500, 'server', 'boom')),
      () => Promise.resolve(7),
    ]
    const r = new Resource(() => (queue.shift() ?? (() => Promise.reject(new Error('exhausted'))))())
    await r.load()
    expect(r.status).toBe('error')
    await r.reload()
    expect(r.status).toBe('ready')
    expect(r.error).toBeNull()
    expect(r.data).toBe(7)
  })
})
