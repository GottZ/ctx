# e2e-Abdeckungs-Matrix — Seite × Prüfart (generiert)

> GENERIERT von `bun run e2e:matrix` (e2e/contract/coverage.ts) — NICHT von Hand
> editieren: der vitest-Drift-Test (coverage.test.ts) und der Matrix-Meta-Test
> (matrix.spec.ts) vergleichen diese Datei gegen das Regenerat (design 06 §3.4).

## Was ein grüner Mock-Tier-Lauf beweist — und was nicht (design 06 §5.1)

**Beweist:** jede Registry-Seite rendert in jedem deklarierten Zustand pixel-identisch
zur eingefrorenen Referenz (@visual, nur im digest-gepinnten Container); der primäre
Nutzungspfad ist bedienbar (@flow); guard-gegatete Seiten weisen niedrigere Rollen ab
(generierte Deny-Tests — CLIENT-Guard-Schicht); tenant-scoped Flächen rendern nie den
Fremd-Sentinel (Leak-Probe mit Positiv-Kontrolle); Listen-Flächen halten den deklarierten
DOM-Deckel bei 10k Fixture-Items (Scale-Zustand mit echtem Keyset-Cursor); kein `pageerror`.

**Beweist NICHT:** Server-Enforcement (403/Scope-Isolation — der Mock liefert, was der
Test erwartet), Shape-Aktualität der Fixtures (W10), SSE-Transport-Verhalten,
Backend-Performance. Adressen: Go-Integration-Suite + Live-Tier (PV10), Achse 02 (Latenz-Budgets).

**Noch nicht generiert (ehrlich):** Live-Tier-Spalte (PV10) — als Spalte bereits
ausgewiesen, Wert `— (PV10)` bis zur Welle. Das axe-Gate (WCAG 2.2 AA inkl. target-size)
+ der Fokus-Walk laufen seit PV5; axe-`incomplete`-Ergebnisse (Kontrast-Blindstellen:
Gradients, überlappende Elemente) hängen als `axe-incomplete`-Attachment am Report und
sind zu triagieren, nie stillschweigend grün (design 06 §4.5). ARIA-Snapshots (PV6) sind
committete YAML-Struktur-Gates (`__screenshots__/smoke.spec/<seite>--aria--<vp>.yml`) —
plattform-/font-unabhängig, laufen auf Host UND Container; Änderungen sind [baseline]-
Marker-pflichtig (der commit-msg-Hook deckt ALLE Pfade unter `__screenshots__/`, nicht nur PNGs).

## Matrix

| Seite | Route | Rolle | States | Kern-Flow | Visual | ARIA | axe | Fokus-Walk | Mobile | Deny | Leak | Scale | Live |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| status | `/status` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) — 2 Debt-Entries | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| graph | `/graph` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| blocks | `/blocks` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) — 4 Debt-Entries | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | ✓ generiert (§5.6b) | `10k` (Deckel 300 @ `main.content ul.results > li`) | — (PV10) |
| chat | `/chat` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| settings | `/settings` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |

## Kern-Pfade (flowDoc — W21: „verifiziert" meint prüfbar den Hauptpfad)

- **status**: Operator öffnet die Status-Übersicht und liest den Live-Zustand: Backend-Zeile mit Namen und die Dream-Telemetrie sind inhaltlich gefüllt (erste Inhalts-Asserts; PV7 vertieft auf Tile-Vollständigkeit).
- **graph**: Nutzer öffnet die Cluster-Übersicht des Korpus: die Sigma-Canvas mountet und trägt exakt die drei Fixture-Cluster als Knoten (Semantik über den __ctxGraph-Hook, Pixel bleiben maskiert).
- **blocks**: Nutzer durchstöbert den Korpus: die Master-Liste füllt sich aus der Suche, ein Klick auf einen Treffer öffnet das Detail mit dem Block-Inhalt (Lese-Kernpfad des Block-Workbench).
- **chat**: Nutzer erreicht den Chat mit bedienbarem Composer (Eingabe + Send-Gate). Benannte PV4-Grenze: der eigentliche Kern-Pfad senden→streamen→persistieren ist per §7-PV7 dem sseRoute-primaryFlow der PV7-Welle zugewiesen — dieser Flow deckt den Renderpfad, nicht den Stream.
- **settings**: Admin liest den Settings-Katalog: die Katalogzeilen rendern Key + Wert aus dem Server-Fixture. Benannte PV4-Grenze: der Edit-Roundtrip (primaryFlow laut §4.1) ist per §7-PV7 der PV7-Welle zugewiesen.

## Ausnahmen-Ausweis (design 06 §4.3c — Opt-outs sind sichtbare Entscheidungen)

