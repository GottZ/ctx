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

**Noch nicht generiert (ehrlich):** ARIA-Snapshots (PV6), axe-Gate + Fokus-Walk (PV5),
Live-Tier-Spalte (PV10) — als Spalten bereits ausgewiesen, Wert `— (PVn)` bis zur Welle.

## Matrix

| Seite | Route | Rolle | States | Kern-Flow | Visual | ARIA | axe | Fokus-Walk | Mobile | Deny | Leak | Scale | Live |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| status | `/status` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | — (PV6) | — (PV5) | — (PV5) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| graph | `/graph` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | — (PV6) | — (PV5) | — (PV5) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| blocks | `/blocks` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×1 VP) | — (PV6) | — (PV5) | — (PV5) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | ✓ generiert (§5.6b) | `10k` (Deckel 300 @ `main.content ul.results > li`) | — (PV10) |
| chat | `/chat` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | — (PV6) | — (PV5) | — (PV5) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| settings | `/settings` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | — (PV6) | — (PV5) | — (PV5) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |

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

### axe-Excludes (ab PV5 wirksam, Ausweis-Pflicht ab sofort)

- keine

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
