// Server detail: per-section save, the 409 conflict banner, 422 errors at the field, the offline notice,
// lifecycle and the guidance editor (SETTINGS-PAGE §5.2, §5.3, §5.7).
import { assert } from './harness.mjs'
import { afterEach, test } from 'node:test'

import { ALICE, BOT, GUILD, GUILD2, ch, directory, guildDetail, iso, overview, routes, secs } from './fixtures.mjs'
import {
  $, $$, NOW, button, byText, cleanup, click, consoleErrors, document, focus, httpError, keydown, memoryStorage, mount,
  settle, text, type
} from './harness.mjs'

const GUILD_PATH = `/v1/guilds/${BOT}/${GUILD}`
const picked = () => memoryStorage({ server: { bot: BOT, guild: GUILD } })

async function addAmbient(container, name) {
  const input = $(container, '#dsp-field-conversation-ambient_channels')
  await focus(input)
  await type(input, name)
  await keydown(input, 'ArrowDown')
  await keydown(input, 'Enter')
}

const sectionSave = (container, key) => {
  const section = document.getElementById(`dsp-section-${key}`).closest('section')
  return $$(section, 'button').find((b) => text(b) === 'Save')
}

afterEach(cleanup)

test('Save sends only the section’s changed paths with expected_rev and focuses the status line', async () => {
  const r = routes({
    [`PATCH ${GUILD_PATH}`]: (req) => ({ guild: guildDetail({ rev: 8, settings: { rev: 8,
      conversation: { ambient_channels: req.body.changes['conversation.ambient_channels'], excluded_channels: [ch(10)],
        language: 'the language of the message' } } }), warnings: [] })
  })
  const { container, api } = await mount('Servers', { routes: r, storage: picked() })
  assert.match(text(container), /Example Guild/)
  assert.equal(sectionSave(container, 'channels').disabled, true, 'nothing to save yet')
  await addAmbient(container, 'general')
  await click(sectionSave(container, 'channels'))
  const [patch] = api.find('PATCH', GUILD_PATH)
  assert.deepEqual(patch.body, { expected_rev: 7, section: 'channels', changes: { 'conversation.ambient_channels': [ch(1)] } })
  const status = document.getElementById('dsp-section-channels-status')
  assert.equal(text(status), '✓ Saved (rev 8).')
  assert.equal(document.activeElement, status, 'focus moves to the section status line')
  assert.ok(byText(container, 'span', '● active · rev 8'))
})

test('a 409 shows who changed what and keeps the unsaved edits until Reload', async () => {
  let current = guildDetail()
  const r = routes({
    [`GET ${GUILD_PATH}`]: () => current,
    [`PATCH ${GUILD_PATH}`]: httpError(409, {
      error: { code: 'conflict', message: 'guild changed (rev 9, you had 7); reload' },
      current_rev: 9, updated_by: ALICE, updated_by_label: `Discord user ${ALICE}`, updated_at: iso(NOW - 12000),
      updated_via: 'discord'
    })
  })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  await addAmbient(container, 'memes')
  await click(sectionSave(container, 'channels'))
  const banner = byText(container, '[role="alert"]', /Not saved/)
  assert.ok(banner, 'conflict banner')
  assert.equal(text(banner), '⚠ Not saved: @alice changed this server\'s settings 12 seconds ago (Discord), after you opened ' +
    'this page. Reload — your unsaved edits stay in the form until you reload.')
  assert.ok(button(container, 'Remove #memes'), 'the edit is still in the form')
  assert.match(text(document.getElementById('dsp-section-channels-status')), /^✕ Not saved/)
  current = guildDetail({ rev: 9, settings: { rev: 9 } })
  await click(button(banner, 'Reload'))
  assert.equal(byText(container, '[role="alert"]', /Not saved/), null)
  assert.equal(button(container, 'Remove #memes'), null, 'Reload drops the unsaved edits')
  assert.ok(byText(container, 'span', '● active · rev 9'))
})

test('a 422 is shown at its field (aria-describedby) and in a summary', async () => {
  const r = routes({
    [`PATCH ${GUILD_PATH}`]: httpError(422, { error: { code: 'invalid', field: 'identity.bot_name',
      message: 'identity.bot_name: control characters are not allowed' } })
  })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  const input = $(container, '#dsp-field-identity-bot_name')
  await type(input, 'Lamp two')
  await click(sectionSave(container, 'identity'))
  const describedBy = input.getAttribute('aria-describedby').split(' ')
  const error = describedBy.map((id) => document.getElementById(id)).find((el) => el && /control characters/.test(el.textContent))
  assert.ok(error, 'the error text is referenced by aria-describedby')
  assert.equal(text(error), '✕ identity.bot_name: control characters are not allowed')
  assert.equal(input.getAttribute('aria-invalid'), 'true')
  const summary = byText(container, '[role="alert"]', /Not saved — 1 problem/)
  assert.ok(summary)
  assert.match(text(summary), /Name the bot uses here: identity\.bot_name: control characters are not allowed/)
  const status = document.getElementById('dsp-section-identity-status')
  assert.equal(document.activeElement, status)
  assert.equal(text(status), '✕ Not saved: 1 problem, listed below.')
  await click(button(summary, 'Name the bot uses here'))
  assert.equal(document.activeElement, input, 'the summary entry leads to the field')
})

