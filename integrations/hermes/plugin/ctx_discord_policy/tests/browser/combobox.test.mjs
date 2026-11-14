// Combobox keyboard and ARIA behaviour (SETTINGS-PAGE §5.7), exercised through the Servers page's
// "Ambient listening" field so the directory wiring (groups, hints, free IDs) is covered too.
import { assert } from './harness.mjs'
import { afterEach, test } from 'node:test'

import { BOT, GUILD, ch, routes } from './fixtures.mjs'
import { $, $$, button, cleanup, click, consoleErrors, document, focus, keydown, memoryStorage, mount, text, type } from './harness.mjs'

const FIELD = '#dsp-field-conversation-ambient_channels'

async function openServer() {
  const page = await mount('Servers', { routes: routes(), storage: memoryStorage({ server: { bot: BOT, guild: GUILD } }) })
  const input = $(page.container, FIELD)
  assert.ok(input, 'the ambient channel combobox is rendered')
  return { ...page, input }
}

const listbox = (input) => document.getElementById(input.getAttribute('aria-controls'))
const options = (input) => $$(listbox(input), '[role="option"]')
const active = (input) => document.getElementById(input.getAttribute('aria-activedescendant'))
const live = (input) => document.getElementById(input.id + '-live')

afterEach(cleanup)

test('the input follows the WAI-ARIA combobox pattern', async () => {
  const { input } = await openServer()
  assert.equal(input.getAttribute('role'), 'combobox')
  assert.equal(input.getAttribute('aria-autocomplete'), 'list')
  assert.equal(input.getAttribute('aria-expanded'), 'false')
  assert.equal(input.hasAttribute('aria-activedescendant'), false)
  const list = listbox(input)
  assert.ok(list, 'aria-controls names an element that exists')
  assert.equal(list.getAttribute('role'), 'listbox')
  assert.equal(document.getElementById(input.getAttribute('aria-labelledby')).textContent, 'Ambient listening')
})

