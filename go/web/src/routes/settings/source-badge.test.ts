// Issue #495: every provenance the settings API reports (SettingSource,
// handler/settings.go apiSource) except the built-in default renders as a
// styled badge — SettingField.svelte gives the badge the class
// source-{setting.source}. The source "install" (dream.enabled on a new
// installation, migration 192) arrived without a rule and rendered unstyled.
// A source added to the union without a rule turns this red.
import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

const types = readFileSync(new URL('../../lib/api/types.ts', import.meta.url), 'utf8')
const field = readFileSync(new URL('./SettingField.svelte', import.meta.url), 'utf8')
const union = /export type SettingSource =([^\n]+)/.exec(types)
const sources = union ? [...union[1].matchAll(/'([a-z]+)'/g)].map((m) => m[1]) : []

describe('settings source badge', () => {
  it('reads the SettingSource union', () => {
    expect(sources).toEqual(expect.arrayContaining(['default', 'env', 'db', 'install']))
  })
  for (const source of sources.filter((s) => s !== 'default')) {
    it(`styles the badge of source ${source}`, () => {
      expect(field).toMatch(new RegExp(`\\.badge\\.source-${source}\\s*\\{[^}]*\\bcolor:`))
    })
  }
})
