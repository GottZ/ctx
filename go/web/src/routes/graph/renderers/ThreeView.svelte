<script lang="ts">
  import { onMount } from 'svelte'
  import {
    BufferGeometry,
    Color,
    Float32BufferAttribute,
    InstancedMesh,
    LineBasicMaterial,
    LineSegments,
    Matrix4,
    MeshBasicMaterial,
    PerspectiveCamera,
    Raycaster,
    Scene,
    SphereGeometry,
    SRGBColorSpace,
    Vector2,
    Vector3,
    WebGLRenderer,
  } from 'three'
  // three@0.185.1 exportiert `./addons/*` → examples/jsm (package.json exports,
  // @types/three spiegelt das Mapping) — löst mit moduleResolution "bundler"
  // UND Vite ohne Alias-Konfiguration auf.
  import { OrbitControls } from 'three/addons/controls/OrbitControls.js'
  import type { MultiDirectedGraph } from 'graphology'
  import type { GraphFilters } from '../../../lib/graph/filters'
  import type { EdgeAttrs, NodeAttrs } from '../../../lib/graph/graph-client'
  import type { GraphPalette } from '../../../lib/graph/graph-theme'
  import {
    buildEdgeBuffers,
    buildNodeBuffers,
    refreshPositions,
    type EdgeBuffers,
    type NodeBuffers,
  } from '../../../lib/graph/render-buffers'

  // RV1 three-Adapter: 2.5D-Renderer über der geteilten graphology-Instanz.
  // Das 2D-FA2-Layout der Seite bleibt die XY-Ebene, die Z-Achse wird
  // SEMANTISCH belegt (hop-Distanz oder createdAt) — Empirie: 2.5D schlägt
  // volles 3D-Force bei Lesbarkeit (Recherche 2026-08-09, ctx 019fe63f).
  let {
    graph,
    filters,
    palette,
    graphRev = 0,
    highlightedIds,
    topId = null,
    onnodeclick,
    onnodedoubleclick,
  }: {
    graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>
    /** Filter wirken beim Buffer-Bau (visible-Maske) — kein Server-Roundtrip. */
    filters: GraphFilters
    /** Nur Rebuild-Trigger: die Farben sind von GraphPage bereits in die
     *  graphology-Attrs gebacken (recolorGraph), die Buffer lesen sie fertig. */
    palette: GraphPalette
    /** Merge-Revision aus GraphPage — jeder Tick baut die TypedArrays neu. */
    graphRev?: number
    /** Offene Fenster: Instanz-Scale ×1.3; das Top-Fenster ×1.5. */
    highlightedIds?: Set<string>
    topId?: string | null
    onnodeclick?: (id: string) => void
    onnodedoubleclick?: (id: string) => void
  } = $props()

  type ZMode = 'hop' | 'time' | 'flat'
  /** Ebenen-Abstand der hop-Z-Achse; auf die FA2-Ring-Radien (~80/hop,
   *  graph-client jitter) abgestimmt, damit XY- und Z-Skala verwandt bleiben. */
  const HOP_LAYER = 90

  // Z-Semantik ist die EINZIGE reaktive Render-Steuerung dieses Adapters —
  // Wechsel = Positions-Nachzug (Instanz-Matrizen + Line-Buffer), KEIN Remount.
  let zMode = $state<ZMode>('hop')
  // Tooltip: nur Geometrie als Inline-Style (style:left/top), Farben im CSS.
  let tip = $state<{ x: number; y: number; label: string } | null>(null)
  let hovering = $state(false)

  let host: HTMLDivElement
  // three-Objekte leben außerhalb der Svelte-Reaktivität (GraphView-Muster):
  // der runes-Proxy auf Scene-Graphen wäre reiner Overhead ohne Nutzen.
  let scene: Scene | null = null
  let camera: PerspectiveCamera | null = null
  let renderer3: WebGLRenderer | null = null
  let controls: OrbitControls | null = null
  let nodeMesh: InstancedMesh | null = null
  let edgeLines: LineSegments | null = null
  let sphereGeo: SphereGeometry | null = null
  let nodeMat: MeshBasicMaterial | null = null
  let edgeMat: LineBasicMaterial | null = null

  let nodes: NodeBuffers | null = null
  let edges: EdgeBuffers | null = null
  /** instanceId → Buffer-Index (nur Filter-SICHTBARE Knoten werden instanziert;
   *  Raycast-instanceId läuft hierüber zurück zur node-id). */
  let visIdx: number[] = []
  /** Semantische Z-Koordinate je Buffer-Index (aus zMode abgeleitet). */
  let zArr = new Float32Array(0)
  /** Bounding-Sphere des InstancedMesh ist nach Positions-Updates stale —
   *  lazy vor dem nächsten Raycast/Kamera-Fit nachziehen statt pro Tick. */
  let boundsDirty = false

  // Render-Loop nur bei Bedarf: controls-change, Layout-Ticks und Rebuilds
  // setzen einen Frame an; ohne Ereignis läuft KEIN rAF (kein Dauer-Loop).
  // Das Damping trägt sich selbst weiter: controls.update() feuert 'change',
  // solange die Trägheit noch Bewegung liefert → der Loop klingt mit ihr aus.
  let renderRaf: number | null = null
  function requestRender(): void {
    if (renderRaf !== null) return
    renderRaf = requestAnimationFrame(() => {
      renderRaf = null
      if (renderer3 === null || scene === null || camera === null) return
      controls?.update()
      renderer3.render(scene, camera)
    })
  }

  // ---- Buffer → GPU ---------------------------------------------------------

  const scratchMtx = new Matrix4()
  const scratchColor = new Color()

  function instanceScale(idx: number): number {
    const nb = nodes as NodeBuffers
    const id = nb.ids[idx]
    if (id === topId) return nb.sizes[idx] * 1.5
    if (highlightedIds?.has(id) === true) return nb.sizes[idx] * 1.3
    return nb.sizes[idx]
  }

  /** Semantische Z je zMode. hop/time sind pro Rebuild eingefroren (hops und
   *  createdAt ändern sich nur mit graphRev); `time` friert auch den spread
   *  ein, damit Layout-Ticks die Z-Ebenen nicht mitatmen lassen. */
  function computeZ(): void {
    const nb = nodes as NodeBuffers
    zArr = new Float32Array(nb.ids.length)
    if (zMode === 'flat' || visIdx.length === 0) return
    if (zMode === 'hop') {
      let maxHop = 0
      for (const i of visIdx) if (nb.hops[i] > maxHop) maxHop = nb.hops[i]
      // -1 = unerreichbar → eine Ebene UNTER dem tiefsten Hop (nicht mischen).
      for (const i of visIdx) {
        zArr[i] = nb.hops[i] < 0 ? -(maxHop + 1) * HOP_LAYER : -nb.hops[i] * HOP_LAYER
      }
      return
    }
    // time: createdAt linear auf [-spread/2, +spread/2]; spread ≈ halbe
    // XY-Ausdehnung, damit die Zeitachse die Raumproportion nicht sprengt.
    let minX = Infinity
    let maxX = -Infinity
    let minY = Infinity
    let maxY = -Infinity
    let minT = Infinity
    let maxT = -Infinity
    for (const i of visIdx) {
      const x = nb.positions[i * 2]
      const y = nb.positions[i * 2 + 1]
      if (x < minX) minX = x
      if (x > maxX) maxX = x
      if (y < minY) minY = y
      if (y > maxY) maxY = y
      const t = nb.createdAt[i]
      if (!Number.isNaN(t)) {
        if (t < minT) minT = t
        if (t > maxT) maxT = t
      }
    }
    const spread = Math.max(maxX - minX, maxY - minY) / 2 || 300 // degeneriert → fester Hub
    const range = maxT - minT
    for (const i of visIdx) {
      const t = nb.createdAt[i]
      // NaN (unparsebares Datum) → eigene unterste Ebene unter der Zeitspanne,
      // damit „undatiert" nicht als „ältester Block" durchgeht.
      if (Number.isNaN(t)) zArr[i] = -spread / 2 - HOP_LAYER
      else zArr[i] = range > 0 && Number.isFinite(range) ? ((t - minT) / range - 0.5) * spread : 0
    }
  }

  /** Positions-Nachzug: Instanz-Matrizen (Position+Scale) und Line-Vertices
   *  aus den aktuellen Buffern — für Layout-Ticks UND zMode-Wechsel. */
  function applyPositions(): void {
    const nb = nodes
    const eb = edges
    if (nb === null || eb === null || nodeMesh === null || edgeLines === null) return
    for (let k = 0; k < visIdx.length; k++) {
      const i = visIdx[k]
      const s = instanceScale(i)
      scratchMtx.makeScale(s, s, s).setPosition(nb.positions[i * 2], nb.positions[i * 2 + 1], zArr[i])
      nodeMesh.setMatrixAt(k, scratchMtx)
    }
    nodeMesh.instanceMatrix.needsUpdate = true
    boundsDirty = true
    const pos = edgeLines.geometry.getAttribute('position')
    const arr = pos.array as Float32Array
    let v = 0
    for (let e = 0; e < eb.visible.length; e++) {
      if (eb.visible[e] !== 1) continue
      const s = eb.pairs[e * 2]
      const t = eb.pairs[e * 2 + 1]
      arr[v++] = nb.positions[s * 2]
      arr[v++] = nb.positions[s * 2 + 1]
      arr[v++] = zArr[s]
      arr[v++] = nb.positions[t * 2]
      arr[v++] = nb.positions[t * 2 + 1]
      arr[v++] = zArr[t]
    }
    pos.needsUpdate = true
  }

  /** Voller Neubau der GPU-Objekte aus der graphology-Instanz (graphRev-Tick,
   *  Filter-/Palette-/Highlight-Wechsel). Instanz-Count und sichtbare
   *  Kantenmenge ändern sich hier — darum neue Objekte statt In-place-Update. */
  function rebuild(): void {
    const sc = scene
    if (sc === null || sphereGeo === null || nodeMat === null || edgeMat === null) return
    nodes = buildNodeBuffers(graph, filters)
    edges = buildEdgeBuffers(graph, filters, nodes)
    visIdx = []
    for (let i = 0; i < nodes.visible.length; i++) if (nodes.visible[i] === 1) visIdx.push(i)
    computeZ()

    if (nodeMesh !== null) {
      sc.remove(nodeMesh)
      nodeMesh.dispose() // gibt instanceMatrix/instanceColor frei; Geometrie/Material sind geteilt
    }
    nodeMesh = new InstancedMesh(sphereGeo, nodeMat, visIdx.length)
    // Culling aus: die Bounding-Sphere wird nur lazy (Raycast/Fit) nachgezogen —
    // ein stale-gecullter Graph wäre ein unsichtbarer-Knoten-Bug.
    nodeMesh.frustumCulled = false
    for (let k = 0; k < visIdx.length; k++) {
      const i = visIdx[k]
      // Buffer-rgba ist sRGB (aus dem gebackenen Hex) — als SRGBColorSpace
      // einlesen, damit three korrekt nach working-linear wandelt und der
      // Output wieder exakt die Sigma-Farbe trifft.
      scratchColor.setRGB(nodes.colors[i * 4], nodes.colors[i * 4 + 1], nodes.colors[i * 4 + 2], SRGBColorSpace)
      nodeMesh.setColorAt(k, scratchColor)
    }
    if (nodeMesh.instanceColor !== null) nodeMesh.instanceColor.needsUpdate = true
    sc.add(nodeMesh)

    if (edgeLines !== null) {
      sc.remove(edgeLines)
      edgeLines.geometry.dispose()
    }
    let visEdges = 0
    for (let e = 0; e < edges.visible.length; e++) if (edges.visible[e] === 1) visEdges++
    const geo = new BufferGeometry()
    geo.setAttribute('position', new Float32BufferAttribute(new Float32Array(visEdges * 6), 3))
    const cols = new Float32Array(visEdges * 6)
    let c = 0
    for (let e = 0; e < edges.visible.length; e++) {
      if (edges.visible[e] !== 1) continue
      scratchColor.setRGB(edges.colors[e * 4], edges.colors[e * 4 + 1], edges.colors[e * 4 + 2], SRGBColorSpace)
      // beide Vertices der Kante tragen dieselbe Farbe (Alpha global am Material)
      cols[c++] = scratchColor.r
      cols[c++] = scratchColor.g
      cols[c++] = scratchColor.b
      cols[c++] = scratchColor.r
      cols[c++] = scratchColor.g
      cols[c++] = scratchColor.b
    }
    geo.setAttribute('color', new Float32BufferAttribute(cols, 3))
    edgeLines = new LineSegments(geo, edgeMat)
    edgeLines.frustumCulled = false
    sc.add(edgeLines)

    applyPositions()
  }

  // ---- Layout-Ticks (FA2-Worker mutiert graphology-x/y kontinuierlich) -----

  let syncRaf: number | null = null
  function scheduleSync(): void {
    if (syncRaf !== null) return
    syncRaf = requestAnimationFrame(() => {
      syncRaf = null
      if (nodes === null) return
      refreshPositions(graph, nodes)
      applyPositions()
      requestRender()
    })
  }

  // ---- Picking (Raycast über instanceId) -----------------------------------

  const raycaster = new Raycaster()
  const pointer = new Vector2()

  function pick(clientX: number, clientY: number): { id: string; idx: number } | null {
    const r = renderer3
    const cam = camera
    const mesh = nodeMesh
    const nb = nodes
    if (r === null || cam === null || mesh === null || nb === null) return null
    const rect = r.domElement.getBoundingClientRect()
    if (rect.width === 0 || rect.height === 0) return null
    pointer.set(((clientX - rect.left) / rect.width) * 2 - 1, -((clientY - rect.top) / rect.height) * 2 + 1)
    if (boundsDirty) {
      mesh.computeBoundingSphere() // Raycaster prüft die Sphere zuerst — stale = verlorene Klicks
      boundsDirty = false
    }
    raycaster.setFromCamera(pointer, cam)
    for (const hit of raycaster.intersectObject(mesh, false)) {
      if (hit.instanceId !== undefined) {
        const idx = visIdx[hit.instanceId]
        return { id: nb.ids[idx], idx }
      }
    }
    return null
  }

  // Klick vs. Orbit-Drag: OrbitControls rotiert auf demselben Button — ein
  // Klick zählt nur, wenn der Pointer seit pointerdown quasi stillstand.
  let downX = 0
  let downY = 0
  function onPointerDown(e: PointerEvent): void {
    downX = e.clientX
    downY = e.clientY
  }
  function onClick(e: MouseEvent): void {
    if (Math.hypot(e.clientX - downX, e.clientY - downY) > 5) return
    const hit = pick(e.clientX, e.clientY)
    if (hit !== null) onnodeclick?.(hit.id)
  }
  function onDblClick(e: MouseEvent): void {
    const hit = pick(e.clientX, e.clientY)
    if (hit !== null) onnodedoubleclick?.(hit.id)
  }

  // Hover rAF-gedrosselt: pro Frame höchstens ein Raycast, nicht pro Move-Event.
  let hoverRaf: number | null = null
  let hoverEvt: { cx: number; cy: number; ox: number; oy: number } | null = null
  function onPointerMove(e: PointerEvent): void {
    hoverEvt = { cx: e.clientX, cy: e.clientY, ox: e.offsetX, oy: e.offsetY }
    if (hoverRaf !== null) return
    hoverRaf = requestAnimationFrame(() => {
      hoverRaf = null
      const ev = hoverEvt
      if (ev === null) return
      const hit = pick(ev.cx, ev.cy)
      const nb = nodes
      if (hit !== null && nb !== null) {
        tip = { x: ev.ox + 14, y: ev.oy + 14, label: nb.labels[hit.idx] }
        hovering = true
      } else {
        tip = null
        hovering = false
      }
    })
  }
  function onPointerLeave(): void {
    hoverEvt = null
    tip = null
    hovering = false
  }

  // ---- Mount / Teardown ----------------------------------------------------

  onMount(() => {
    // alpha:true — der Canvas bleibt durchsichtig, der Host liefert
    // var(--graph-bg): Theme-Wechsel färbt den Hintergrund gratis um.
    const r = new WebGLRenderer({ alpha: true, antialias: true })
    r.setPixelRatio(Math.min(window.devicePixelRatio, 2))
    host.appendChild(r.domElement)
    renderer3 = r
    scene = new Scene()
    camera = new PerspectiveCamera(50, 1, 0.1, 100_000)
    camera.position.set(0, 0, 1000)
    const ctl = new OrbitControls(camera, r.domElement)
    ctl.enableDamping = true
    ctl.addEventListener('change', requestRender)
    controls = ctl

    // geteilte GPU-Ressourcen — überleben Rebuilds, sterben erst im Teardown
    sphereGeo = new SphereGeometry(1, 12, 8)
    nodeMat = new MeshBasicMaterial() // weiße Basis; instanceColor multipliziert
    edgeMat = new LineBasicMaterial({ vertexColors: true, transparent: true, opacity: 0.5 })

    const canvas = r.domElement
    canvas.addEventListener('pointerdown', onPointerDown)
    canvas.addEventListener('click', onClick)
    canvas.addEventListener('dblclick', onDblClick)
    canvas.addEventListener('pointermove', onPointerMove)
    canvas.addEventListener('pointerleave', onPointerLeave)

    // Layout-Bewegung: der FA2-Worker schreibt x/y einzeln UND gebatcht —
    // beide Event-Namen abonnieren (graphology-types index.d.ts:280/282).
    graph.on('nodeAttributesUpdated', scheduleSync)
    graph.on('eachNodeAttributesUpdated', scheduleSync)

    const ro = new ResizeObserver(() => {
      const w = host.clientWidth
      const h = host.clientHeight
      if (w === 0 || h === 0 || camera === null) return
      r.setSize(w, h, false) // CSS macht der Style-Block — kein Inline-Style-Write
      camera.aspect = w / h
      camera.updateProjectionMatrix()
      requestRender()
    })
    ro.observe(host)

    rebuild()
    resetCamera()
    requestRender()

    return () => {
      if (renderRaf !== null) cancelAnimationFrame(renderRaf)
      if (syncRaf !== null) cancelAnimationFrame(syncRaf)
      if (hoverRaf !== null) cancelAnimationFrame(hoverRaf)
      graph.off('nodeAttributesUpdated', scheduleSync)
      graph.off('eachNodeAttributesUpdated', scheduleSync)
      ro.disconnect()
      ctl.dispose()
      nodeMesh?.dispose()
      edgeLines?.geometry.dispose()
      sphereGeo?.dispose()
      nodeMat?.dispose()
      edgeMat?.dispose()
      r.dispose()
      renderer3 = null
      scene = null
      camera = null
      controls = null
      nodeMesh = null
      edgeLines = null
      nodes = null
      edges = null
    }
  })

  // Rebuild-Trigger: Deps werden VOR dem Mount-Guard gelesen, sonst verlöre
  // der Early-Return das Dependency-Tracking. Der Mount-Lauf ist ein no-op
  // (renderer3 noch/nicht mehr null) — onMount macht den Erst-Bau selbst.
  $effect(() => {
    void graphRev
    void filters
    void palette
    void highlightedIds
    void topId
    if (renderer3 === null) return
    rebuild()
    requestRender()
  })

  // zMode-Wechsel = reiner Positions-Nachzug (Kern-Feature): Z neu ableiten,
  // Matrizen + Line-Buffer schreiben — kein Buffer-Neubau, kein Remount.
  $effect(() => {
    void zMode
    if (renderer3 === null || nodes === null) return
    computeZ()
    applyPositions()
    requestRender()
  })

  /** Kamera auf die Bounding-Sphäre der sichtbaren Daten fitten (Page ruft
   *  das nach Fokus-Sprüngen; onMount nutzt es für den Erst-Fit). */
  export function resetCamera(): void {
    const cam = camera
    const ctl = controls
    const nb = nodes
    if (cam === null || ctl === null || nb === null) return
    let minX = Infinity
    let maxX = -Infinity
    let minY = Infinity
    let maxY = -Infinity
    let minZ = Infinity
    let maxZ = -Infinity
    for (const i of visIdx) {
      const x = nb.positions[i * 2]
      const y = nb.positions[i * 2 + 1]
      const z = zArr[i]
      if (x < minX) minX = x
      if (x > maxX) maxX = x
      if (y < minY) minY = y
      if (y > maxY) maxY = y
      if (z < minZ) minZ = z
      if (z > maxZ) maxZ = z
    }
    if (visIdx.length === 0) {
      minX = maxX = minY = maxY = minZ = maxZ = 0
    }
    const center = new Vector3((minX + maxX) / 2, (minY + maxY) / 2, (minZ + maxZ) / 2)
    // Halbe Box-Diagonale als Sphären-Radius (konservativ, umschließt sicher).
    const radius = Math.max(1, Math.hypot(maxX - minX, maxY - minY, maxZ - minZ) / 2)
    // Distanz so, dass die Sphäre den vertikalen fov füllt; ×1.15 Rand.
    const dist = (radius / Math.sin(((cam.fov / 2) * Math.PI) / 180)) * 1.15
    // Blick von +Z mit leichtem Tilt: die XY-Ebene bleibt lesbar wie in 2D,
    // die semantischen Z-Ebenen werden als Tiefenstaffelung sichtbar (2.5D).
    const dir = new Vector3(0, -0.45, 1).normalize()
    cam.position.copy(center).addScaledVector(dir, dist)
    cam.near = Math.max(0.1, dist / 1000)
    cam.far = dist * 20
    cam.updateProjectionMatrix()
    ctl.target.copy(center)
    ctl.update()
    requestRender()
  }

  /** Voller Rebuild (Page ruft das z. B. nach off-band Recolor). */
  export function refresh(): void {
    if (renderer3 === null) return
    rebuild()
    requestRender()
  }
