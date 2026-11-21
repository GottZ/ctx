// The test plan of the contract runner (#457): planContract is the one list
// definePageContract registers and renderCoverage counts. Two pins keep it
// honest, so a family or a state that drops out of the generation turns this
// file red instead of shrinking the suite unnoticed (on the base of #454 a
// mutant that dropped the mobile fit tests took them from 9 to 0 and every
// permanent test stayed green):
//
//   1. the plan against the §4.1 table — the families, states, themes and
//      viewports each contract must generate, written from the contract data
//      as the design states it, not from planContract;
//   2. the plan against Playwright's own collection (`playwright test --list`,
//      collection only: no browser, no webServer) — what definePageContract
//      really registers under `contract:<name>`, by title and tag.

import { execFileSync } from 'node:child_process'
import { describe, expect, it } from 'vitest'
import { denyRoleFor, isScaleState, planContract, THEMES, type PageContract, type PlannedTest } from './contract'
import { contracts } from './registry'

const WEB = new URL('../..', import.meta.url).pathname

/**
 * family|state|theme|viewport|title of a planned test — the §4.1 coordinates
 * and the frozen title (it keys the test id, the CI history and the
 * quarantine ledger, so a changed title template turns this red).
 */
function key(t: Pick<PlannedTest, 'family' | 'viewport' | 'title'> & { state?: string; theme?: string }): string {
  return `${t.family}|${t.state ?? '-'}|${t.theme ?? '-'}|${t.viewport}|${t.title}`
}

/** The design 06 §4.1 table for one contract, from its data. */
function expectedKeys(c: PageContract): string[] {
  const keys: string[] = []
  const n = c.name
  const visualStates = [...c.states.map((s) => s.name), ...(isScaleState(c.scale) ? [c.scale.name] : [])]
  keys.push(key({ family: 'flow', state: 'default', theme: 'dark', viewport: 'desktop', title: `${n} primary flow` }))
  for (const s of visualStates) {
    for (const theme of THEMES) {
      keys.push(key({ family: 'visual', state: s, theme, viewport: 'desktop', title: `${n} ${s} ${theme} desktop visual` }))
    }
    // every visual state is checked at 390 px: captured in both themes, or —
    // a reasoned mobile opt-out — fit-checked once without a capture
    if (c.mobile === undefined) {
      for (const theme of THEMES) {
        keys.push(key({ family: 'visual', state: s, theme, viewport: 'mobile', title: `${n} ${s} ${theme} mobile visual` }))
      }
    } else {
      keys.push(key({ family: 'mobile-fit', state: s, theme: 'dark', viewport: 'mobile', title: `${n} ${s} dark mobile fit` }))
    }
  }
  keys.push(key({ family: 'aria', state: 'default', theme: 'dark', viewport: 'desktop', title: `${n} aria desktop` }))
  if (c.mobile === undefined) {
    keys.push(key({ family: 'aria', state: 'default', theme: 'dark', viewport: 'mobile', title: `${n} aria mobile` }))
  }
  for (const viewport of ['desktop', 'mobile'] as const) {
    for (const theme of THEMES) {
      keys.push(key({ family: 'axe', state: 'default', theme, viewport, title: `${n} default ${theme} ${viewport} axe` }))
    }
  }
  keys.push(key({ family: 'focus', state: 'default', theme: 'dark', viewport: 'desktop', title: `${n} focus walk` }))
  const lower = denyRoleFor(c.role)
  if (lower !== null) keys.push(key({ family: 'deny', viewport: 'desktop', title: `${n} deny (${lower})` }))
  if (c.tenantScoped === true) keys.push(key({ family: 'leak', viewport: 'desktop', title: `${n} tenant leak probe` }))
  if (isScaleState(c.scale)) {
    const s = c.scale.name
    keys.push(key({ family: 'scale', state: s, theme: 'dark', viewport: 'desktop', title: `${n} scale ${s}` }))
  }
  return keys.sort()
}

interface PwNode {
  title?: string
  tags?: string[]
  specs?: PwNode[]
  suites?: PwNode[]
}

/** title + tag of every test Playwright collects under `contract:<name>`, per contract. */
function collectContractTests(): Map<string, string[]> {
  const raw = execFileSync('bun', ['x', 'playwright', 'test', '--list', '--reporter=json', 'e2e/smoke.spec.ts'], {
    cwd: WEB,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    // CTX_E2E_CONTAINER/CTX_E2E_QUARANTINE: the config's grepInvert hides
    // nothing from the collection (no test runs, so no pixel is compared)
    env: { ...process.env, CTX_E2E_CONTAINER: '1', CTX_E2E_QUARANTINE: '1' },
  })
  const out = new Map<string, string[]>()
  const walk = (node: PwNode, contract: string | null): void => {
    const name = node.title?.startsWith('contract:') === true ? node.title.slice('contract:'.length) : contract
    for (const spec of node.specs ?? []) {
      if (name === null) continue
      out.set(name, [...(out.get(name) ?? []), `${spec.title} @${(spec.tags ?? []).join(' @')}`])
    }
    for (const s of node.suites ?? []) walk(s, name)
  }
  for (const s of (JSON.parse(raw) as PwNode).suites ?? []) walk(s, null)
  return out
}

describe('contract test plan (#457)', () => {
  it('plans every family, state, theme and viewport of the §4.1 table, and nothing else', () => {
    for (const c of contracts) {
      const planned = planContract(c)
        .map((t) => key({ family: t.family, state: t.state?.name, theme: t.theme, viewport: t.viewport, title: t.title }))
        .sort()
      expect(planned, `test plan of contract '${c.name}'`).toEqual(expectedKeys(c))
    }
  })

  it('plans unique titles (they key the test ids, the CI history and the baselines)', () => {
    const titles = contracts.flatMap((c) => planContract(c).map((t) => t.title))
    expect(titles.filter((t, i) => titles.indexOf(t) !== i)).toEqual([])
  })

  it("registers exactly the planned tests, by Playwright's own collection", () => {
    const collected = collectContractTests()
    expect([...collected.keys()].sort()).toEqual(contracts.map((c) => c.name).sort())
    for (const c of contracts) {
      const planned = planContract(c)
        .map((t) => `${t.title} ${t.tag}`)
        .sort()
      expect((collected.get(c.name) ?? []).sort(), `registered tests of contract '${c.name}'`).toEqual(planned)
    }
  }, 60_000)
})