test('ArrowDown/ArrowUp open the list and move the active option; Home/End jump', async () => {
  const { input } = await openServer()
  await focus(input)
  await keydown(input, 'ArrowDown')
  assert.equal(input.getAttribute('aria-expanded'), 'true')
  const opts = options(input)
  assert.equal(opts.length, 12)
  assert.equal(active(input), opts[0])
  assert.equal(opts[0].getAttribute('aria-selected'), 'true')
  assert.ok(opts.slice(1).every((o) => o.getAttribute('aria-selected') === 'false'))
  assert.match(text(opts[0]), /^#general/)
  await keydown(input, 'ArrowDown')
  assert.equal(active(input), options(input)[1])
  await keydown(input, 'ArrowUp')
  assert.equal(active(input), options(input)[0])
  await keydown(input, 'ArrowUp')
  assert.equal(active(input), options(input)[11], 'ArrowUp on the first option wraps to the last')
  await keydown(input, 'Home')
  assert.equal(active(input), options(input)[0])
  await keydown(input, 'End')
  assert.equal(active(input), options(input)[11])
})

test('ArrowUp on a closed list opens it on the last option', async () => {
  const { input } = await openServer()
  await focus(input)
  await keydown(input, 'ArrowUp')
  assert.equal(input.getAttribute('aria-expanded'), 'true')
  assert.equal(active(input), options(input)[11])
})

test('a live region counts the suggestions', async () => {
  const { input } = await openServer()
  await focus(input)
  await keydown(input, 'ArrowDown')
  assert.equal(live(input).getAttribute('aria-live'), 'polite')
  assert.equal(text(live(input)), '12 channels found')
  await type(input, 'mem')
  assert.equal(text(live(input)), '1 channel found')
  await type(input, 'zzz')
  assert.equal(text(live(input)), 'No channels found')
})

test('channels are grouped by category with labelled groups', async () => {
  const { input } = await openServer()
  await focus(input)
  await keydown(input, 'ArrowDown')
  const groups = $$(listbox(input), '[role="group"]')
  assert.deepEqual(groups.map((g) => text(document.getElementById(g.getAttribute('aria-labelledby')))), ['Text Channels', 'Community'])
  assert.equal($$(groups[0], '[role="option"]').length, 6)
  assert.equal($$(groups[1], '[role="option"]').length, 6)
  const hints = $$(groups[1], '[role="option"]').map(text)
  assert.ok(hints.includes('#announcements · announcement'))
  assert.ok(hints.includes('#mod-log · the bot cannot write here'), 'permission problems are said in words')
})

test('Enter accepts the active option as a chip with a remove button', async () => {
  const { input, container } = await openServer()
  await focus(input)
  await keydown(input, 'ArrowDown')
  await keydown(input, 'Enter')
  assert.equal(input.getAttribute('aria-expanded'), 'false')
  assert.equal(input.value, '')
  const chip = button(container, 'Remove #general')
  assert.ok(chip, 'chip with aria-label "Remove #general"')
  assert.equal(chip.tagName, 'BUTTON')
  const status = document.getElementById('dsp-section-channels-status')
  assert.equal(text(status), '● Unsaved changes')
  await focus(input)
  await keydown(input, 'ArrowDown')
  assert.equal(options(input).length, 11, 'a chosen channel is not offered again')
})

test('Escape closes the list; a second Escape clears the text', async () => {
  const { input } = await openServer()
  await focus(input)
  await type(input, 'gen')
  assert.equal(input.getAttribute('aria-expanded'), 'true')
  await keydown(input, 'Escape')
  assert.equal(input.getAttribute('aria-expanded'), 'false')
  assert.equal(input.value, 'gen')
  await keydown(input, 'Escape')
  assert.equal(input.value, '')
})

test('Tab closes the list without accepting the active option', async () => {
  const { input, container } = await openServer()
  await focus(input)
  await type(input, 'mem')
  await keydown(input, 'ArrowDown')
  assert.match(text(active(input)), /#memes/)
  const event = await keydown(input, 'Tab')
  assert.equal(event.defaultPrevented, false, 'Tab keeps its default: focus leaves the field')
  assert.equal(input.getAttribute('aria-expanded'), 'false')
  assert.equal(button(container, 'Remove #memes'), null)
})

test('Backspace in the empty field removes the last chip; chip buttons remove theirs', async () => {
  const { input, container } = await openServer()
  await focus(input)
  for (const name of ['general', 'memes', 'help']) {
    await type(input, name)
    await keydown(input, 'ArrowDown')
    await keydown(input, 'Enter')
  }
  assert.ok(button(container, 'Remove #help'))
  await keydown(input, 'Backspace')
  assert.equal(button(container, 'Remove #help'), null)
  assert.ok(button(container, 'Remove #memes'))
  await type(input, 'x')
  await keydown(input, 'Backspace')
  assert.ok(button(container, 'Remove #memes'), 'Backspace with text in the field edits the text only')
  await click(button(container, 'Remove #general'))
  assert.equal(button(container, 'Remove #general'), null)
  assert.equal(document.activeElement, input, 'focus returns to the input')
})

test('a typed or pasted ID of 17–20 digits is offered as "Use ID …"', async () => {
  const { input, container } = await openServer()
  await focus(input)
  await type(input, '1234567890123456')
  assert.equal(options(input).length, 0, '16 digits are not an ID')
  const id = '123456789012345678'
  await type(input, id)
  const opts = options(input)
  assert.equal(text(opts[0]), 'Use ID ' + id)
  await keydown(input, 'Enter')
  assert.ok(button(container, 'Remove ' + id))
  assert.match(text(container), /1 ID is not in the directory \(123456789012345678\); that is fine if the channel is new\./)
})

test('an ID the directory knows is offered under its name, not as a free ID', async () => {
  const { input } = await openServer()
  await focus(input)
  await type(input, ch(2))
  const opts = options(input)
  assert.equal(opts.length, 1)
  assert.match(text(opts[0]), /^#memes/)
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})