test('findings of a 422 land at their fields; warnings of a 200 are shown too', async () => {
  let fail = true
  const r = routes({
    [`PATCH ${GUILD_PATH}`]: () => fail
      ? httpError(422, { error: { code: 'invalid', message: 'Some values are not available.', findings: [
        { level: 'error', field: 'lanes.skills[0]', code: 'skill_missing', message: 'The skill nope is not installed in profile default.' }
      ] } })
      : { guild: guildDetail({ rev: 8, settings: { rev: 8 } }), warnings: [
        { level: 'warning', field: 'lanes.skills', code: 'x', message: 'Heads up.' }] }
  })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  const input = $(container, '#dsp-field-lanes-skills')
  await focus(input)
  await type(input, 'nope')
  assert.equal(text($$(document.getElementById(input.getAttribute('aria-controls')), '[role="option"]')[0]), 'Use skill “nope”')
  await keydown(input, 'Enter')
  await click(sectionSave(container, 'public_lanes'))
  assert.match(text(container), /✕ The skill nope is not installed in profile default\./)
  assert.ok(input.getAttribute('aria-describedby').split(' ').some((id) => /not installed/.test(document.getElementById(id)?.textContent || '')))
  fail = false
  await click(sectionSave(container, 'public_lanes'))
  assert.match(text(document.getElementById('dsp-section-public_lanes-status')), /✓ Saved \(rev 8\)\. 1 warning at the fields\./)
  assert.match(text(container), /⚠ Heads up\./)
})

test('with the bot offline the page says where suggestions come from and how old they are', async () => {
  const at = Date.UTC(2031, 2, 5, 1, 12)
  const r = routes({
    'GET /v1/meta': { ...routes()['GET /v1/meta'], mode: 'snapshot' },
    'GET /v1/overview': overview({ bots: [{ ...overview().bots[0], online: false,
      directory: { source: 'snapshot', at: secs(at), stale: true } }] }),
    [`GET /v1/directory/${BOT}/${GUILD}`]: directory({ source: 'snapshot', at: secs(at), stale: true })
  })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  const banner = byText(container, '[role="status"]', /The bot is offline/)
  assert.ok(banner)
  assert.equal(text(banner), 'ⓘ The bot is offline. Suggestions come from the directory of 2031-03-05 01:12 UTC; channels ' +
    'created since then are missing. You can paste an ID instead.')
  assert.ok(byText(container, 'p', 'Suggestions: directory of 01:12 UTC (old)'))
  assert.match(text($(container, '[data-kit="badge"]')), /directory/)
})

test('Reset section asks first; a 501 disables it with a visible reason', async () => {
  const r = routes({ [`POST ${GUILD_PATH}/reset`]: httpError(501, { error: { code: 'not_available', message: 'not in this build' } }) })
  const { container, api } = await mount('Servers', { routes: r, storage: picked() })
  const section = document.getElementById('dsp-section-progress').closest('section')
  await click(button(section, 'Reset section'))
  assert.match(text(section), /Reset Progress to the defaults\?/)
  await click(button(section, 'Reset'))
  assert.deepEqual(api.find('POST', `${GUILD_PATH}/reset`)[0].body, { expected_rev: 7, section: 'progress' })
  assert.equal(text(document.getElementById('dsp-section-progress-status')), '✕ Resetting a section is not available in this version.')
  const resets = $$(container, 'button').filter((b) => text(b) === 'Reset section')
  assert.ok(resets.length > 1 && resets.every((b) => b.disabled), 'every Reset section button is disabled')
  assert.ok(byText(section, 'span', 'Reset is not available in this version.'), 'the reason is visible, not only a tooltip')
})

test('routing is reserved and says it is not available in this version', async () => {
  const { container } = await mount('Servers', { routes: routes(), storage: picked() })
  const routing = document.getElementById('dsp-section-routing').closest('section')
  assert.match(text(routing), /Not available in this version/)
  assert.equal($$(routing, 'button').length, 0)
})

test('choice fields use the segmented control and save their typed value', async () => {
  const r = routes({ [`PATCH ${GUILD_PATH}`]: { guild: guildDetail({ rev: 8, settings: { rev: 8 } }), warnings: [] } })
  const { container, api } = await mount('Servers', { routes: r, storage: picked() })
  const section = document.getElementById('dsp-section-worklog').closest('section')
  await click(button(section, '3 days'))
  await click(sectionSave(container, 'worklog'))
  assert.deepEqual(api.find('PATCH', GUILD_PATH)[0].body.changes, { 'worklog.archive_minutes': 4320 })
})

test('the last pick is remembered and the picker changes it', async () => {
  const storage = picked()
  const { container } = await mount('Servers', { routes: routes(), storage })
  const pick = $(container, '#dsp-server-pick')
  assert.ok(button(container, 'Remove Example Guild'), 'the stored pick is shown')
  await focus(pick)
  await type(pick, 'Second')
  await keydown(pick, 'ArrowDown')
  await keydown(pick, 'Enter')
  assert.deepEqual(storage.data.server, { bot: BOT, guild: GUILD2 })
})

