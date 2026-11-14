// Overview, Operators, Status and Activity pages, and the ctx.rest error parser (SETTINGS-PAGE §5.1–§5.6).
import { assert } from './harness.mjs'
import { afterEach, test } from 'node:test'

import { ALICE, BOT, GUILD, GUILD2, OPAL, iso, overview, routes, secs, settingsBody } from './fixtures.mjs'
import {
  $, $$, NOW, React, button, byText, cleanup, click, consoleErrors, createPolicyUI, document, fakeKit, fakeTimers, focus,
  h, httpError, keydown, memoryStorage, mount, restError, text, type, window
} from './harness.mjs'

afterEach(cleanup)

// ── overview ─────────────────────────────────────────────────────────────────────────────────────────

test('overview without a bot explains when it appears and offers the next step', async () => {
  const r = routes({ 'GET /v1/overview': overview({ bots: [], store: { state: 'missing', error: null, problems: [] },
    next_step: 'Start the Hermes gateway with the Discord platform enabled.' }) })
  const { container, navigated } = await mount('Overview', { routes: r })
  assert.match(text(container), /No bot yet/)
  assert.match(text(container), /The bot appears here once the Hermes gateway has connected it to Discord\. You can already set the operators now\./)
  assert.match(text(container), /Next step: Start the Hermes gateway with the Discord platform enabled\./)
  assert.match(text(container), /profile: default/)
  await click(button(container, 'Go to Operators'))
  assert.deepEqual(navigated, ['operators'])
})

test('overview lists the bot, protection, operators and servers with state words', async () => {
  const { container, navigated, api } = await mount('Overview', { routes: routes() })
  const all = text(container)
  assert.match(all, /Lamplighter\s*\(application 100…\)\s*● online\s*· directory live/)
  assert.match(all, /✓ armed in 1 profile \(default\) · comfort: 1 fallback/)
  assert.match(all, /1 operator configured/)
  assert.match(all, /Servers \(2\)/)
  assert.match(all, /● active\s*Example Guild\s*rev 7\s*ambient in 2 · 1 excluded · guidance 1,842\/6,000 · worklog/)
  assert.match(all, /○ not active\s*Second Server\s*the bot is in this server · not set up/)
  await click(button(container, 'View status'))
  await click(button(container, 'Edit'))
  assert.deepEqual(navigated, ['status', 'operators'])
  await type($(container, 'input[type="search"]'), 'second')
  assert.equal(byText(container, 'button', 'Example Guild'), null)
  assert.ok(byText(container, 'button', 'Second Server'))
  assert.equal(api.find('GET', '/v1/overview').length, 1)
})

test('"Activate…" asks first and activates with the row revision', async () => {
  const r = routes({ [`POST /v1/guilds/${BOT}/${GUILD2}/state`]: { guild: { state: 'active', rev: 1 } } })
  const { container, api } = await mount('Overview', { routes: r })
  const activate = $$(container, 'button').filter((b) => text(b) === 'Activate…')
  assert.equal(activate.length, 1, 'only the server that is not active offers it')
  await click(activate[0])
  assert.match(text(container), /Activate the policy in Second Server\?/)
  await click(button(container, 'Activate'))
  assert.deepEqual(api.find('POST', `/v1/guilds/${BOT}/${GUILD2}/state`)[0].body, { expected_rev: 0, state: 'active' })
  assert.match(text(container), /✓ Activated in Second Server\./)
  assert.equal(api.find('GET', '/v1/overview').length, 2, 'the list reloads')
})

test('opening a server stores the pick and navigates to Servers', async () => {
  const storage = memoryStorage()
  const { container, navigated } = await mount('Overview', { routes: routes(), storage })
  await click(byText(container, 'button', 'Example Guild'))
  assert.deepEqual(storage.data.server, { bot: BOT, guild: GUILD })
  assert.deepEqual(navigated, ['servers'])
})

test('a damaged store says why and what it means, and offers no writes', async () => {
  const r = routes({
    'GET /v1/meta': { ...routes()['GET /v1/meta'], mode: 'read-only' },
    'GET /v1/overview': overview({ mode: 'read-only', store: { state: 'corrupt', error: 'Expecting value: line 1 column 2',
      problems: ['bots.x: malformed'] } })
  })
  const { container } = await mount('Overview', { routes: r })
  const alert = byText(container, '[role="alert"]', /policy file is damaged/)
  assert.ok(alert)
  assert.match(text(alert), /⚠ The policy file is damaged and cannot be read: Expecting value: line 1 column 2/)
  assert.match(text(alert), /every server is paused/)
  assert.match(text(alert), /bots\.x: malformed/)
  assert.equal($$(container, 'button').filter((b) => /Activate/.test(text(b))).length, 0)
  assert.match(text($(container, '[data-kit="badge"]')), /read-only/)
})

