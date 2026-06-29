import { defineConfig, devices } from '@playwright/test'

// Visual smoke for the web-redesign shell/layout/theme/graph + role-gated areas
// (HANDOVER §8 debt). Builds the SPA and serves it with `vite preview` (the
// production build the Go binary embeds — static chunks, no dev-optimizer race),
// driven by browser-level `/api/**` mocks (e2e/fixtures.ts): no live ctxd, no
// real key, role-/theme-parametrised, fully deterministic.
//
// Browser: chromium-headless-shell (installed via `npx playwright install
// chromium`). NOT wired into the Go CI yet — `npm run test:e2e` is the local
// gate that closes the "rendering never seen in a browser" debt.

const PORT = 5179
const baseURL = `http://localhost:${PORT}`

export default defineConfig({
  testDir: './e2e',
  outputDir: './e2e/.results',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  // The preview build is deterministic — no retry needed locally; CI keeps one
  // as a guard against transient infra hiccups.
  retries: process.env.CI ? 1 : 0,
  reporter: [['list']],
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    viewport: { width: 1440, height: 900 },
    trace: 'retain-on-failure',
    screenshot: 'off',
    // Sigma renders WebGL; the headless shell needs a software GL backend.
    launchOptions: { args: ['--enable-unsafe-swiftshader'] },
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    // Build + preview (NOT the dev server): the production build has static
    // hashed chunks, so an immediate boot-time beforeLoad redirect (N4/N5) can
    // load its target chunk without the dev optimizer's reload race. VITE_E2E
    // keeps the __ctxGraph test hook in this build (it is import.meta.env.DEV-
    // gated otherwise) for the graph-palette assertions.
    command: `VITE_E2E=1 npm run build && npm run preview -- --port ${PORT} --strictPort`,
    url: baseURL,
    // Never reuse: a stale preview server would serve an old build (no HMR), so
    // every run rebuilds to guarantee the current code is under test.
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: 'ignore',
    stderr: 'pipe',
  },
})