### Scale-Exempts

- **status**: Aggregat-Ansicht mit fixer Kachelzahl; die einzigen Listen (backends, llm_24h) sind server-seitig auf die Backend-/Pipeline-Anzahl begrenzt — kein 10k-Wachstumspfad.
- **graph**: Canvas-Fläche: der Overview-Endpoint aggregiert server-seitig zu Clustern (stats.truncated deckelt), im DOM stehen keine Listen-Knoten — der 10k-DOM-Deckel ist gegenstandslos; Graph-Semantik läuft über den __ctxGraph-Hook (S12).
- **chat**: Thread-Ansicht rendert nur die aktive Konversation; die Sitzungsliste ist im Ist-Bestand die einzige Liste und ohne 10k-Pfad im Mock — die Scale-Pflicht greift mit den virtualisierten Achse-04-Listenflächen (design 06 §6.2).
- **settings**: Settings-Katalog ist eine bounded, server-definierte Konfigurationsliste (Dutzende Keys, kein nutzergetriebenes Wachstum) — keine 10k-Dimension.

### Mobile-Opt-outs

- **status**: Mobile-Baselines existieren noch nicht — Voll-Satz ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); Opt-out fällt mit dem PV7-Voll-Satz.
- **graph**: Mobile-Baselines existieren noch nicht — Voll-Satz ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); Opt-out fällt mit dem PV7-Voll-Satz.
- **blocks**: Mobile-Baselines existieren noch nicht — Voll-Satz ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); Opt-out fällt mit dem PV7-Voll-Satz.
- **chat**: Mobile-Baselines existieren noch nicht — Voll-Satz ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); Opt-out fällt mit dem PV7-Voll-Satz.
- **settings**: Mobile-Baselines existieren noch nicht — Voll-Satz ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); Opt-out fällt mit dem PV7-Voll-Satz.

### Mask-Budget-Overrides (40 %-Deckel, §4.3b)

- **graph**: full-bleed sigma canvas (ForceAtlas2 layout is not seed-stable); graph SEMANTICS stay asserted via the __ctxGraph hook (primaryFlow + graph-palette special) — Referenz: .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §4.3 (interim ref until Achse-02 issues exist)

### axe-Excludes (design 06 §4.3c — wirksam im PV5-Gate)

- keine

### a11y-Baseline-Debt (e2e/a11y-baseline.json — Node-Count-Freeze, shrink-only)

| Seite | Regel | Kontext | Selektoren | Nodes | Seit | Referenz |
|---|---|---|---|---|---|---|
| `/status` | color-contrast | light/desktop | `.modes button.active` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — DreamTile mode segment: --accent on --accent-dim misses 4.5:1 in the light theme only |
| `/status` | color-contrast | light/mobile | `.modes button.active` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — DreamTile mode segment: --accent on --accent-dim misses 4.5:1 in the light theme only |
| `/blocks` | select-name | dark/desktop | `.panel fieldset select` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — FilterPanel category select has no accessible name (the fieldset legend does not label the control) |
| `/blocks` | select-name | dark/mobile | `.panel fieldset select` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — FilterPanel category select has no accessible name (the fieldset legend does not label the control) |
| `/blocks` | select-name | light/desktop | `.panel fieldset select` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — FilterPanel category select has no accessible name (the fieldset legend does not label the control) |
| `/blocks` | select-name | light/mobile | `.panel fieldset select` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — FilterPanel category select has no accessible name (the fieldset legend does not label the control) |

Gesamt: 6 Einträge. Wachstum (neue Einträge oder Node-Zuwachs) ⇒ rot bzw.
`[baseline]`-Marker-Pflicht (.hooks/commit-msg); behobene Einträge MÜSSEN raus (stale ⇒ rot).

## Ausstehende Kontrakte (PV7 leert diese Liste — Matrix-Gate)

| Route | Grund |
|---|---|
| `/home` | PV7 (design 06 §7-PV7): Member-Landing-Kontrakt |
| `/settings/backends` | PV7 (design 06 §7-PV7): Backends-Editor-Kontrakt |
| `/admin` | PV7 (design 06 §7-PV7): Server-Admin-Kontrakt inkl. generiertem Deny |
| `/admin/tenants/:id` | PV7 (design 06 §7-PV7): Template-Kontrakt mit Fixture-Param |
| `/tenant` | PV7 (design 06 §7-PV7): Tenant-Admin-Kontrakt inkl. generiertem Deny |
| `*` | PV7 (design 06 §7-PV7): NotFound-Kontrakt |
| `login` | PV7 (design 06 §7-PV7): Login-Gate liegt VOR dem Router (src/App.svelte) — Mock-Variante + Fehl-Key-Pfad |