test('an unreachable backend is explained with the next step', async () => {
  const transport = () => new Error("Error invoking remote method 'hermes:api': Error: connect ECONNREFUSED")
  const { container } = await mount('Overview', { routes: { 'GET /v1/meta': transport, 'GET /v1/overview': transport } })
  assert.match(text(container), /The plugin backend is not reachable\./)
  assert.match(text(container), /enable the plugin for this profile/)
  assert.ok(button(container, 'Try again'))
})

test('an offline bot shows the directory age on the overview', async () => {
  const at = Date.UTC(2031, 2, 4, 22, 5)
  const r = routes({ 'GET /v1/overview': overview({ bots: [{ ...overview().bots[0], online: false,
    directory: { source: 'snapshot', at: secs(at), stale: true } }] }) })
  const { container } = await mount('Overview', { routes: r })
  assert.match(text(container), /○ offline\s*· directory of 2031-03-04 22:05 UTC \(old\)/)
  assert.match(text(container), /The bot is offline\. Suggestions come from the directory of 2031-03-04 22:05 UTC/)
})

test('the page works with the plain fallbacks when the kit is empty', async () => {
  const { container } = await mount('Overview', { routes: routes(), kit: {} })
  assert.match(text(container), /Example Guild/)
  assert.ok(button(container, 'View status'))
})

// ── operators ────────────────────────────────────────────────────────────────────────────────────────

test('operators: chips carry names, the empty list and self-removal are explained', async () => {
  const { container } = await mount('Operators', { routes: routes() })
  assert.ok(button(container, 'Remove Opal (500…)'), 'the chip shows the resolved name')
  await click(button(container, 'Remove Opal (500…)'))
  assert.match(text(container), /⚠ You are removing 500000000000000002|⚠ You are removing Opal \(500…\)/)
  assert.match(text(container), /Removing yourself: you keep host access, but lose tool authority in Discord on the next turn\./)
  assert.match(text(container), /No operator: only public lanes; activation only from this page or the CLI\./)
})

test('operators: member search, falling back to ID entry on 503', async () => {
  const r = routes({ [`GET /v1/members/${BOT}/${GUILD}`]: httpError(503, { error: { code: 'unavailable', message: 'no way to search members' } }) })
  const { container, api, timers } = await mount('Operators', { routes: r })
  const input = $(container, '#dsp-op-operators')
  await focus(input)
  await type(input, 'al')
  await timers.advance(299)
  assert.equal(api.find('GET', `/v1/members/${BOT}/${GUILD}`).length, 0, 'debounced')
  await timers.advance(1)
  assert.deepEqual(api.find('GET', `/v1/members/${BOT}/${GUILD}`)[0].query, { q: 'al' })
  assert.match(text(container), /Member search is not available \(no way to search members\)\. Paste a user ID instead/)
  await type(input, ALICE)
  await keydown(input, 'Enter')
  assert.ok(button(container, 'Remove alice (500…)'))
})

test('operators: search results become options', async () => {
  const r = routes({ [`GET /v1/members/${BOT}/${GUILD}`]: { source: 'live', results: [
    { id: ALICE, name: 'alice', display_name: 'Alice A.', avatar: null }], note: null } })
  const { container, timers } = await mount('Operators', { routes: r })
  const input = $(container, '#dsp-op-operators')
  await focus(input)
  await type(input, 'ali')
  await timers.advance(300)
  const list = document.getElementById(input.getAttribute('aria-controls'))
  assert.equal(text($(list, '[role="option"]')), 'Alice A. · @alice · ' + ALICE)
  assert.match(text(container), /search in:/)
})

