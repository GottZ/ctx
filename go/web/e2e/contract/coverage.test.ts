// vitest drift gate for the generated e2e/COVERAGE.md (design 06 §3.4, PV4
// gate c): the committed file must equal the regenerate byte for byte — a hand
// edit turns this red on the FAST path (bun run test), the Playwright matrix
// meta-test (matrix.spec.ts) pins the same equality on the e2e path.

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { renderCoverage } from './coverage'
import { contracts, pendingContracts } from './registry'
import { validateContract } from './contract'

describe('e2e/COVERAGE.md drift gate (generated artifact)', () => {
  it('matches the registry regenerate byte for byte (bun run e2e:matrix)', () => {
    const committed = readFileSync(new URL('../COVERAGE.md', import.meta.url), 'utf8')
    expect(committed).toBe(renderCoverage())
  })

  it('registry entries stay structurally valid on the fast path too', () => {
    for (const c of contracts) {
      expect(validateContract(c), `contract '${c.name}'`).toEqual([])
    }
    for (const p of pendingContracts) {
      expect(p.reason.trim(), `pending '${p.route}'`).not.toBe('')
    }
  })
})
