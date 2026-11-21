// Argument pin of e2e-visual.sh (#456), without a container: the script runs
// from a temporary copy whose e2e-container.sh is a stub that prints the argv
// it receives (the script calls its runner by a path relative to itself). The
// last case hands the built arguments to the real Playwright CLI (--list:
// collection only, no browser, no webServer), so a CLI whose option grammar
// rejects them turns this red — the failure #456 reported under 1.64.

import { execFileSync, spawnSync } from 'node:child_process'
import { copyFileSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterAll, describe, expect, it } from 'vitest'

const WEB = new URL('../..', import.meta.url).pathname
const dir = mkdtempSync(join(tmpdir(), 'e2e-visual-'))
copyFileSync(join(WEB, 'e2e-visual.sh'), join(dir, 'e2e-visual.sh'))
writeFileSync(join(dir, 'e2e-container.sh'), "printf '%s\\0' \"$@\"\n")
afterAll(() => rmSync(dir, { recursive: true, force: true }))

/** The playwright arguments e2e-visual.sh hands to `bun run test:e2e`. */
function playwrightArgs(...args: string[]): string[] {
  const out = execFileSync('bash', [join(dir, 'e2e-visual.sh'), ...args], { encoding: 'utf8' })
  const argv = out.split('\0').slice(0, -1)
  // bash -c '<install && test:e2e "$@">' bash <playwright args…>
  expect(argv.slice(0, 2)).toEqual(['bash', '-c'])
  expect(argv[2]).toBe('bun install --frozen-lockfile && bun run test:e2e "$@"')
  expect(argv[3]).toBe('bash')
  return argv.slice(4)
}

describe('e2e-visual.sh arguments (#456)', () => {
  it('passes the update mode as one argument, so a spec file after it stays a file', () => {
    expect(playwrightArgs()).toEqual([])
    expect(playwrightArgs('--update')).toEqual(['--update-snapshots=changed'])
    expect(playwrightArgs('--update', '--', 'e2e/smoke.spec.ts')).toEqual(['--update-snapshots=changed', 'e2e/smoke.spec.ts'])
    expect(playwrightArgs('--update', '--', '-g', 'blocks')).toEqual(['--update-snapshots=changed', '-g', 'blocks'])
    expect(playwrightArgs('--', '-g', 'blocks')).toEqual(['-g', 'blocks'])
  })

  it('rejects an unknown argument', () => {
    const r = spawnSync('bash', [join(dir, 'e2e-visual.sh'), '--updat'], { encoding: 'utf8' })
    expect(r.status).toBe(1)
    expect(r.stderr).toContain("unknown arg '--updat'")
  })

  it('builds arguments the pinned Playwright CLI accepts with a spec file', () => {
    // --reporter=list: the config's blob and json reporters would overwrite
    // e2e/.results/ (report.json, blob) of the last e2e run
    const args = playwrightArgs('--update', '--', 'e2e/smoke.spec.ts', '-g', 'contract:login', '--list', '--reporter=list')
    const r = spawnSync('bun', ['x', 'playwright', 'test', ...args], { cwd: WEB, encoding: 'utf8' })
    expect(r.stderr, 'the Playwright CLI rejected the arguments').not.toMatch(/is invalid/)
    expect(r.status, r.stderr).toBe(0)
    expect(r.stdout).toMatch(/Total: [1-9]\d* tests? in 1 file/)
  }, 60_000)
})