test('operators: a 409 keeps the edits until Reload; Save sends only changed keys with the digest', async () => {
  let digest = 'digest-1'
  const r = routes({
    'GET /v1/settings': () => settingsBody({ digest }),
    'PUT /v1/settings': (req) => req.body.expected_digest === digest
      ? { values: { ...settingsBody().values, ...req.body.values }, digest: 'digest-3', written: Object.keys(req.body.values) }
      : httpError(409, { error: { code: 'conflict', message: 'changed' }, digest })
  })
  const { container, api } = await mount('Operators', { routes: r })
  await type($(container, '#dsp-op-label'), 'the host')
  digest = 'digest-2'
  await click(button(container, 'Save'))
  assert.deepEqual(api.find('PUT', '/v1/settings')[0].body, { expected_digest: 'digest-1', values: { operator_label: 'the host' } })
  const banner = byText(container, '[role="alert"]', /operator settings were changed elsewhere/)
  assert.ok(banner)
  assert.match(text(banner), /Reload — your unsaved edits stay in the form until you reload\./)
  assert.equal($(container, '#dsp-op-label').value, 'the host')
  await click(button(banner, 'Reload'))
  assert.equal($(container, '#dsp-op-label').value, 'the operator')
  await type($(container, '#dsp-op-label'), 'the host')
  await click(button(container, 'Save'))
  assert.equal(api.find('PUT', '/v1/settings')[1].body.expected_digest, 'digest-2')
  assert.match(text(container), /✓ Saved: Operator label\./)
})

test('operators: a 422 lands at its field', async () => {
  const r = routes({ 'PUT /v1/settings': httpError(422, { error: { code: 'invalid', field: 'operator_label', message: 'operator_label: must be one line' } }) })
  const { container } = await mount('Operators', { routes: r })
  const input = $(container, '#dsp-op-label')
  await type(input, 'x')
  await click(button(container, 'Save'))
  assert.equal(text(document.getElementById(input.getAttribute('aria-describedby'))), '✕ operator_label: must be one line')
  assert.match(text(byText(container, '[role="alert"]', /Not saved —/)), /Operator label: operator_label: must be one line/)
})

test('operators: an unreadable configuration blocks saving and says why', async () => {
  const r = routes({ 'GET /v1/settings': settingsBody({ readable: false, error: 'config.yaml: bad indentation',
    values: { ...settingsBody().values, operators: [] } }) })
  const { container } = await mount('Operators', { routes: r })
  assert.match(text(container), /The Hermes configuration could not be read \(config\.yaml: bad indentation\)\. Nobody counts as an operator/)
  assert.equal(button(container, 'Save'), null)
})

// ── status and activity ──────────────────────────────────────────────────────────────────────────────

test('status lists the interventions by class and downloads status.json', async () => {
  const status = { status_schema: 1, pid: 4242 }
  const r = routes({
    'GET /v1/status': { status, page: { process: 'dashboard', at: secs(NOW - 60000) }, armed_profiles: ['default', 'second'],
      at: secs(NOW - 120000), pid: 4242, plugin_version: '3.0.0', hermes_version: '0.23.1',
      interventions: [{ id: 'A1[default]', klass: 'S', outcome: 'ok', reason: 'in_process', probe: 'x', at: secs(NOW - 120000) },
        { id: 'K2', klass: 'K', outcome: 'native', reason: 'Hermes does it itself', probe: 'y', at: secs(NOW - 120000) }] },
    'GET /v1/status/history': { entries: [{ at: secs(Date.UTC(2031, 2, 5, 1, 12)), id: 'K2', from: 'injected', to: 'native',
      reason: 'upstream', hermes_version: '0.23.1' }] }
  })
  const { container } = await mount('Status', { routes: r })
  const all = text(container)
  assert.match(all, /plugin 3\.0\.0 · Hermes 0\.23\.1 · gateway pid 4242 · 2 minutes ago/)
  assert.match(all, /✓ Armed in default, second\./)
  const rows = $$(container, 'tbody tr').map((tr) => $$(tr, 'td').slice(0, 4).map(text))
  assert.deepEqual(rows, [['S', 'A1[default]', 'ok', 'in_process'], ['K', 'K2', 'native', 'Hermes does it itself']])
  assert.match(all, /2031-03-05 01:12 UTC · K2 · injected → native · upstream · Hermes 0\.23\.1/)
  let saved = null
  window.URL.createObjectURL = (blob) => { saved = blob; return 'blob:x' }
  window.URL.revokeObjectURL = () => {}
  globalThis.URL.createObjectURL = window.URL.createObjectURL
  globalThis.URL.revokeObjectURL = window.URL.revokeObjectURL
  await click(button(container, 'Download status.json'))
  assert.ok(saved, 'a blob was offered for download')
  assert.deepEqual(JSON.parse(await saved.text()), status)
})

