import { defineConfig, devices } from '@playwright/test'

// Live-tier Playwright config (design 06 §4.7, wave PV10) — DELIBERATELY
// SEPARATE from the mock tier (../../playwright.config.ts). Key differences:
//
//   • NO webServer: the SUT is the throwaway ctxd from docker-compose.e2e.yml
//     (started by run-live.sh), which serves BOTH the embedded SPA and the API
//     on one origin. baseURL comes from CTX_E2E_BASE_URL — never the fixed mock
//     port 5179 (that port belongs to the mock tier / a parallel FE agent).
//   • globalSetup = seed.ts: the fail-closed target gate + production-path
//     seeds run ONCE before any spec; a gate refusal aborts the whole run
//     before a single write (§3.6).
//   • NO @visual: the live tier proves CLASSES of statements (enforcement,
//     shape-truth, real streams), not pixels — zero screenshot baselines here
//     (§4.7). Every spec is tagged @live; the project grep pins that.
//   • traces retain-on-failure but the CI job caps live artifacts at
//     retention-days 3 (real keys flow only here — §4.8); the DB dies with the
//     stack, so a leaked key has impact null (§3.6 invariant 3).

const baseURL = process.env.CTX_E2E_BASE_URL ?? 'http://localhost:18099'

export default defineConfig({
  testDir: '.',
  testMatch: '**/*.spec.ts',
  outputDir: './.results/pw',
  globalSetup: './seed.ts',
  fullyParallel: false, // one throwaway instance, shared corpus — serialise
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: [
    ['list'],
    ['json', { outputFile: './.results/live-report.json' }],
  ],
  timeout: 30_000,
  expect: { timeout: 10_000 },
  projects: [
    {
      name: 'live',
      grep: /@live/,
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
  ],
  use: {
    baseURL,
    trace: 'retain-on-failure',
    screenshot: 'off',
    launchOptions: { args: ['--enable-unsafe-swiftshader'] },
  },
})
