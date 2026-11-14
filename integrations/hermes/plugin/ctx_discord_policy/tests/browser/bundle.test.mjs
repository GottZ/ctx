// The generated desktop/plugin.js evaluated as the ES module Hermes Desktop loads: its two specifiers are
// mapped to shim modules over a fake SDK, the way the Desktop runtime loader maps them to its own.
import { assert } from './harness.mjs'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { routes } from './fixtures.mjs'
import { React, cleanup, consoleErrors, fakeKit, fakeRest, fakeTimers, h, render, repoRoot, text } from './harness.mjs'

afterEach(cleanup)

function shim(globalKey, names) {
  const source = `const m = globalThis.${globalKey};\nexport default m.default ?? m;\n` +
    (names.length ? `export const { ${names.join(', ')} } = m;\n` : '')
  return 'data:text/javascript;base64,' + Buffer.from(source).toString('base64')
}

async function loadBundle(sdk) {
  globalThis.__TEST_SDK__ = sdk
  globalThis.__TEST_REACT__ = React
  const bundle = readFileSync(join(repoRoot, 'desktop', 'plugin.js'), 'utf8')
  const map = {
    '@hermes/plugin-sdk': shim('__TEST_SDK__', Object.keys(sdk)),
    react: shim('__TEST_REACT__', Object.keys(React).filter((n) => /^[A-Za-z_$][\w$]*$/.test(n) && n !== 'default'))
  }
  const rewritten = bundle.replace(/(from\s*)(['"])([^'"]+)\2/g, (whole, pre, quote, spec) => {
    assert.ok(spec in map, `unexpected import specifier ${spec}`)
    return pre + quote + map[spec] + quote
  })
  const mod = await import('data:text/javascript;base64,' + Buffer.from(rewritten + `\n//# ${Math.random()}`).toString('base64'))
  return mod.default
}

function fakeCtx(extra = {}) {
  const api = fakeRest(routes())
  const timers = fakeTimers()
  const registered = []
  const store = {}
  const ctx = {
    rest: api.rest,
    storage: { get: (k, d) => (k in store ? store[k] : d), set: (k, v) => { store[k] = v }, remove: (k) => { delete store[k] } },
    setTimeout: timers.setTimeout, setInterval: timers.setInterval, addEventListener: timers.addEventListener,
    onDispose: (fn) => { ctx.disposers.push(fn) }, disposers: [],
    registerSettingsPage: (page) => { registered.push(page); return () => {} },
    register: (c) => { registered.push(c); return () => {} },
    ...extra
  }
  return { ctx, registered, api, timers }
}

test('the bundle default-exports the plugin and registers one settings entry with sub-pages in order', async () => {
  const navigated = []
  const sdk = { ...fakeKit(), host: { navigate: (href) => navigated.push(href) },
    pluginSettingsHref: (plugin, page) => `/settings?tab=plugins&plugin=${plugin}${page ? `&ppage=${page}` : ''}`,
    SETTINGS_PLUGINS_AREA: 'settings.plugins' }
  const plugin = await loadBundle(sdk)
  assert.equal(plugin.id, 'ctx_discord_policy')
  assert.equal(typeof plugin.register, 'function')
  const { ctx, registered } = fakeCtx()
  plugin.register(ctx)
  assert.equal(registered.length, 1)
  const [page] = registered
  assert.equal(page.icon, 'shield')
  assert.equal(page.title, 'ctx_discord_policy')
  assert.equal(typeof page.render, 'function')
  assert.deepEqual(page.children.map((c) => [c.id, c.title]),
    [['servers', 'Servers'], ['operators', 'Operators'], ['status', 'Status'], ['activity', 'Activity']])
  assert.ok(page.children.every((c) => typeof c.render === 'function'))
  const view = await render(h(() => page.render()))
  assert.match(text(view.container), /ctx_discord_policy/)
  assert.match(text(view.container), /Example Guild/)
  const edit = [...view.container.querySelectorAll('button')].find((b) => text(b) === 'Edit')
  edit.click()
  assert.deepEqual(navigated, ['/settings?tab=plugins&plugin=ctx_discord_policy&ppage=operators'])
  assert.equal(ctx.disposers.length, 1)
})

test('without registerSettingsPage the bundle falls back to ctx.register in the settings area', async () => {
  const plugin = await loadBundle({ ...fakeKit(), SETTINGS_PLUGINS_AREA: 'settings.plugins' })
  const { ctx, registered } = fakeCtx({ registerSettingsPage: undefined })
  plugin.register(ctx)
  const [c] = registered
  assert.equal(c.area, 'settings.plugins')
  assert.equal(c.title, 'ctx_discord_policy')
  assert.equal(c.data.icon, 'shield')
  assert.deepEqual(c.data.children.map((x) => x.id), ['servers', 'operators', 'status', 'activity'])
})

test('a host without some kit components still renders (plain fallbacks)', async () => {
  const plugin = await loadBundle({})
  const { ctx, registered } = fakeCtx()
  plugin.register(ctx)
  const view = await render(h(() => registered[0].children[2].render()))
  assert.match(text(view.container), /Protection status/)
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})
