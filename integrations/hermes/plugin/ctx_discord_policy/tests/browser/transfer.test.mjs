// Settings file of one server: download (export) and import (SETTINGS-PAGE §3.3, §3.4). An import sends the
// revision the page shows; importing exactly the settings in effect is reported as no change.
import { afterEach, test } from 'node:test'

import { BOT, GUILD, guildDetail, routes } from './fixtures.mjs'
import {
  $, assert, button, byText, cleanup, click, consoleErrors, document, httpError, memoryStorage, mount, settle, text,
  type, window
} from './harness.mjs'

const GUILD_PATH = `/v1/guilds/${BOT}/${GUILD}`
const picked = () => memoryStorage({ server: { bot: BOT, guild: GUILD } })
const EXPORT = { schema: 1, bots: { [BOT]: { guilds: { [GUILD]: guildDetail().settings } } } }
const status = () => document.getElementById('dsp-transfer-status')

afterEach(cleanup)

async function choose(container, name, content) {
  const input = $(container, 'input[type="file"][accept=".json,application/json"]')
  assert.ok(input, 'a JSON file input exists')
  const file = new window.File([content], name, { type: 'application/json' })
  Object.defineProperty(input, 'files', { value: [file], configurable: true })
  input.dispatchEvent(new window.Event('change', { bubbles: true }))
  await settle()
}

test('Download saves the server’s settings file', async () => {
  const { container, api } = await mount('Servers', { routes: routes({ [`GET ${GUILD_PATH}/export`]: EXPORT }),
    storage: picked() })
  let saved = null
  window.URL.createObjectURL = (blob) => { saved = blob; return 'blob:x' }
  window.URL.revokeObjectURL = () => {}
  globalThis.URL.createObjectURL = window.URL.createObjectURL
  globalThis.URL.revokeObjectURL = window.URL.revokeObjectURL
  await click(button(container, 'Download settings (.json)'))
  assert.equal(api.find('GET', `${GUILD_PATH}/export`).length, 1)
  assert.ok(saved, 'a blob was offered for download')
  assert.deepEqual(JSON.parse(await saved.text()), EXPORT)
  assert.equal(text(status()), `✓ Downloaded ctx_discord_policy-${GUILD}.json.`)
})

test('importing the settings in effect is reported as no change', async () => {
  const r = routes({ [`POST ${GUILD_PATH}/import`]: () => ({ guild: guildDetail(), warnings: [], changed: false }) })
  const { container, api } = await mount('Servers', { routes: r, storage: picked() })
  await choose(container, 'same.json', JSON.stringify(EXPORT))
  const [sent] = api.find('POST', `${GUILD_PATH}/import`)
  assert.deepEqual(sent.body, { expected_rev: guildDetail().rev, document: EXPORT })
  assert.equal(text(status()), `ⓘ Nothing imported: the file holds exactly the settings in effect (rev ${guildDetail().rev}).`)
  assert.equal(document.activeElement, status(), 'focus moves to the result')
})

test('a changed file is imported and the page shows the new revision', async () => {
  const next = guildDetail({ rev: 8, settings: { rev: 8, identity: { bot_name: 'Imported' } } })
  const r = routes({ [`POST ${GUILD_PATH}/import`]: () => ({ guild: next, warnings: [], changed: true }) })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  await choose(container, 'other.json', JSON.stringify(EXPORT))
  assert.equal(text(status()), '✓ Imported (rev 8).')
  assert.match(text(container), /· rev 8/)
  assert.equal($(container, '#dsp-field-identity-bot_name').value, 'Imported')
})

test('a file that is not JSON is refused in the browser and nothing is sent', async () => {
  const { container, api } = await mount('Servers', { routes: routes(), storage: picked() })
  await choose(container, 'broken.json', '{not json')
  assert.equal(api.find('POST', `${GUILD_PATH}/import`).length, 0)
  assert.equal(text(status()), '✕ Not imported: the file is not JSON.')
})

test('a file over 1 MB is refused in the browser', async () => {
  const { container, api } = await mount('Servers', { routes: routes(), storage: picked() })
  await choose(container, 'big.json', '"' + 'x'.repeat(1024 * 1024) + '"')
  assert.equal(api.find('POST', `${GUILD_PATH}/import`).length, 0)
  assert.equal(text(status()), '✕ Not imported: the file is larger than 1 MB.')
})

test('an import that lost a race shows the conflict notice', async () => {
  const r = routes({ [`POST ${GUILD_PATH}/import`]: httpError(409, { error: { code: 'conflict', message: 'changed' },
    current_rev: 9, updated_by: 'dashboard', updated_by_label: 'the Hermes settings page', updated_via: 'dashboard',
    updated_at: '2031-03-05T14:29:50Z' }) })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  await choose(container, 'other.json', JSON.stringify(EXPORT))
  assert.match(text(status()), /^✕ Not imported: this server's settings changed elsewhere/)
  assert.ok(byText(container, '[role="alert"]', /^⚠ Not saved: the Hermes settings page changed this server's settings/),
    'the conflict notice is shown')
})

test('with unsaved edits the import waits for them', async () => {
  const { container } = await mount('Servers', { routes: routes(), storage: picked() })
  await type($(container, '#dsp-field-identity-bot_name'), 'Edited')
  const importButton = button(container, 'Import settings…')
  assert.ok(importButton.disabled, 'Import is disabled while the form has unsaved edits')
  assert.match(text(container), /Save or discard your unsaved changes before importing\./)
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})
