<script lang="ts">
  import { toApiError, type ApiError } from '../../lib/api'
  import { searchBlocks, type SearchResult } from '../../lib/graph/api'

  // U03-W1 (§4.2): der Pick liefert das Fokus-Rückgabe-Ziel mit. `origin` ist
  // das Such-INPUT-Element (stabil über beide Stages, überlebt den Pick) — NIE
  // der Treffer-Button. (Seit W3 überlebt der Button den Pick zwar, das Input
  // bleibt aber die stabilere Fokus-Rückgabe, §4.2.)
  // U03-W3 (§4.7.1): `pageBusy` = der Ego-Load-Zustand der GraphPage; solange er
  // läuft, sind die Treffer-Buttons disabled → der stille Pick-Drop (busy-Guard
  // in setFocus) wird SICHTBAR statt kommentarlos verschluckt.
  let {
    onpick,
    pageBusy = false,
  }: { onpick: (id: string, origin: HTMLElement | null) => void; pageBusy?: boolean } = $props()

  let query = $state('')
  let results = $state<SearchResult[]>([])
  let busy = $state(false)
  let error = $state<ApiError | null>(null)
  let searched = $state(false)
  // Fokus-Rückgabe-Ziel: das Such-Input (bind:this), stabil über den Pick hinweg.
  let inputEl = $state<HTMLInputElement | null>(null)
  // U03-W4 (§4.7.2): die Trefferliste (bind:this) — Quelle der nativen Treffer-
  // Buttons für den Roving-Fokus. querySelectorAll statt eines Index-States, damit
  // die Fokus-Wahrheit immer das LIVE-DOM ist (kein Sync-Zweig, kein Stale-Index).
  let listEl = $state<HTMLUListElement | null>(null)

  // FTS entry point (design 05-§3.1): stemmed words match, substrings/typos
  // do not — plain /api/search, no LLM touched.
  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault()
    const q = query.trim()
    if (q === '' || busy) return
    busy = true
    error = null
    try {
      const res = await searchBlocks(q, 10)
      results = res.results
      searched = true
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }

  function pick(id: string): void {
    // U03-W3 (§4.7.1): die Liste bleibt nach dem Pick OFFEN (kein results=[]/
    // searched=false mehr) → Mehrfach-Pick aus einer Liste + Fehler-Retry ohne
    // Re-Submit. Der Query-Text stand ohnehin schon (bind:value). Das Such-Input
    // bleibt das Fokus-Rückgabe-Ziel (§4.2).
    onpick(id, inputEl)
  }

  // U03-W3 (§4.7.1): EIN Escape-Handler am gemeinsamen Wrapper. <form> und
  // <ul class="results"> sind Geschwister — ein Handler nur am <form> erreichte
  // den fokussierten Treffer-Button (im <ul>) NICHT. Liste offen → schließen +
  // preventDefault (verhindert zugleich das native Text-Löschen von
  // input[type=search] in Blink/WebKit). Liste zu → Event unangetastet (native
  // Semantik). Kein Konflikt mit dem Fenster-Escape (der hängt am Fenster-
  // Container, nicht an diesem Teilbaum).
  // U03-W4 (§4.7.2): Arrow-Roving hängt am SELBEN Wrapper-onkeydown wie Escape
  // (W3) — genau EINE Anbindung, denn nur der gemeinsame Wrapper sieht Events aus
  // BEIDEN Fokus-Orten (Such-Input UND Treffer-Button im <ul>, die im Layout
  // Geschwister sind; ein form-Handler erreichte den Button nicht). Mechanismus:
  // roving `focus()` auf den nativen <button>s — KEINE ARIA-Combobox-Rollen und
  // KEINE tabindex-Akrobatik (Buttons sind nativ fokussierbar), §4.7.2. Zugleich
  // zweite Hälfte des Tastatur-Escape-Loops (§4.5-Zweig B): nach Fenster-Close
  // (Fokus zurück im Input) re-öffnet ArrowDown die per W3 offen bleibende Liste.
  function onkeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      if (results.length === 0 && !searched) return
      event.preventDefault()
      results = []
      searched = false
      return
    }

    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return
    // Leere/geschlossene Liste → nichts tun, Event unangetastet (native Semantik).
    // pageBusy → die Treffer-Buttons sind disabled, `.focus()` darauf wäre wirkungs-
    // los; die Navigation gar nicht erst starten (Event durchlassen) ist der saubere
    // Weg (§Scope: pageBusy-Verhalten). Steady-State: pageBusy=false → volle Roving.
    if (results.length === 0 || pageBusy) return

    const buttons = listEl ? [...listEl.querySelectorAll<HTMLButtonElement>('button')] : []
    if (buttons.length === 0) return

    const idx = buttons.indexOf(document.activeElement as HTMLButtonElement)
    if (idx === -1) {
      // Fokus liegt nicht auf einem Treffer: nur ArrowDown AUS DEM INPUT springt
      // auf den ersten Treffer. ArrowUp im Input bleibt nativ (Cursor-Semantik).
      if (event.key === 'ArrowDown' && document.activeElement === inputEl) {
        event.preventDefault()
        buttons[0].focus()
      }
      return
    }

    // Fokus auf einem Treffer → roven. preventDefault verhindert das Seiten-Scrollen.
    event.preventDefault()
    if (event.key === 'ArrowDown') {
      buttons[Math.min(idx + 1, buttons.length - 1)].focus() // am letzten Treffer stehen bleiben
    } else if (idx === 0) {
      inputEl?.focus() // ArrowUp vom ersten Treffer → zurück ins Such-Input
    } else {
      buttons[idx - 1].focus()
    }
  }
