<script lang="ts">
  import type { Snippet } from 'svelte'
  import { DomProjector } from '../../lib/windows/window-projection'
  import type { WindowStore, WinState } from '../../lib/windows/windows.svelte'

  // The render-leaf (design 07-§C): the single DOM-px point of the window
  // system. It consumes the store's logical rect (lu) THROUGH the DomProjector
  // and paints it; the store stays renderer-agnostic. For WebXR this whole leaf
  // is replaced by a 3D-panel component consuming a QuadPose — the store and
  // store.move/resize(lu) stay untouched (// WebXR: markers below).
  //
  // Content-Snippet-Injection (U02, design 04-§4.6.2): das Fenster rendert im
  // Body NUR das injizierte `content(win, titleId)`-Snippet — keine Graph-Prop,
  // kein hartkodierter BlockDetailContent mehr. Der Graph-Host reicht
  // BlockDetailContent durch; ein Board-Host einen Issue-Renderer.
  let {
    win,
    store,
    content,
    sheet = false,
  }: {
    win: WinState
    store: WindowStore
    /** Fenster-Inhalt; titleId ist die id des Host-<h2> (aria-labelledby). */
    content: Snippet<[WinState, string]>
    /** Mobile-Vollbild-Sheet: unterdrückt inline-px/Drag/Resize, rendert inset:0. */
    sheet?: boolean
  } = $props()

  // Stable + unique without Math.random — win.id is a Block-UUID (vgl.
  // ConfirmDialog.svelte:97, das fuer generierte ids Math.random braucht).
  // svelte-ignore state_referenced_locally
  const titleId = `wt-${win.id}`
  // RenderRect (px) — only in the 2D renderer.
  // WebXR: SpatialProjector<QuadPose> liefert hier eine 3D-Pose statt RenderRect.
  const r = $derived(DomProjector.toRender(win.rect, store.surface))

  // Autofokus-Ziel beim Open: der Fenster-Container (tabindex=-1). Der Sigma-
  // Node-Klick hat KEIN DOM-Target → ohne explizites Ziel landete der Fokus auf
  // <body>. Das deps-freie $effect feuert einmal, sobald `el` gebunden ist.
  let el = $state<HTMLElement>()
  $effect(() => {
    el?.focus()
  })

  // Fokus-Rückgabe beim Restore (U04-W3, design 04-§4.3): mit keepMinimized ist
  // Restore kein Remount → der Mount-Autofokus oben feuert dabei nicht. Dieser
  // ADDITIVE Effect deckt genau die true→false-Flanke von win.minimized ab: er
  // feuert NICHT beim Open (wasMinimized=false ∧ minimized=false → keine Flanke)
  // und NICHT bei fremden Invalidierungen. Auf dem Board-Host (destroy-basiert)
  // ist Restore ein frischer Mount → wasMinimized=false → inert, keine Doppel-
  // Fokussierung. wasMinimized ist bewusst plain let (kein $state, kein Self-Track).
  let wasMinimized = false
  $effect(() => {
    const m = win.minimized
    if (wasMinimized && !m) el?.focus() // nur die true→false-Flanke (Restore)
    wasMinimized = m
  })

  // Pointer-Drag/Resize ohne Lib. setPointerCapture haelt den Pointer-Stream im
  // Window → Events erreichen den Sigma-Canvas NICHT → kein Kamera-Pan.
  // WebXR: derselbe Pfad nimmt Controller-/Hand-Ray-Deltas direkt als lu.
  function startDrag(e: PointerEvent): void {
    // A pointerdown on the min/close buttons must NOT start a drag: capturing
    // the pointer here would steal their click (pointerup redirects to the
    // capture target). Let the button own its click.
    if ((e.target as HTMLElement).closest('button')) return
    const handle = e.currentTarget as HTMLElement
    handle.setPointerCapture(e.pointerId)
    let lastX = e.clientX
    let lastY = e.clientY
    const onMove = (ev: PointerEvent): void => {
      const { dx, dy } = DomProjector.toLogicalDelta(ev.clientX - lastX, ev.clientY - lastY, store.surface)
      store.move(win.id, dx, dy)
      lastX = ev.clientX
      lastY = ev.clientY
    }
    const onUp = (ev: PointerEvent): void => {
      handle.releasePointerCapture(ev.pointerId)
      handle.removeEventListener('pointermove', onMove)
      handle.removeEventListener('pointerup', onUp)
    }
    handle.addEventListener('pointermove', onMove)
    handle.addEventListener('pointerup', onUp)
  }

  function startResize(e: PointerEvent): void {
    const handle = e.currentTarget as HTMLElement
    handle.setPointerCapture(e.pointerId)
    let lastX = e.clientX
    let lastY = e.clientY
    const onMove = (ev: PointerEvent): void => {
      const { dx, dy } = DomProjector.toLogicalDelta(ev.clientX - lastX, ev.clientY - lastY, store.surface)
      store.resize(win.id, dx, dy)
      lastX = ev.clientX
      lastY = ev.clientY
    }
    const onUp = (ev: PointerEvent): void => {
      handle.releasePointerCapture(ev.pointerId)
      handle.removeEventListener('pointermove', onMove)
      handle.removeEventListener('pointerup', onUp)
    }
    handle.addEventListener('pointermove', onMove)
    handle.addEventListener('pointerup', onUp)
  }

  function onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.stopPropagation()
      store.close(win.id)
    }
  }
