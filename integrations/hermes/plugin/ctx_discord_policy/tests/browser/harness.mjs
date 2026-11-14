// Test harness for the settings page core (B1): a jsdom document, React 19, a fake SDK kit, a fake ctx.rest
// and manual timers. ui/core.js has no module statements, so it is evaluated as a function body here.
import nodeAssert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { JSDOM, VirtualConsole } from 'jsdom'

const here = dirname(fileURLToPath(import.meta.url))
export const repoRoot = join(here, '..', '..')

// jsdom cannot follow the blob: link a download clicks; that and only that is not a test failure.
const virtualConsole = new VirtualConsole()
virtualConsole.on('error', (...args) => console.error(...args))
virtualConsole.on('warn', (...args) => console.warn(...args))
virtualConsole.on('jsdomError', (error) => {
  if (!/^Not implemented: navigation/.test(String(error && error.message))) console.error(error)
})
const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/', pretendToBeVisual: true,
  virtualConsole })
export const window = dom.window
export const document = window.document
for (const key of ['window', 'document', 'navigator', 'HTMLElement', 'HTMLInputElement', 'HTMLTextAreaElement', 'Node',
  'Element', 'getComputedStyle', 'requestAnimationFrame', 'cancelAnimationFrame', 'FileReader']) {
  Object.defineProperty(globalThis, key, { value: key === 'window' ? window : window[key], configurable: true, writable: true })
}
globalThis.IS_REACT_ACT_ENVIRONMENT = true

export const consoleErrors = []
const originalError = console.error
console.error = (...args) => {
  consoleErrors.push(args.map(String).join(' '))
  originalError(...args)
}

// ── assert that never inspects the DOM ────────────────────────────────────────────────────────────────
// A failing node:assert comparison formats its operands with util.inspect at depth 1000 and with getters,
// ignoring custom inspectors. A DOM node React rendered carries its fiber (__reactFiber$…), so the message walks
// the whole fiber tree and the jsdom window: measured 23 KB for one button, 480 KB for eight, and a cgroup
// OOM kill at 2 GB for sixteen; a whole page took the host down. Test files therefore import this assert:
// comparisons that involve a DOM node compare by identity here and fail with a one-line description.
const DOMISH = (v) => v !== null && typeof v === 'object' &&
  (v instanceof window.Node || Object.keys(v).some((k) => k.startsWith('__react')))
export function describeNode(v) {
  if (v === null || v === undefined) return String(v)
  if (!DOMISH(v)) return typeof v
  if (!(v instanceof window.Element)) return `<${v.nodeName}>`
  const label = (v.getAttribute('aria-label') || v.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 60)
  return `<${v.tagName.toLowerCase()}${v.id ? '#' + v.id : ''}> "${label}"`
}
// A DOM node anywhere in plain arrays and objects (bounded walk: depth 6, 500 values).
function involvesDom(v) {
  let budget = 500
  const walk = (x, depth) => {
    if (DOMISH(x)) return true
    if (x === null || typeof x !== 'object' || depth > 6) return false
    const proto = Object.getPrototypeOf(x)
    if (!Array.isArray(x) && proto !== Object.prototype && proto !== null) return false
    for (const value of Object.values(x)) {
      if (--budget < 0) return false
      if (walk(value, depth + 1)) return true
    }
    return false
  }
  return walk(v, 0)
}
const describeValue = (v) => (DOMISH(v) || v === null || v === undefined ? describeNode(v)
  : Array.isArray(v) ? 'an array holding DOM nodes' : typeof v === 'object' ? 'an object holding DOM nodes' : typeof v)
