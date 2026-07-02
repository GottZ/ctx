import { writeFileSync } from 'node:fs'
import type { PluginOption } from 'vite'
// vitest/config re-exports Vite's defineConfig with the `test` field typed —
// one config file drives dev, build and the unit tests.
import { defineConfig } from 'vitest/config'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { compression } from 'vite-plugin-compression2'

// Dev proxy target: the ctxd instance the dev server forwards API calls to.
// The compose ctx service publishes no ports by default — map one in your
// local docker-compose.override.yml (see docker-compose.override.yml.example
// at the repo root) and set CTX_DEV_PROXY when it differs from the default.
const proxyTarget = process.env.CTX_DEV_PROXY ?? 'http://localhost:8080'

// Vite's emptyOutDir wipes dist/ including the committed .gitkeep that keeps
// the //go:embed all:dist pattern satisfiable on fresh clones. Restore it
// after every build so local builds never leave a "deleted: .gitkeep" behind.
function keepGitkeep(): PluginOption {
  return {
    name: 'keep-gitkeep',
    closeBundle() {
      writeFileSync('dist/.gitkeep', '')
    },
  }
}

export default defineConfig({
  plugins: [
    svelte(),
    // Pre-compress at build time; the Go handler (go/web/web.go) negotiates
    // the .br/.gz siblings — ctxd runs no on-the-fly compression middleware.
    compression({ algorithms: ['gzip', 'brotliCompress'], threshold: 1024 }),
    keepGitkeep(),
  ],
  build: {
    sourcemap: false,
  },
  server: {
    // Same-origin dev loop: the proxy is server-side, the browser sees one
    // origin — ctxd needs zero CORS code.
    proxy: {
      '/api': { target: proxyTarget, changeOrigin: false },
      '/health': { target: proxyTarget, changeOrigin: false },
      '/mcp': { target: proxyTarget, changeOrigin: false },
    },
  },
  test: {
    // Logic modules only (design 04-§5.5) — no component snapshots, no DOM:
    // fetch/sessionStorage are stubbed per test, runes compile fine in node.
    // tokens/ carries the build-time token gates (drift test, design 05-§4.1).
    // e2e/contract/ carries the COVERAGE.md drift gate (design 06 §3.4, PV4) —
    // registry/coverage are pure data modules, no Playwright runtime is invoked.
    environment: 'node',
    include: ['src/**/*.test.ts', 'tokens/**/*.test.ts', 'e2e/contract/**/*.test.ts'],
  },
})