</script>

<!-- non-modal dialog: mehrere offen, KEIN Fokus-Trap (kein <dialog>/showModal,
     vgl. ConfirmDialog.svelte:101). tabindex=-1 = programmatisch fokussierbar
     (Autofokus beim Open). pointerdown → z-raise (vor DOM-Fokus). -->
<div
  class="window"
  class:sheet
  class:minimized={!sheet && win.minimized}
  bind:this={el}
  tabindex="-1"
  role="dialog"
  aria-modal="false"
  aria-labelledby={titleId}
  style={sheet
    ? ''
    : `left:${r.left}px; top:${r.top}px; width:${r.width}px; height:${r.height}px; z-index:calc(var(--z-window) + ${win.z})`}
  onpointerdown={() => store.focus(win.id)}
  onkeydown={onKeydown}
>
  <!-- WebXR: dieser inline-px-style ist der einzige DOM-px-Bind; der Spatial-
       Renderer ersetzt ihn durch eine 3D-Pose. Sheet (Mobile) traegt inset:0
       per CSS, keine px. -->
  <!-- Drag is a pointer-only, non-essential enhancement (design 07-§C a11y:
       position carries no exclusive information; all content is Tab-reachable
       and the window itself is the role=dialog). No keyboard handler needed. -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="titlebar" onpointerdown={sheet ? undefined : startDrag}>
    {#if !sheet}<span class="grip" aria-hidden="true">⠿</span>{/if}
    <span class="spacer"></span>
    <button class="act" aria-label="minimize" type="button" onclick={() => store.minimize(win.id)}>–</button>
    <button class="act" aria-label="close" type="button" onclick={() => store.close(win.id)}>×</button>
  </div>
  <div class="body">
    {@render content(win, titleId)}
  </div>
  {#if !sheet}
    <!-- pointer-only resize grip; kein Resize im Sheet. -->
    <div class="resize" onpointerdown={startResize} aria-hidden="true"></div>
  {/if}
</div>

<style>
  /* BEIDE Regeln Pflicht: ohne position:absolute wirken die Inline-left/top-px
     NICHT, die Fenster stacken im Fluss. */
  .window {
    position: absolute;
    pointer-events: auto; /* re-enabled die click-through-Schicht (.wm-root) */
    display: flex;
    flex-direction: column;
    overflow: hidden;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    box-shadow: var(--shadow-2);
  }
  .window:focus {
    outline: none;
  }
  /* U04-W3 (design 04-§4.3, §5-Nr.6): minimiert = keep-mounted, aber display:none
     → raus aus Layout, Tab-Order UND a11y-Tree (kein Screenreader-Geisterfenster,
     korrekte getByRole-Zählung). Restore = kein Remount → Scroll/Content bleiben.
     Nur der keepMinimized-Host (Graph) rendert überhaupt minimierte Fenster. */
  .window.minimized {
    display: none;
  }
  .window.sheet {
    position: fixed;
    inset: 0;
    z-index: var(--z-window); /* Mobile-Vollbild, kein px-Inline */
    border: none;
    border-radius: 0;
    box-shadow: none;
  }

  .titlebar {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-1) var(--space-2);
    border-bottom: 1px solid var(--border);
    background: var(--surface-2);
    cursor: move;
    user-select: none;
    touch-action: none; /* Pointer-Capture-Drag, kein Browser-Scroll/-Gesture */
  }
  .window.sheet .titlebar {
    cursor: default;
  }
  .grip {
    color: var(--text-faint);
    font-size: var(--fs-sm);
    line-height: var(--lh-solid);
  }
  .spacer {
    flex: 1;
  }
  .act {
    border: 1px solid transparent;
    border-radius: var(--radius);
    background: transparent;
    color: var(--text-dim);
    cursor: pointer;
    font-size: var(--fs-md);
    line-height: var(--lh-solid);
    padding: 0 var(--space-2);
  }
  .act:hover {
    border-color: var(--border);
    color: var(--text);
  }

  .body {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: var(--space-3);
  }

  /* Resize-Grip unten-rechts; nur Pointer (a11y: Position traegt keine
     exklusive Information, Inhalt voll per Tab erreichbar). */
  .resize {
    position: absolute;
    right: 0;
    bottom: 0;
    width: 1rem;
    height: 1rem;
    cursor: nwse-resize;
    touch-action: none;
    background: linear-gradient(135deg, transparent 50%, var(--border-strong) 50%);
  }
</style>
