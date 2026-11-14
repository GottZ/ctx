// Behaviour the review of the desktop half found unpinned: polling never replaces what the operator is editing,
// timers end with the page, every write names its revision, read-only after a 423, full lists take no more
// entries, names are asked for again, a deleted server that the bot left, field errors of every kind, and the
// next step after a 404.
import { afterEach, test } from 'node:test'

import { ALICE, BOT, GUILD, guildDetail, routes, settingsBody } from './fixtures.mjs'
import {
  $, $$, assert, button, byText, cleanup, click, consoleErrors, document, focus, httpError, keydown, memoryStorage,
  mount, text, type
} from './harness.mjs'

const GUILD_PATH = `/v1/guilds/${BOT}/${GUILD}`
const picked = () => memoryStorage({ server: { bot: BOT, guild: GUILD } })
const sectionButton = (key, label) => {
  const section = document.getElementById(`dsp-section-${key}`).closest('section')
  return $$(section, 'button').find((b) => text(b) === label) || null
}

afterEach(cleanup)

test('a poll while the form has unsaved edits keeps the revision the edits were made on', async () => {
  let current = guildDetail()
  const r = routes({ [`GET ${GUILD_PATH}`]: () => current, [`PATCH ${GUILD_PATH}`]: httpError(409, {
    error: { code: 'conflict', message: 'changed' }, current_rev: 9, updated_by: ALICE,
    updated_by_label: `Discord user ${ALICE}`, updated_via: 'discord', updated_at: '2031-03-05T14:29:50Z' }) })
  const { container, api, timers } = await mount('Servers', { routes: r, storage: picked() })
  await type($(container, '#dsp-field-identity-bot_name'), 'Edited')
  current = guildDetail({ rev: 9, settings: { rev: 9, identity: { bot_name: 'Changed in Discord' } } })
  await timers.advance(30000)
  assert.equal($(container, '#dsp-field-identity-bot_name').value, 'Edited')
  await click(sectionButton('identity', 'Save'))
  assert.equal(api.find('PATCH', GUILD_PATH)[0].body.expected_rev, 7, 'not the polled 9: that would overwrite Discord')
})

test('a poll while the operator settings are being edited keeps the draft and its digest', async () => {
  let body = settingsBody()
  const r = routes({ 'GET /v1/settings': () => body,
    'PUT /v1/settings': httpError(409, { error: { code: 'conflict', message: 'changed' }, digest: 'digest-2' }) })
  const { container, api, timers } = await mount('Operators', { routes: r })
  await type($(container, '#dsp-op-label'), 'the host')
  body = settingsBody({ digest: 'digest-2', values: { ...settingsBody().values, operator_label: 'changed elsewhere' } })
  await timers.advance(30000)
  assert.equal($(container, '#dsp-op-label').value, 'the host')
  await click(button(container, 'Save'))
  assert.equal(api.find('PUT', '/v1/settings')[0].body.expected_digest, 'digest-1')
})

test('leaving a page stops its polling and its focus listener', async () => {
  const page = await mount('Servers', { routes: routes(), storage: picked() })
  assert.ok(page.timers.intervals().length > 0 && page.timers.listeners().length > 0)
  await page.unmount()
  assert.equal(page.timers.intervals().length, 0)
  assert.equal(page.timers.listeners().length, 0)
})

test('restoring a guidance version names the revision; a 409 on guidance keeps the draft', async () => {
  const r = routes({
    [`POST ${GUILD_PATH}/guidance/restore`]: () => ({ guild: guildDetail({ rev: 8 }) }),
    [`POST ${GUILD_PATH}/guidance`]: httpError(409, { error: { code: 'conflict', message: 'changed' }, current_rev: 8,
      updated_by: ALICE, updated_by_label: `Discord user ${ALICE}`, updated_via: 'discord', updated_at: '2031-03-05T14:29:50Z' })
  })
  const { container, api } = await mount('Servers', { routes: r, storage: picked() })
  await click(button(container, 'Edit guidance'))
  await type($(container, '#dsp-guidance-text'), 'my draft')
  await click(button(container, 'Save'))
  assert.equal($(container, '#dsp-guidance-text').value, 'my draft', 'the draft survives the conflict')
  await click(button(container, 'Restore…'))
  await click(button(container, 'Restore'))
  assert.deepEqual(api.find('POST', `${GUILD_PATH}/guidance/restore`)[0].body, { expected_rev: 7, sha256: 'b'.repeat(64) })
})

test('a 423 makes the server read-only', async () => {
  const r = routes({ [`PATCH ${GUILD_PATH}`]: httpError(423, { error: { code: 'read_only', message: 'the policy file is damaged' } }) })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  await type($(container, '#dsp-field-identity-bot_name'), 'Edited')
  await click(sectionButton('identity', 'Save'))
  assert.ok(byText(container, '[role="alert"]', /^⚠ Read-only: /))
  assert.equal(sectionButton('identity', 'Save'), null, 'no Save while the store is read-only')
})