</script>

<!-- U03-W3 (§4.7.1): gemeinsamer, CSS-neutraler Wrapper (display:contents → kein
     eigener Box im Layout, Kind-Flow im .search-card bleibt byte-gleich zum Ist,
     der nur block-flow um Form + Liste legt) mit EINEM onkeydown, das Escape
     unabhängig vom Fokus-Ort (Input ODER Treffer-Button) fängt. -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="searchbox" {onkeydown}>
  <form class="search" onsubmit={submit}>
    <input
      type="search"
      placeholder="search blocks (FTS) — pick a hit to focus its ego net"
      spellcheck="false"
      bind:this={inputEl}
      bind:value={query}
    />
    <button type="submit" disabled={busy || query.trim() === ''}>{busy ? '…' : 'Search'}</button>
  </form>

  {#if error}
    <p class="error" role="alert">{error.message}</p>
  {:else if searched && results.length === 0}
    <p class="empty" role="status">no FTS match — words are stemmed, substrings don't match</p>
  {:else if results.length > 0}
    <ul class="results" bind:this={listEl}>
      {#each results as r (r.id)}
        <li>
          <button type="button" disabled={pageBusy} onclick={() => pick(r.id)}>
            <span class="title">{r.title}</span>
            <span class="meta">{r.category} · {r.scope}</span>
            <span class="preview">{r.content_preview.slice(0, 140)}</span>
          </button>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  /* U03-W3: rein struktureller Wrapper (Escape-Handler-Träger). display:contents
     erzeugt keinen eigenen Box → Form + Liste bleiben layout-identisch zum Ist
     (der .search-card legt nur normalen Block-Flow um sie). */
  .searchbox {
    display: contents;
  }

  .search {
    display: flex;
    gap: var(--space-2);
  }
  .search input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }

  .error {
    margin: var(--space-2) 0 0;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .empty {
    margin: var(--space-2) 0 0;
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }

  .results {
    list-style: none;
    margin: var(--space-2) 0 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    max-height: 22rem;
    overflow-y: auto;
  }
  .results li + li {
    border-top: 1px solid var(--border);
  }
  .results button {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    border-radius: 0;
    padding: var(--space-2) var(--space-3);
  }
  .results button:hover {
    background: var(--surface-2);
  }
  /* U03-W3 (§4.7.1): während des Ego-Loads sind die Treffer disabled — sichtbar
     gedämpft statt still gedroppt. */
  .results button:disabled {
    cursor: default;
    opacity: 0.5;
  }
  .title {
    font-size: var(--fs-md);
    color: var(--text);
  }
  .meta {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .preview {
    font-size: var(--fs-xs);
    color: var(--text-dim);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 100%;
  }
</style>
