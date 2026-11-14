// After "Delete settings" the store keeps a revision epoch for the server (revisions never repeat); the page
// takes the revision from the reloaded server, so activating it again passes the optimistic check.
import { afterEach, test } from 'node:test'

import { BOT, GUILD, guildDetail, routes } from './fixtures.mjs'
import { $, assert, button, cleanup, click, consoleErrors, memoryStorage, mount, type } from './harness.mjs'

const GUILD_PATH = `/v1/guilds/${BOT}/${GUILD}`

afterEach(cleanup)

test('a deleted server is activated again with the revision the reloaded page shows', async () => {
  let current = guildDetail()
  const r = routes({
    [`GET ${GUILD_PATH}`]: () => current,
    [`POST ${GUILD_PATH}/state`]: (req) => {
      if (req.body.purge) {
        current = guildDetail({ configured: false, state: 'unbound', rev: 8 })
        return { guild: null, purged: true }
      }
      current = guildDetail({ rev: 9, state: 'active' })
      return { guild: current }
    }
  })
  const { container, api } = await mount('Servers', { routes: r, storage: memoryStorage({ server: { bot: BOT, guild: GUILD } }) })
  await click(button(container, 'Delete settings…'))
  await type($(container, '#dsp-purge-name'), 'Example Guild')
  await click(button(container, 'Delete settings'))
  await click(button(container, 'Activate'))
  const writes = api.find('POST', `${GUILD_PATH}/state`).map((c) => c.body)
  assert.deepEqual(writes, [{ expected_rev: 7, state: 'unbound', purge: true }, { expected_rev: 8, state: 'active' }])
})

test('no React warnings were logged', () => {
  assert.deepEqual(consoleErrors, [])
})