// node:assert rejects an explicit undefined message on newer Node versions: pass it only when there is one.
const pass = (fn, actual, expected, message) => (message === undefined ? fn(actual, expected) : fn(actual, expected, message))
const SAME = { equal: true, strictEqual: true, notEqual: false, notStrictEqual: false }
const DEEP = new Set(['deepEqual', 'deepStrictEqual', 'notDeepEqual', 'notDeepStrictEqual', 'partialDeepStrictEqual'])
export const assert = new Proxy(nodeAssert, {
  get(target, key) {
    const original = target[key]
    if (typeof original !== 'function') return original
    if (key in SAME) {
      return (actual, expected, message) => {
        if (!involvesDom(actual) && !involvesDom(expected)) return pass(original, actual, expected, message)
        if ((actual === expected) !== SAME[key]) {
          nodeAssert.fail(`${message ? message + ': ' : ''}expected ${describeValue(actual)} ` +
            `${SAME[key] ? 'to be' : 'not to be'} ${describeValue(expected)}`)
        }
      }
    }
    if (DEEP.has(key)) {
      return (actual, expected, message) => {
        if (involvesDom(actual) || involvesDom(expected)) {
          nodeAssert.fail(`${message ? message + ': ' : ''}${String(key)} with DOM nodes is not supported; ` +
            'compare ids, text or attributes')
        }
        return pass(original, actual, expected, message)
      }
    }
    return original
  }
})

export const React = (await import('react')).default
const { createRoot } = (await import('react-dom/client')).default
export const act = React.act
export const h = React.createElement

export const NOW = Date.UTC(2031, 2, 5, 14, 30, 0)

const coreSource = readFileSync(join(repoRoot, 'ui', 'core.js'), 'utf8')
// eslint-disable-next-line no-new-func
export const createPolicyUI = new Function(coreSource + '\nreturn createPolicyUI;')()

// A stand-in for the SDK components with the prop contracts of Hermes' own (simplified markup).
export function fakeKit() {
  const omit = (obj, keys) => Object.fromEntries(Object.entries(obj).filter(([k]) => !keys.includes(k)))
  return {
    Button: (p) => h('button', { 'data-variant': p.variant, ...omit(p, ['variant', 'size', 'loading', 'children']) }, p.children),
    Input: (p) => h('input', { 'data-kit': 'input', ...omit(p, ['prefix', 'suffix', 'containerClassName', 'size']) }),
    Textarea: (p) => h('textarea', { 'data-kit': 'textarea', ...omit(p, ['size']) }),
    ListRow: (p) => h('div', { id: p.id, 'data-kit': 'list-row' },
      h('div', null, h('span', null, p.title), p.wide && p.action ? h('div', null, p.action) : null),
      p.description ? h('div', null, p.description) : null,
      p.below || null,
      !p.wide && p.action ? h('div', null, p.action) : null),
    ToggleRow: (p) => h('div', { id: p.id, 'data-kit': 'toggle-row' },
      h('span', null, p.label),
      p.description ? h('div', null, p.description) : null,
      h('button', { type: 'button', role: 'switch', 'aria-checked': p.checked ? 'true' : 'false', 'aria-label': p.label,
        disabled: p.disabled, onClick: () => p.onChange(!p.checked) }),
      p.below || null),
    SegmentedControl: (p) => h('div', { 'data-kit': 'segmented' }, p.options.map((o) => h('button', {
      key: o.id, type: 'button', 'aria-pressed': p.value === o.id ? 'true' : 'false', disabled: p.disabled,
      onClick: () => p.onChange(o.id) }, o.label))),
    Badge: (p) => h('span', { 'data-kit': 'badge' }, p.children),
    EmptyState: (p) => h('div', { 'data-kit': 'empty' }, h('div', null, p.title), p.description ? h('div', null, p.description) : null)
  }
}

export function restError(status, body) {
  const json = JSON.stringify(body)
  return new Error(status === 404 ? `404: ${json}` : `Error invoking remote method 'hermes:api': Error: ${status}: ${json}`)
}

export function httpError(status, body) { return { __error: restError(status, body) } }

// routes: {'GET /v1/meta': body | (req) => body | httpError(...)}; the query string is not part of the key.
export function fakeRest(routes) {
  const calls = []
  const rest = async (path, opts = {}) => {
    const method = (opts.method || 'GET').toUpperCase()
    const [bare, query = ''] = path.split('?')
    const req = { method, path, bare, query: Object.fromEntries(new URLSearchParams(query)), body: opts.body }
    calls.push(req)
    const handler = routes[`${method} ${bare}`]
    if (handler === undefined) throw restError(404, { error: { code: 'not_found', message: `no route ${method} ${bare}` } })
    const out = typeof handler === 'function' ? await handler(req) : handler
    if (out instanceof Error) throw out
    if (out && out.__error) throw out.__error
    return JSON.parse(JSON.stringify(out))
  }
  return { rest, calls, routes, find: (method, bare) => calls.filter((c) => c.method === method && c.bare === bare) }
}