test('a full list takes no further entry, also not a typed one', async () => {
  const { container } = await mount('Servers', { routes: routes(), storage: picked() })
  const input = $(container, '#dsp-field-identity-wake_names')
  await focus(input)
  for (const name of ['two', 'three', 'four', 'five', 'six']) {
    await type(input, name)
    await keydown(input, 'Enter')
  }
  assert.ok(button(container, 'Remove five'))
  assert.equal(button(container, 'Remove six'), null)
  assert.match(text(container), /5\/5/)
})

test('a name the backend could not give is asked for again a minute later', async () => {
  let known = false
  const r = routes({ [`GET /v1/users/${BOT}`]: () => ({ users: known ? { [ALICE]: { name: 'alice', display_name: 'Alice' } } : {} }),
    'GET /v1/settings': settingsBody({ values: { ...settingsBody().values, operators: [ALICE] } }) })
  const { api, timers, container } = await mount('Operators', { routes: r })
  const asked = () => api.find('GET', `/v1/users/${BOT}`).length
  const first = asked()
  known = true
  await timers.advance(60000)
  assert.ok(asked() > first, 'asked again')
  assert.ok($$(container, 'button').some((b) => /^Remove Alice \(/.test(b.getAttribute('aria-label') || '')),
    'the chip shows the name now')
})

test('deleting the settings of a server the bot has left says so and offers the way back', async () => {
  let gone = false
  const r = routes({
    [`GET ${GUILD_PATH}`]: () => (gone ? httpError(404, { error: { code: 'not_found', message: 'This server is not known for this bot.' } }) : guildDetail()),
    [`POST ${GUILD_PATH}/state`]: () => { gone = true; return { guild: null, purged: true } }
  })
  const { container, storage } = await mount('Servers', { routes: r, storage: picked() })
  await click(button(container, 'Delete settings…'))
  await type($(container, '#dsp-purge-name'), 'Example Guild')
  await click(button(container, 'Delete settings'))
  assert.match(text(container), /✓ Settings deleted\. The bot is no longer in this server/)
  assert.equal($(container, '#dsp-field-identity-bot_name'), null, 'no form with the deleted values')
  await click(button(container, 'Choose another server'))
  assert.equal('server' in storage.data, false, 'the pick is cleared')
})

test('a 422 on a choice field is described at the field and its summary link focuses it', async () => {
  const r = routes({ [`PATCH ${GUILD_PATH}`]: httpError(422, { error: { code: 'invalid', field: 'worklog.archive_minutes',
    message: 'worklog.archive_minutes: not a supported value' } }) })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  await click(button(container, '1 hour'))
  await click(sectionButton('worklog', 'Save'))
  const group = document.getElementById('dsp-field-worklog-archive_minutes')
  assert.ok(group, 'the control carries the field id')
  const described = document.getElementById(group.getAttribute('aria-describedby'))
  assert.match(text(described), /not a supported value/)
  const link = $$(byText(container, '[role="alert"]', /Not saved —/), 'button').find((b) => text(b) === 'Archive after')
  await click(link)
  assert.ok(group.contains(document.activeElement), 'focus is in the field')
})

test('a 404 of a server says what to do, not "enable the plugin"', async () => {
  const r = routes({ [`GET ${GUILD_PATH}`]: httpError(404, { error: { code: 'not_found', message: 'This server is not known for this bot.' } }) })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  const all = text(container)
  assert.match(all, /This server is not known to this profile\./)
  assert.doesNotMatch(all, /enable the plugin/)
  assert.match(all, /Next step: choose another server/)
})


test('profile and model sections of the catalogue are shown read-only with the way to change them', async () => {
  const { schema } = await import('./fixtures.mjs')
  const base = schema()
  const extra = [
    { key: 'profile', label: 'Hermes profile', note: '', fields: [
      { path: 'routing.profile', label: 'Profile', help: '', kind: 'readonly', default: null }] },
    { key: 'model', label: 'Models', note: '', fields: [
      { path: 'models.public', label: 'Public model', help: '', kind: 'readonly', default: null }] },
    { key: 'model.pick', label: 'Model pick', note: '', fields: [
      { path: 'models.manager_pick', label: 'Manager pick', help: '', kind: 'readonly', default: null }] }
  ]
  const detail = guildDetail({ settings: { routing: { profile: 'work', set_by: 'dashboard', set_at: null },
    models: { public: { provider: 'openai', model: 'gpt-x' }, operator: null, public_choices: [], on_unavailable: 'fallback', manager_pick: null } } })
  const r = routes({ 'GET /v1/schema': { ...base, sections: [...base.sections, ...extra] }, [`GET ${GUILD_PATH}`]: detail })
  const { container } = await mount('Servers', { routes: r, storage: picked() })
  for (const key of ['profile', 'model', 'model.pick']) {
    const section = document.getElementById(`dsp-section-${key.replace('.', '-')}`).closest('section')
    assert.match(text(section), /Chosen in Discord for now: \/policy/, key)
    assert.equal($$(section, 'button').filter((b) => /^(Save|Reset section)$/.test(text(b))).length, 0, key)
  }
  assert.match(text(document.getElementById('dsp-section-profile').closest('section')), /Runs in the Hermes profile work\./)
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})