test('status without a status file says what to do', async () => {
  const { container } = await mount('Status', { routes: routes() })
  assert.match(text(container), /No status yet/)
  assert.match(text(container), /starts with this plugin enabled/)
})

test('activity says "changed" and filters by origin', async () => {
  const r = routes({ 'GET /v1/audit': { entries: [
    { at: iso(NOW - 60000), bot: BOT, guild: GUILD, field: 'progress.grouping', by: 'dashboard', label: 'the Hermes settings page', via: 'dashboard' },
    { at: iso(NOW - 120000), bot: BOT, guild: GUILD, field: 'conversation.ambient_channels', by: ALICE, label: `Discord user ${ALICE}`, via: 'discord' }
  ] } })
  const { container } = await mount('Activity', { routes: r })
  const rows = () => $$(container, 'tbody tr').map((tr) => $$(tr, 'td').map(text))
  assert.deepEqual(rows()[0].slice(1), ['Example Guild', 'progress.grouping', 'changed', 'the Hermes settings page', 'settings page'])
  assert.deepEqual(rows()[1].slice(3), ['changed', '@alice', 'Discord'])
  await click(button(container, 'Discord'))
  assert.equal(rows().length, 1)
  assert.equal(rows()[0][2], 'conversation.ambient_channels')
})

test('activity filtered to one server asks the backend for that server and shows guidance versions', async () => {
  const r = routes({ 'GET /v1/audit': (req) => ({ entries: [], guidance: req.query.guild
    ? [{ sha256: 'c'.repeat(64), by: OPAL, label: `Discord user ${OPAL}`, at: iso(NOW - 3600000), chars: 40, diff: '-old\n+new' }] : [] }) })
  const { container, api } = await mount('Activity', { routes: r })
  const pick = $(container, '#dsp-act-server')
  await focus(pick)
  await keydown(pick, 'ArrowDown')
  await keydown(pick, 'Enter')
  assert.deepEqual(api.find('GET', '/v1/audit').at(-1).query, { bot: BOT, guild: GUILD, limit: '200' })
  assert.match(text(container), /Guidance versions/)
  assert.match(text(container), /@Opal · 40 characters/)
})

// ── ctx.rest error parsing ───────────────────────────────────────────────────────────────────────────

test('the rest error parser reads the last status whose remainder is the JSON body', () => {
  const ui = createPolicyUI({ React, kit: fakeKit(), rest: async () => ({}), timers: fakeTimers(), nav: { go() {} } })
  const parse = ui.util.parseRestError
  const notFound = parse(restError(404, { error: { code: 'not_found', message: 'nope' } }))
  assert.equal(notFound.status, 404)
  assert.equal(notFound.message, 'nope')
  const conflict = parse(restError(409, { error: { code: 'conflict', message: 'Error 500: inside' }, current_rev: 9,
    updated_by: ALICE, updated_at: '2031-03-05T14:29:48Z', updated_via: 'discord' }))
  assert.equal(conflict.status, 409)
  assert.deepEqual(conflict.conflict, { rev: 9, by: ALICE, label: undefined, at: '2031-03-05T14:29:48Z', via: 'discord', digest: undefined })
  const nested = parse(new Error('Error invoking remote method \'hermes:api\': Error: 422: {"error": {"code": "invalid", "field": "a.b", "message": "x 404: y"}}'))
  assert.equal(nested.status, 422)
  assert.equal(nested.field, 'a.b')
  const html = parse(new Error('Error: 502: <html>Bad gateway</html>'))
  assert.equal(html.transport, true)
  assert.equal(html.status, 502)
  const bare = parse(new Error('net::ERR_CONNECTION_REFUSED'))
  assert.equal(bare.transport, true)
  assert.equal(bare.status, 0)
})

test('a 200 body of the form {ok: false, error} counts as an error', async () => {
  const ui = createPolicyUI({ React, kit: fakeKit(), timers: fakeTimers(), nav: { go() {} },
    rest: async () => ({ ok: false, error: { code: 'conflict', message: 'stale' }, current_rev: 3 }) })
  const r = await ui.util.call('/v1/x')
  assert.equal(r.ok, false)
  assert.equal(r.status, 409)
  assert.equal(r.conflict.rev, 3)
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})

void h