export function fakeTimers() {
  let clock = 0
  let seq = 0
  const queue = new Set()
  const listeners = []
  const api = {
    setTimeout(fn, ms) {
      const item = { id: ++seq, at: clock + ms, fn }
      queue.add(item)
      return () => queue.delete(item)
    },
    setInterval(fn, ms) {
      const item = { id: ++seq, at: clock + ms, fn, every: ms }
      queue.add(item)
      return () => queue.delete(item)
    },
    addEventListener(target, type, fn) {
      target.addEventListener(type, fn)
      const entry = { target, type, fn, active: true }
      listeners.push(entry)
      return () => {
        entry.active = false
        target.removeEventListener(type, fn)
      }
    },
    intervals: () => [...queue].filter((i) => i.every),
    listeners: () => listeners.filter((l) => l.active),
    async advance(ms) {
      const until = clock + ms
      for (;;) {
        const due = [...queue].filter((i) => i.at <= until).sort((a, b) => a.at - b.at || a.id - b.id)[0]
        if (!due) break
        clock = due.at
        if (due.every) due.at += due.every
        else queue.delete(due)
        await act(async () => { due.fn() })
        await settle()
      }
      clock = until
    }
  }
  return api
}

export function memoryStorage(initial = {}) {
  const data = { ...initial }
  return { data, get: (k, d) => (k in data ? data[k] : d), set: (k, v) => { data[k] = v }, remove: (k) => { delete data[k] } }
}

export async function settle(rounds = 6) {
  for (let i = 0; i < rounds; i++) {
    await act(async () => { await new Promise((resolve) => setImmediate(resolve)) })
  }
}

let mounted = []
export async function render(element) {
  const container = document.createElement('div')
  document.body.appendChild(container)
  const root = createRoot(container)
  await act(async () => { root.render(element) })
  await settle()
  const handle = {
    container,
    async unmount() {
      await act(async () => { root.unmount() })
      container.remove()
    }
  }
  mounted.push(handle)
  return handle
}
export async function cleanup() {
  const all = mounted
  mounted = []
  for (const m of all) await m.unmount()
}

// One page of the core with fresh fakes; returns everything a test needs.
export async function mount(page, { routes, kit = fakeKit(), storage = memoryStorage(), now = NOW } = {}) {
  const api = fakeRest(routes)
  const timers = fakeTimers()
  const navigated = []
  const ui = createPolicyUI({ React, kit, rest: api.rest, storage, timers, now: () => now,
    nav: { go: (p) => navigated.push(p) } })
  const view = await render(h(ui[page]))
  return { ui, api, timers, storage, navigated, ...view }
}

// ── DOM helpers ────────────────────────────────────────────────────────────────────────────────────────

export const $ = (root, sel) => root.querySelector(sel)
export const $$ = (root, sel) => [...root.querySelectorAll(sel)]
export const text = (el) => (el ? el.textContent.replace(/\s+/g, ' ').trim() : '')
export function byText(root, sel, wanted) {
  return $$(root, sel).find((el) => (wanted instanceof RegExp ? wanted.test(text(el)) : text(el) === wanted)) || null
}
export function button(root, label) {
  return $$(root, 'button').find((b) => text(b) === label || b.getAttribute('aria-label') === label) || null
}

export async function keydown(el, key, extra = {}) {
  const event = new window.KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...extra })
  await act(async () => { el.dispatchEvent(event) })
  return event
}
export async function type(el, value) {
  const proto = el instanceof window.HTMLTextAreaElement ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype
  await act(async () => {
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, value)
    el.dispatchEvent(new window.Event('input', { bubbles: true }))
  })
}
export async function click(el) {
  if (!el) throw new Error('click: no element')
  await act(async () => { el.dispatchEvent(new window.MouseEvent('click', { bubbles: true, cancelable: true })) })
  await settle()
}
export async function focus(el) {
  await act(async () => { el.focus() })
}
