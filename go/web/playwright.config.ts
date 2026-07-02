import { defineConfig, devices } from '@playwright/test'

// Visual smoke for the web-redesign shell/layout/theme/graph + role-gated areas
// (HANDOVER §8 debt) — now also the config FOUNDATION for the page-validation
// framework (design 06, wave PV1). Builds the SPA and serves it with
// `bun run preview` (the production build the Go binary embeds — static chunks,
// no dev-optimizer race), driven by browser-level `/api/**` mocks
// (e2e/fixtures.ts): no live ctxd, no real key, role-/theme-parametrised,
// fully deterministic.
//
// Browser: chromium-headless-shell (installed via `bunx playwright install
// chromium`). `bun run test:e2e` is the local gate; the CI `web` job lands in
// wave PV8.

const PORT = 5179
const baseURL = `http://localhost:${PORT}`

export default defineConfig({
  testDir: './e2e',
  outputDir: './e2e/.results',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  // The preview build is deterministic — no retry needed locally; CI keeps one
  // as a guard against transient infra hiccups (declared, not a flake blanket:
  // every `flaky` status is surfaced from report.json — design 06 §5.4).
  retries: process.env.CI ? 1 : 0,
  // Explicit worker base (design 06 §6.1): the Playwright default (50 % of
  // cores) would mean only 2 workers on a 4-vCPU CI runner; the inventory
  // measurement (~0.4 cores/worker) shows 4 workers are sustainable there.
  // Locally the default stays (undefined = 50 % of cores).
  workers: process.env.CI ? 4 : undefined,
  // list = human console; blob = sharding/merge-reports foundation (§6.3);
  // json = machine-readable durations + flaky status (§4.8). Blob goes under
  // e2e/.results/ so the existing e2e/.gitignore covers all reporter output.
  reporter: [
    ['list'],
    ['blob', { outputDir: 'e2e/.results/blob' }],
    ['json', { outputFile: 'e2e/.results/report.json' }],
  ],
  timeout: 30_000,
  expect: {
    timeout: 10_000,
    // Visual policy (design 06 §4.3): zero tolerance by default. Escalation
    // ladder on REAL need is (1) mask, (2) stylePath, (3) per-page
    // maxDiffPixels with comment + issue — NEVER a global ratio loosening.
    toHaveScreenshot: { maxDiffPixels: 0, animations: 'disabled', caret: 'hide' },
  },
  // One canonical render environment (the digest-pinned toolchain container,
  // arrives in wave PV3) — deliberately NO {platform} token: a second
  // platform-suffixed baseline set would be the backdoor to unpinned local
  // renders (design 06 §3.1). {arg} carries <page>--<state>--<theme>--<viewport>.
  snapshotPathTemplate: '{testDir}/__screenshots__/{testFileBaseName}/{arg}{ext}',
  // @visual gating (design 06 §4.4): pixel asserts only run inside the pinned
  // container (CTX_E2E_CONTAINER=1, self-declared + CI-defended); flow/ARIA/axe
  // tests are platform-stable and always run.
  grepInvert: process.env.CTX_E2E_CONTAINER ? undefined : /@visual/,
  use: {
    baseURL,
    trace: 'retain-on-failure',
    screenshot: 'off',
    // Sigma renders WebGL; the headless shell needs a software GL backend.
    launchOptions: { args: ['--enable-unsafe-swiftshader'] },
  },
  projects: [
    {
      name: 'chromium',
      // Explicit viewport AFTER the device spread (design 06 S1): the
      // devices['Desktop Chrome'] preset carries 1280×720 and used to silently
      // override the declared 1440×900. The decision is now explicit, never
      // inherited — baselines must not inherit that silent discrepancy.
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
  ],
  webServer: {
    // Build + preview (NOT the dev server): the production build has static
    // hashed chunks, so an immediate boot-time beforeLoad redirect (N4/N5) can
    // load its target chunk without the dev optimizer's reload race. VITE_E2E
    // keeps the __ctxGraph test hook in this build (it is import.meta.env.DEV-
    // gated otherwise) for the graph-palette assertions. bun is the single
    // install/run path — identical to the Docker release build (go/Dockerfile),
    // see design 06 S4: bun.lock is the only dependency truth.
    command: `VITE_E2E=1 bun run build && bun run preview -- --port ${PORT} --strictPort`,
    url: baseURL,
    // Never reuse: a stale preview server would serve an old build (no HMR), so
    // every run rebuilds to guarantee the current code is under test.
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: 'ignore',
    stderr: 'pipe',
  },
})