test('Delete settings needs the server name typed', async () => {
  const r = routes({ [`POST ${GUILD_PATH}/state`]: { guild: null, purged: true } })
  const { container, api } = await mount('Servers', { routes: r, storage: picked() })
  await click(button(container, 'Delete settings…'))
  const confirm = button(container, 'Delete settings')
  assert.equal(confirm.disabled, true)
  await type($(container, '#dsp-purge-name'), 'Example')
  assert.equal(button(container, 'Delete settings').disabled, true)
  await type($(container, '#dsp-purge-name'), 'Example Guild')
  await click(button(container, 'Delete settings'))
  assert.deepEqual(api.find('POST', `${GUILD_PATH}/state`)[0].body, { expected_rev: 7, state: 'unbound', purge: true })
  assert.match(text(document.getElementById('dsp-lifecycle-status')), /Settings deleted/)
})

test('the guidance editor previews after 400 ms, separates the sections and warns about unknown placeholders', async () => {
  const r = routes({ [`POST ${GUILD_PATH}/guidance`]: (req) => ({ guild: guildDetail({ rev: 8, settings: { rev: 8,
    prompts: { guild: req.body.text, guild_in_operator_lane: false, history: [], template_overrides: {} } } }), history: true }) })
  const { container, api, timers } = await mount('Servers', { routes: r, storage: picked() })
  await click(button(container, 'Edit guidance'))
  const ta = $(container, '#dsp-guidance-text')
  assert.equal(ta.value, 'Be kind.')
  await timers.advance(400)
  assert.equal(api.find('POST', '/v1/preview').length, 1)
  await type(ta, 'Talk about {server_name}. {botname}')
  await timers.advance(399)
  assert.equal(api.find('POST', '/v1/preview').length, 1, 'debounced: nothing before 400 ms')
  assert.match(text(container), /\(provisional, checking…\)/)
  await timers.advance(1)
  const previews = api.find('POST', '/v1/preview')
  assert.equal(previews.length, 2)
  assert.deepEqual(previews[1].body, { bot: BOT, guild: GUILD, lane: 'public', mode: 'ambient', draft: 'Talk about {server_name}. {botname}' })
  const headings = $$(container, '#dsp-guidance-preview h5').map(text)
  assert.deepEqual(headings, ['1. Operator frame', '2. Capability lane', '3. Conversation lane', '4. Server guidance'])
  assert.match(text(container), /⚠ Unknown placeholder \{botname\} — it stays as written\./)
  assert.match(text(container), /35 \/ 6,000 characters · in the context 39 · room left 3,410/)
  assert.ok($(container, '[role="img"][aria-label^="Left to right: frame and lanes"]'), 'budget bar with a text alternative')
  await click(button(container, 'operator · direct'))
  await timers.advance(400)
  assert.deepEqual(api.find('POST', '/v1/preview').at(-1).body.lane, 'operator')
  await click(button(container, 'Save'))
  assert.deepEqual(api.find('POST', `${GUILD_PATH}/guidance`)[0].body, { expected_rev: 7, text: 'Talk about {server_name}. {botname}' })
  const status = document.getElementById('dsp-section-guidance-status')
  assert.equal(text(status), '✓ Guidance saved (rev 8).')
  assert.equal(document.activeElement, status)
})

test('a guidance file over 64 KB is refused in the browser; a small one fills the editor only', async () => {
  const { container, api } = await mount('Servers', { routes: routes(), storage: picked() })
  await click(button(container, 'Edit guidance'))
  const file = $(container, 'input[type="file"][accept^=".md"]')
  const { window } = await import('./harness.mjs')
  const big = new window.File(['x'.repeat(64 * 1024 + 1)], 'big.md', { type: 'text/markdown' })
  Object.defineProperty(file, 'files', { value: [big], configurable: true })
  file.dispatchEvent(new window.Event('change', { bubbles: true }))
  await settle()
  assert.match(text(container), /✕ The file is larger than 64 KB; nothing was loaded\./)
  const small = new window.File(['Hello from a file\r\n'], 'g.md', { type: 'text/markdown' })
  Object.defineProperty(file, 'files', { value: [small], configurable: true })
  file.dispatchEvent(new window.Event('change', { bubbles: true }))
  await settle()
  assert.equal($(container, '#dsp-guidance-text').value, 'Hello from a file\n')
  assert.equal(api.find('POST', `${GUILD_PATH}/guidance`).length, 0, 'loading a file saves nothing')
})

test('polling refreshes every 30 s and on focus, never faster', async () => {
  const { api, timers } = await mount('Servers', { routes: routes(), storage: picked() })
  assert.ok(timers.intervals().length > 0)
  assert.ok(timers.intervals().every((i) => i.every >= 30000))
  assert.ok(timers.listeners().some((l) => l.type === 'focus'))
  const before = api.find('GET', GUILD_PATH).length
  await timers.advance(30000)
  assert.equal(api.find('GET', GUILD_PATH).length, before + 1)
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})
