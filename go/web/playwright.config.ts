import { defineConfig, devices } from '@playwright/test'

// Visual smoke for the web-redesign shell/layout/theme/graph (HANDOVER §8 debt).
// Runs the Vite dev server and drives it with browser-level `/api/**` mocks
// (e2e/fixtures.ts) — no live ctxd, no real key, fully deterministic.
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
  // The Vite dev server's dep-optimizer reload can drop one in-flight dynamic
  // route import on the first cold load ("Failed to fetch dynamically imported
  // module"); a single retry runs against the now-warm server. Serialised
  // locally so cold navigations don't thrash the optimizer in parallel.
  retries: 1,
  workers: process.env.CI ? 2 : 1,
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
    command: `npm run dev -- --port ${PORT} --strictPort`,
    url: baseURL,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    stdout: 'ignore',
    stderr: 'pipe',
  },
})
