<script lang="ts">
  // Kategorie-Farb-Override-Fläche (AM-2, Web-UX U02-W6; design 02a §A4-W6).
  // Eigene Lazy-Sub-Route unter /settings (eigener Chunk, Muster
  // /settings/backends). Gate = admin/tenant-admin (dieselbe Gate wie der W5-
  // PUT/DELETE, RequireAdminOrTenantAdmin): Member SEHEN die Farben im Graph
  // (Member-Tier-GET), konfigurieren sie hier aber nicht. Der Server leitet den
  // Ziel-Scope aus der Auth ab (tenant-admin → eigener Scope, operator →
  // _global) — NIE aus dem Body (§A5-MT).
  //
  // Nur der HUE (HSL-Grad 0–359) wird überschrieben (§A2); sat/lum bleiben
  // Theme-Token. Wählen ⇒ optimistisch lokale Map + Live-Vorschau (Swatches +
  // Rad) + PUT; »zurücksetzen« ⇒ DELETE + Map-Eintrag weg (fällt auf den Seed,
  // §A3-3). KEIN Kontrast-Warn-Pfad: G1a deckt JEDEN wählbaren Hue ≥3:1 (§A4-W6)
  // — der Ist-Kontrast wird rein als Info gezeigt. Sprache: deutsch, lowercase-
  // mono-Idiom der W6-Karten; kein {@html} (render-Gate).
  import { onMount } from 'svelte'
  import { toApiError, type ApiError } from '../../../lib/api'
  import { listCategories } from '../../../lib/api/blocks'
  import { fetchCategoryHues, putCategoryHue, deleteCategoryHue } from '../../../lib/graph/api'
  import { categoryColor, categoryHue, hslToHex } from '../../../lib/graph/graph-client'
  import { readGraphPalette } from '../../../lib/graph/graph-theme'
  import { contrastRatio } from '../../../lib/graph/color-space'
  import { THEME_CHANGE_EVENT } from '../../../lib/theme/theme.svelte'
  import { session } from '../../../lib/auth.svelte'
  import HueWheel from './HueWheel.svelte'

  // Gate (§A4-W6): admin ODER tenant-admin. viewOpsSurfaces ist genau
  // tenant-admin-or-up (capabilities.ts) — deckt beide Tiers, deckungsgleich mit
  // dem Server-Schreib-Gate. Ein Member erreicht die Route zwar (kein Tier-Guard
  // auf /settings/*), sieht aber das Banner statt eines aussichtslosen Requests.
  const allowed = $derived(session.admin || session.caps.viewOpsSurfaces)

  let palette = $state(readGraphPalette())
  let categories = $state<string[]>([])
  // Aufgelöste sparse Override-Map {Kategorie → HSL-Hue}. $state via Neu-Zuweisung
  // (Map-Mutation triggert Svelte nicht) — jeder Schreibpfad ersetzt die Referenz.
  let overrides = $state<Map<string, number>>(new Map())
  let selected = $state<string | null>(null)
  let status = $state<'loading' | 'ready' | 'error'>('loading')
  let loadError = $state<ApiError | null>(null)
  let actionError = $state<string | null>(null)

  // Canvas-Hintergrund für die Ist-Kontrast-Info (dieselbe Quelle wie die
  // Kontrast-Gates: --graph-bg). Fallback dunkel, falls Token/SSR fehlt.
  let graphBg = $state('#0b0b0f')

  const entries = $derived(
    [...categories].sort().map((category) => ({
      category,
      hue: categoryHue(category, overrides),
      overridden: overrides.has(category),
    })),
  )

  const selectedHue = $derived(selected !== null ? categoryHue(selected, overrides) : 0)

  function swatch(category: string): string {
    return categoryColor(category, palette, overrides)
  }

  function contrastInfo(category: string): string {
    return contrastRatio(swatch(category), graphBg).toFixed(2)
  }

  async function load(): Promise<void> {
    status = 'loading'
    loadError = null
    try {
      // Parallel — keiner blockiert den anderen.
      const [cats, hues] = await Promise.all([listCategories(), fetchCategoryHues()])
      categories = cats.categories.map((c) => c.category)
      overrides = new Map(Object.entries(hues.hues))
      status = 'ready'
    } catch (err) {
      loadError = toApiError(err)
      status = 'error'
    }
  }

  function readBg(): void {
    if (typeof document === 'undefined') return
    const raw = getComputedStyle(document.documentElement).getPropertyValue('--graph-bg').trim()
    if (raw) graphBg = raw
  }

  onMount(() => {
    if (allowed) void load()
    readBg()
    // Live-Vorschau bei Theme-Wechsel: Palette + Canvas-Hintergrund neu lesen,
    // die Swatches/das Rad folgen (design 02a §A3 onThemeChange-Analogie).
    const onTheme = (): void => {
      palette = readGraphPalette()
      readBg()
    }
    window.addEventListener(THEME_CHANGE_EVENT, onTheme)
    return () => window.removeEventListener(THEME_CHANGE_EVENT, onTheme)
  })

  /** Optimistisch die lokale Map setzen (Vorschau) + PUT; Fehlschlag rollt zurück. */
  async function pick(hue: number): Promise<void> {
    if (selected === null) return
    const cat = selected
    const before = new Map(overrides)
    const next = new Map(overrides)
    next.set(cat, hue)
    overrides = next
    actionError = null
    try {
      await putCategoryHue(cat, hue)
    } catch (err) {
      overrides = before // Rollback auf den letzten Server-bestätigten Stand
      actionError = toApiError(err).message
    }
  }

  /** Override löschen: optimistisch aus der Map + DELETE; fällt auf den Seed. */
  async function reset(category: string): Promise<void> {
    if (!overrides.has(category)) return
    const before = new Map(overrides)
    const next = new Map(overrides)
    next.delete(category)
    overrides = next
    actionError = null
    try {
      await deleteCategoryHue(category)
    } catch (err) {
      overrides = before
      actionError = toApiError(err).message
    }
  }

  function onSlider(e: Event & { currentTarget: HTMLInputElement }): void {
    void pick(Number(e.currentTarget.value))
  }
