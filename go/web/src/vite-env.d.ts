/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Set by the e2e build (`VITE_E2E=1 vite build`) so the `__ctxGraph` test hook
   * ships in the preview build the Playwright smoke runs against — without ever
   * exposing it in the real production build (where this is unset). Read alongside
   * `import.meta.env.DEV` at the hook sites (GraphView/OverviewMap).
   */
  readonly VITE_E2E?: string
}