</script>

<!-- data-e2e-mask: WebGL-Pixel-Output ist nicht seed-stabil — das visual-Gate
     maskiert die Region (gleiches Muster wie GraphView, design 06 §4.3). -->
<div class="canvas" class:hovering data-e2e-mask bind:this={host}>
  <!-- Z-Achsen-Semantik (Kern-Feature dieses Renderers); UI-Sprache englisch -->
  <select class="zmode" bind:value={zMode} aria-label="Z axis semantic">
    <option value="hop">z: hop</option>
    <option value="time">z: time</option>
    <option value="flat">z: flat</option>
  </select>
  {#if tip !== null}
    <!-- Inline-Style NUR Geometrie (Lint-Gate) — Farben/Font im Style-Block -->
    <div class="tip" style:left={tip.x + 'px'} style:top={tip.y + 'px'}>{tip.label}</div>
  {/if}
</div>

<style>
  .canvas {
    position: relative;
    width: 100%;
    height: 100%;
    min-height: 0;
    overflow: hidden;
    /* U02-W4-Muster: gleiche Hintergrund-Quelle wie alle Kontrast-Gates —
       der alpha-Canvas lässt sie durchscheinen (Theme gratis). */
    background: var(--graph-bg);
  }
  /* three erzeugt den Canvas dynamisch (kein Svelte-Scope) — setSize läuft mit
     updateStyle=false, die Ausdehnung kommt vollständig von hier. */
  .canvas :global(canvas) {
    display: block;
    width: 100%;
    height: 100%;
  }
  .canvas.hovering :global(canvas) {
    cursor: pointer;
  }
  .zmode {
    position: absolute;
    top: 0.5rem;
    right: 0.5rem;
    /* Ordnet nur gegen den Geschwister-Canvas im lokalen Stacking-Context. */
    z-index: var(--z-popover);
    padding: 0.15rem 0.35rem;
    font-size: var(--fs-xs);
    color: var(--text);
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .tip {
    position: absolute;
    /* Über dem .zmode-Select — beide Popover-Ebene, DOM-Ordnung entscheidet. */
    z-index: var(--z-popover);
    max-width: 18rem;
    padding: 0.2rem 0.45rem;
    overflow: hidden;
    font-size: var(--fs-xs);
    color: var(--text);
    text-overflow: ellipsis;
    white-space: nowrap;
    pointer-events: none;
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
</style>
