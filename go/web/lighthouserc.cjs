// LHCI config — nightly timing TREND source (overnight plan C, wave C-W3).
//
//   bun run perf:lhci        (run `bun run build` first — release build,
//                             NO VITE_E2E; in CI inside the pinned
//                             toolchain container, nightly-only job)
//
// Role boundary (DECISIONS C-E1/C-E2, keeps decision 06-E6): LHCI NEVER
// judges bytes — the on-disk .br budget gate (e2e/perf/chunk-budget.ts, PLc)
// is the PR byte authority. LHCI is a lab TIMING probe: nightly-only,
// warn-only, never a PR gate. Lab timings on shared CI runners carry a
// ±20-40% noise floor — only jumps beyond ~40% are signal (review C1-M2);
// any future timing BUDGET must be calibrated from the MEDIAN of several
// nightly runs, never a single run (review C1-M3).
//
// calibrated: FALSE — the warn thresholds below are deliberately uncalibrated
// placeholders that only flag egregious regressions; they carry no gate
// authority (every assertion level is "warn", the job stays green).
//
// Static serve, not `vite preview` (masterplan C-W3): preview inherits
// server.proxy from vite.config.ts (resolveConfig) and would point /api at a
// dead backend inside the CI container. staticDistDir makes LHCI serve dist/
// itself — no proxy, no webServer, the /api calls of the shell die at the
// static server exactly like in the e2e mock tier. We measure the shell, not
// live data.
//
// Chrome comes from the pinned Playwright toolchain image (D4 resolved:
// /ms-playwright/chromium-<rev>/chrome-linux64/chrome, node is present in the
// image). The revision directory changes with a toolchain digest bump, so the
// path is globbed at runtime; CHROME_PATH overrides for local/other setups.
// No chrome-launcher dependency — reviews C2-10/C3-m3: redundant, the image
// ships the browser.

'use strict'

const { globSync } = require('node:fs')

function chromePath() {
  if (process.env.CHROME_PATH) return process.env.CHROME_PATH
  const hits = globSync('/ms-playwright/chromium-*/chrome-linux64/chrome')
  if (hits.length > 0) return hits.sort().at(-1)
  // Outside the toolchain image: let chrome-launcher-less LHCI fail loudly
  // rather than silently probing PATH — the verdict is only comparable
  // in-container anyway (same reasoning as the byte budget, C1-M1).
  throw new Error(
    'lighthouserc: no Chromium found — run inside the pinned toolchain container or set CHROME_PATH',
  )
}

module.exports = {
  ci: {
    collect: {
      staticDistDir: './dist',
      url: ['http://localhost/index.html'],
      // 3 runs per night: LHCI takes the median run, damping single-run
      // outliers before the trend line ever sees them (C1-M3 in-run layer).
      numberOfRuns: 3,
      // collect.chromePath is what the LHCI healthcheck and chrome-launcher
      // read (settings.chromePath is NOT consulted by the healthcheck —
      // verified against @lhci/cli 0.15.1: "Chrome installation not found").
      chromePath: chromePath(),
      settings: {
        // Container runs as root — Chromium needs --no-sandbox there (same
        // constraint as the Playwright e2e tier in the identical image).
        chromeFlags: '--no-sandbox --headless=new',
      },
    },
    assert: {
      // warn-only BY DESIGN — see header. No resource-size assertions here:
      // bytes are PLc's jurisdiction (chunk-budget.ts).
      assertions: {
        'categories:performance': ['warn', { minScore: 0.6 }],
        'first-contentful-paint': ['warn', { maxNumericValue: 4000 }],
        'largest-contentful-paint': ['warn', { maxNumericValue: 6000 }],
        'total-blocking-time': ['warn', { maxNumericValue: 1000 }],
        'cumulative-layout-shift': ['warn', { maxNumericValue: 0.2 }],
      },
    },
    upload: {
      target: 'filesystem',
      outputDir: './e2e/perf/.lhci',
    },
  },
}
