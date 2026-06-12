// FA2 layout supervisor (design 05-§4-W3): graphology's web-worker layout
// (Blob-URL worker — the G05 CSP carries worker-src blob: for exactly this)
// runs 3–10s after every merge, scaled by graph size, then stops. A re-merge
// during a run extends it instead of stacking workers.

import type { DirectedGraph } from 'graphology'
import forceAtlas2 from 'graphology-layout-forceatlas2'
import FA2Layout from 'graphology-layout-forceatlas2/worker'
import type { EdgeAttrs, NodeAttrs } from './graph-client'

export class LayoutRunner {
  #layout: FA2Layout | null = null
  #stopTimer: ReturnType<typeof setTimeout> | null = null

  /** (Re)start the layout for a size-scaled duration. */
  run(graph: DirectedGraph<NodeAttrs, EdgeAttrs>): void {
    if (graph.order < 2) return
    if (this.#layout === null) {
      this.#layout = new FA2Layout(graph, {
        settings: { ...forceAtlas2.inferSettings(graph), slowDown: 10 },
      })
    }
    if (!this.#layout.isRunning()) this.#layout.start()
    if (this.#stopTimer !== null) clearTimeout(this.#stopTimer)
    const seconds = Math.min(10, 3 + graph.order / 500)
    this.#stopTimer = setTimeout(() => this.#layout?.stop(), seconds * 1000)
  }

  /** Kill the worker (component unmount). */
  dispose(): void {
    if (this.#stopTimer !== null) clearTimeout(this.#stopTimer)
    this.#stopTimer = null
    this.#layout?.kill()
    this.#layout = null
  }
}