</script>

<section class="area">
  <header>
    <div class="crumb"><a href="/settings">Settings</a> / farb-overrides</div>
    <h1>kategorie-farben</h1>
    <p class="sub">
      pro kategorie den farb-hue überschreiben — das rad zeigt belegte hues perceptuell (oklch), damit die abstände
      sichtbar werden. gespeichert wird der hsl-hue-grad; sat/lum bleiben theme-token. der graph zieht die farben beim
      nächsten aufruf.
    </p>
  </header>

  {#if !allowed}
    <p class="banner" role="status">
      nur-lese-schlüssel — farb-overrides sind admin-/tenant-admin-gegated (der server antwortet mit 403). melde dich mit
      einem admin-schlüssel an, um sie zu setzen. die farben im graph siehst du auch so.
    </p>
  {:else if status === 'loading'}
    <p class="state" aria-busy="true">lade kategorien &amp; overrides…</p>
  {:else if status === 'error'}
    <div class="error" role="alert">
      <p>{loadError?.message}</p>
      {#if loadError?.requestId}<p class="request-id">request {loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void load()}>erneut versuchen</button>
    </div>
  {:else}
    {#if actionError}
      <p class="problem error" role="alert">{actionError}</p>
    {/if}

    <div class="layout">
      <div class="wheel-col" aria-label="farb-rad">
        <HueWheel
          markers={entries}
          {selected}
          {palette}
          onpick={(hue) => void pick(hue)}
        />
        {#if selected !== null}
          <div class="picker" aria-label={`hue für ${selected}`}>
            <span class="pick-swatch" style="--dot-hue: {hslToHex(selectedHue, palette.nodeSat, palette.nodeLum)}"></span>
            <input
              type="range"
              min="0"
              max="359"
              step="1"
              value={selectedHue}
              aria-label={`hue-grad für kategorie ${selected}`}
              oninput={onSlider}
            />
            <span class="pick-val">{selectedHue}°</span>
          </div>
          <p class="pick-hint">
            wähle einen hue am rad oder per regler für <strong>{selected}</strong> · ist-kontrast gegen den canvas
            {contrastInfo(selected)}:1
          </p>
        {:else}
          <p class="pick-hint">wähle links eine kategorie, um ihren hue zu setzen.</p>
        {/if}
      </div>

      <ul class="cats" aria-label="kategorien">
        {#each entries as e (e.category)}
          <li class="cat" class:sel={e.category === selected}>
            <button type="button" class="row" aria-pressed={e.category === selected} onclick={() => (selected = e.category)}>
              <span class="cat-swatch" style="--dot-hue: {swatch(e.category)}"></span>
              <span class="cat-name">{e.category}</span>
              <span class="cat-meta">{e.hue}°{e.overridden ? ' · override' : ' · seed'}</span>
            </button>
            {#if e.overridden}
              <button type="button" class="ghost reset" onclick={() => void reset(e.category)}>zurücksetzen</button>
            {/if}
          </li>
        {/each}
        {#if entries.length === 0}
          <li class="empty">keine kategorien — lege blöcke an, dann erscheinen ihre kategorien hier.</li>
        {/if}
      </ul>
    </div>
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  .crumb {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-faint);
  }
  .crumb a {
    color: var(--text-dim);
  }
  h1 {
    margin: var(--space-1) 0 0;
    font-family: var(--font-mono);
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
    max-width: 46rem;
  }
  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: var(--fs-sm);
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }
  .problem {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--danger);
    background: var(--danger-dim);
  }
  .layout {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-4);
    align-items: flex-start;
  }
  .wheel-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: center;
    flex: 0 0 auto;
    width: min(20rem, 100%);
  }
  .picker {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 100%;
  }
  .picker input[type='range'] {
    flex: 1;
    accent-color: var(--accent);
  }
  .pick-swatch {
    width: 1.4rem;
    height: 1.4rem;
    border-radius: var(--radius);
    border: 1px solid var(--border-strong);
    background: var(--dot-hue);
    flex: 0 0 auto;
  }
  .pick-val {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
    min-width: 3ch;
    text-align: right;
  }
  .pick-hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
    text-align: center;
  }
  .cats {
    list-style: none;
    margin: 0;
    padding: 0;
    flex: 1;
    min-width: min(18rem, 100%);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .cat {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex: 1;
    text-align: left;
    padding: var(--space-1) var(--space-2);
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    cursor: pointer;
  }
  .row:hover {
    border-color: var(--text-dim);
  }
  .cat.sel .row {
    border-color: var(--accent);
  }
  .cat-swatch {
    width: 1.1rem;
    height: 1.1rem;
    border-radius: 50%;
    border: 1px solid var(--border-strong);
    background: var(--dot-hue);
    flex: 0 0 auto;
  }
  .cat-name {
    font-size: var(--fs-sm);
    color: var(--text);
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .cat-meta {
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    color: var(--text-faint);
    flex: 0 0 auto;
  }
  .ghost {
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text-dim);
    cursor: pointer;
  }
  .ghost:hover {
    border-color: var(--text-dim);
  }
  .empty {
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
</style>
