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

## Flake-Politik, Quarantäne & Budgets (design 06 §5.4/§6.3)

**Flake-Prozess-Regel:** Retries sind deklarierter Infra-Schutz (CI: 1, lokal: 0), kein
Flake-Teppich. Jeder `flaky`-Status (Test bestand erst im Retry) erzeugt eine sichtbare
CI-Annotation (`.github/scripts/flake-annotations.sh`) und fließt in die nightly Trend-Zeile
(`.github/scripts/e2e-trend.sh`). **Ein Test, der zweimal in 14 Tagen flaky war, MUSS in
Quarantäne oder gefixt werden** — nicht optional.

**Quarantäne:** Eintrag in `e2e/quarantine.json` (Pflicht: `issue` + `since`) + `@quarantine`-
Tag am Test. Das PR-Gate schließt getaggte Tests aus (config `grepInvert`), nightly führt sie
WEITER aus (`CTX_E2E_QUARANTINE=1` — beobachtet, nicht vergessen). Harter Deckel > 5 ⇒ rot;
Tag↔Ledger-Bijektion 1:1 (Tag ohne Eintrag ⇒ rot „untracked", Eintrag ohne Tag ⇒ rot „stale") —
erzwungen in `e2e/contract/quarantine.test.ts`. Ledger leer = gesunder Default.

**Budgets (kalibriert, `.github/e2e-budget.json`):** e2e-Teilbudget (report.json-Wall) + Job-
Budget; Überschreitung annotiert, Job > 10 min ⇒ rot. Drei Läufe über dem e2e-Teilbudget ⇒
Sharding-Aktivierung (in ci.yml vorbereitet, NICHT aktiv — §6.3). **History-Budget (nightly):**
kumuliertes `__screenshots__`-Blob-Volumen der Git-History; Annotation ab 60 MB, dokumentierter
Eskalationspfad (Orphan-Branch/LFS) ab 150 MB — nie Auto-Fail (`.github/scripts/history-budget.sh`).

## Matrix

| Seite | Route | Rolle | States | Kern-Flow | Visual | ARIA | axe | Fokus-Walk | Mobile | Deny | Leak | Scale | Live |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| status | `/status` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) — 2 Debt-Entries | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| graph | `/graph` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| blocks | `/blocks` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) — 4 Debt-Entries | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | ✓ generiert (§5.6b) | `10k` (Deckel 300 @ `main.content ul.results > li`) | — (PV10) |
| chat | `/chat` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| settings | `/settings` | member | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| login | `login` | member | `default`, `error` | ✓ | ✓ 8 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| home | `/home` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| issues | `/issues` | member | `default`, `empty`, `search` | ✓ | ✓ 16 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | `10k` (Deckel 200 @ `main.content tr[data-issue-row]`) | — (PV10) |
| issue-detail | `/issues/:id` | member | `default`, `dialog-status` | ✓ | ✓ 12 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | `10k` (Deckel 150 @ `main.content li[data-comment]`) | — (PV10) |
| board | `/board` | member | `default`, `empty` | ✓ | ✓ 12 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | `board-10k` (Deckel 300 @ `main.content [data-board-card]`) | — (PV10) |
| settings-backends | `/settings/backends` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| settings-hues | `/settings/hues` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |
| admin | `/admin` | server-admin | `default`, `wizard` | ✓ | ✓ 8 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | ✓ generiert (server-admin → nächst-niedrigere Rolle) | — | EXEMPT | — (PV10) |
| admin-tenant-detail | `/admin/tenants/:id` | server-admin | `default` | ✓ | ✓ 4 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | ✓ generiert (server-admin → nächst-niedrigere Rolle) | — | EXEMPT | — (PV10) |
| admin-types | `/admin/types` | server-admin | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | ✓ generiert (server-admin → nächst-niedrigere Rolle) | — | EXEMPT | — (PV10) |
| tenant | `/tenant` | tenant-admin | `default` | ✓ | ✓ 4 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) — 1 Debt-Entry | ✓ (1440, Tab-Walk) | ✓ (390×844) | ✓ generiert (tenant-admin → nächst-niedrigere Rolle) | — | EXEMPT | — (PV10) |
| tenant-backends | `/tenant/backends` | tenant-admin | `default` | ✓ | ✓ 2 Shots (states×dark/light×1 VP) | ✓ 1 (Desktop; Mobile fällt mit dem Opt-out) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | OPT-OUT | ✓ generiert (tenant-admin → nächst-niedrigere Rolle) | — | EXEMPT | — (PV10) |
| notfound | `*` | member | `default` | ✓ | ✓ 4 Shots (states×dark/light×2 VP) | ✓ 2 (Desktop+Mobile) | ✓ 4 Scans (dark/light × 2 VP) | ✓ (1440, Tab-Walk) | ✓ (390×844) | — (role member: guard gated ist nur /admin,/tenant) | — | EXEMPT | — (PV10) |

## Kern-Pfade (flowDoc — W21: „verifiziert" meint prüfbar den Hauptpfad)

- **status**: Operator öffnet die Status-Übersicht und liest den Live-Zustand tile-vollständig: Health-Ampel mit Services, Profil-Quick-Toggle, Dream-Queue-Zahlen, Backend-Pool-Zeile und LLM-Telemetrie tragen die Fixture-DATEN (PV7: erste Inhalts-Asserts auf jede Tile — Inventur §5 hatte keine einzige).
- **graph**: Nutzer öffnet die Cluster-Übersicht des Korpus: die Sigma-Canvas mountet und trägt exakt die drei Fixture-Cluster als Knoten (Semantik über den __ctxGraph-Hook, Pixel bleiben maskiert).
- **blocks**: Nutzer durchstöbert den Korpus: die Master-Liste füllt sich aus der Suche, ein Klick auf einen Treffer öffnet das Detail mit dem Block-Inhalt (Lese-Kernpfad des Block-Workbench).
- **chat**: Nutzer sendet einen Prompt, die Antwort streamt über SSE ins DOM und die Session persistiert in der Sidebar (sseRoute-Kernpfad, §7-PV7) — inklusive der XSS-Probe-Familie §5.6a auf der gerenderten Antwort (ctx:-Citation-Rewrite, escaped Raw-HTML, encodierte ctx:-Payloads, DOMPurify-URI-Allowlist).
- **settings**: Admin editiert eine Einstellung im Katalog (Edit-Roundtrip, §7-PV7): dream-Karte aufklappen (Karten starten eingeklappt) → dream.enabled-Switch kippen → Save der Gruppe → genau EIN PUT /api/settings/dream.enabled mit {value:false} auf dem Draht (postData-Assert) → Echo wendet source=db an und der Dirty-Zähler verschwindet. Danach Fuzzy-Suche als Zweit-Probe: Tippfehler-Query findet den Key über die Levenshtein-Stufe.
- **login**: Nutzer fügt den API-Key ein und meldet sich an: POST /auth/login tauscht ihn gegen httpOnly-Cookies (R4) → whoami hydriert → Shell mountet → Member-Landing /home. Der Fehl-Key-Pfad (Fehlerband, NIE Shell) ist der deklarierte error-State + freie Negativ-Probe in smoke.spec.ts.
- **home**: Member landet auf /home, liest seinen Korpus-Zuschnitt (Write-Scope home, Read-Scopes home+shared, Rolle, Tenant), sieht bei viewWorkflow die Workflow-Kachel und springt über „Browse blocks" in die Korpus-Fläche.
- **issues**: Ein Member öffnet /issues: der einzige Projekt-Scope wird auto-selektiert (Picker), die virtualisierte Liste füllt sich, und der Filter-Zustand (inkl. ?scope=) wandert in die URL (deep-linkbar).
- **issue-detail**: Deep-Link auf /issues/:id lädt Issue + Kommentare: der Markdown-Body und jeder Kommentar rendern über die sanitizende lib/markdown-Pipeline, das Sync-Badge zeigt den Forge-Zustand, und im schreibbaren Scope erscheinen Composer + Status-/Titel-Mutation (sonst read-only).
- **board**: Deep-Link auf /board rendert die Status-Spalten aus dem Board-Wire (Reihenfolge == Wire-Order == Type-Config), jede mit ihrem Wire-Count; terminale Spalten (registry workflow.terminal) starten eingeklappt, offene zeigen ihre Karten; Desktop-Klick öffnet das Detail als Fenster (lib/windows), Mobile (<SM) blättert einspaltig per Column-Pager (Tap navigiert zu /issues/:id).
- **settings-backends**: Admin öffnet den Pool-&-Vault-Editor unter dem Settings-Crumb: Pool-Tabelle (Fixture-Empty-State als Positiv-Kontrolle des Ladepfads) und Secrets-Vault mounten hinter dem admin-Gate.
- **settings-hues**: Admin öffnet die Farb-Override-Fläche unter dem Settings-Crumb, wählt eine Kategorie und setzt ihren Graph-Hue am Regler (optimistische Vorschau + PUT); der Crumb führt zurück in den Katalog.
- **admin**: Server-Admin liest das Tenant-Register (Slug, Status-Ampel), prüft die Scope-Map und steigt über den Slug-Link in die Tenant-Detailseite ein.
- **admin-tenant-detail**: Server-Admin öffnet die Tenant-Detailseite und setzt die Tages-Kosten-Quota eines Scopes (set → re-get → saved-Marker) — der Verwaltungs-Kernpfad der Seite.
- **admin-types**: Server-Admin öffnet die Type-Registry, liest die Typen mit Source-Badge (builtin/tenant) + Policy-Zusammenfassung und öffnet das deklarative Policy-Formular eines Typs (Edit-Kernpfad); builtin-Typen sind nicht löschbar (Delete disabled — die Komfort-Hälfte des Doppel-Schutzes, Server ist das Gate).
- **tenant**: Tenant-Admin verwaltet die Schlüssel seines Tenants: Tabelle listet die Keys, „+ New key" mintet einen neuen Key mit Reveal-once-Plaintext (Kernpfad der Selbstverwaltung).
- **tenant-backends**: Tenant-Admin öffnet den Backend-Pool unter dem Tenant-Crumb: die Pool-Tabelle mountet hinter dem tenant-admin-Self-Gate (Fixture-Empty-State als Positiv-Kontrolle des Ladepfads), der Vault bleibt server-admin-only ausgeblendet, und der Crumb führt zurück auf /tenant.
- **notfound**: Nutzer landet auf einer unbekannten Route, sieht die 404-Auskunft und kehrt über den Status-Link zurück (Guard-Ist: /status ist member-erreichbar — Rail↔Guard-Divergenz, PV4-Befund).

## Ausnahmen-Ausweis (design 06 §4.3c — Opt-outs sind sichtbare Entscheidungen)

### Scale-Exempts

- **status**: Aggregat-Ansicht mit fixer Kachelzahl; die einzigen Listen (backends, llm_24h) sind server-seitig auf die Backend-/Pipeline-Anzahl begrenzt — kein 10k-Wachstumspfad.
- **graph**: Canvas-Fläche: der Overview-Endpoint aggregiert server-seitig zu Clustern (stats.truncated deckelt), im DOM stehen keine Listen-Knoten — der 10k-DOM-Deckel ist gegenstandslos; Graph-Semantik läuft über den __ctxGraph-Hook (S12).
- **chat**: Thread-Ansicht rendert nur die aktive Konversation; die Sitzungsliste ist im Ist-Bestand die einzige Liste und ohne 10k-Pfad im Mock — die Scale-Pflicht greift mit den virtualisierten Achse-04-Listenflächen (design 06 §6.2).
- **settings**: Settings-Katalog ist eine bounded, server-definierte Konfigurationsliste (Dutzende Keys, kein nutzergetriebenes Wachstum) — keine 10k-Dimension.
- **login**: Login-Maske: ein einzelnes Formular ohne Datenliste — es existiert kein 10k-Wachstumspfad.
- **home**: Capability-Screen mit fixer Kartenzahl aus whoami (Write-Scope, Read-Scopes, Rolle, Tenant) — keine Liste, kein 10k-Pfad.
- **settings-backends**: Backend-Pool + Vault sind bounded Betreiber-Listen (Provider-Backends, Secret-Namen) — kein nutzergetriebenes 10k-Wachstum.
- **settings-hues**: Kategorie-Liste ist eine bounded Betreiber-/Tenant-Menge (Block-Kategorien) — die Override-Zeilen skalieren mit der Kategorie-Anzahl, nicht mit Blöcken (kein 10k-Nutzerpfad).
- **admin**: Tenant-Register + Scope-Map sind Betreiber-Aggregate (Anzahl Tenants/Scopes, server-seitig überschaubar) — der 10k-Korpus-Pfad läuft über /api/search-Flächen, nicht über dieses Register.
- **admin-tenant-detail**: Detailseite EINES Tenants: Register-Karte + eine QuotaForm pro eigenem Scope (server-seitig bounded über tenant_limits) — keine 10k-Liste.
- **admin-types**: Type-Registry ist eine bounded, betreiber-definierte Liste (≪ 100 Zeilen: builtin-Defaults ∪ Tenant-Overlays) — kein nutzergetriebener 10k-Pfad (design 04 §5.5-Zeile /admin/types).
- **tenant**: Key-/Scope-Tabellen sind durch tenant_limits gedeckelt (max_keys/max_scopes, Migration 069) — strukturell kein 10k-Pfad.
- **tenant-backends**: Backend-Pool ist eine bounded Betreiber-Liste (Provider-Backends pro Tenant, server-seitig überschaubar) — kein nutzergetriebener 10k-Pfad (design 04 §5.5-Zeile /tenant/backends).
- **notfound**: Statische 404-Seite ohne Daten — kein 10k-Pfad.

### Mobile-Opt-outs

- **status**: Mobile-Baselines der PV4-Erstbelegung existieren noch nicht — Voll-Satz (Re-Freeze der Bestands-Areas) ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); PV7 friert nur die NEUEN Seiten ein.
- **graph**: Mobile-Baselines der PV4-Erstbelegung existieren noch nicht — Voll-Satz (Re-Freeze der Bestands-Areas) ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); PV7 friert nur die NEUEN Seiten ein.
- **blocks**: Mobile-Baselines der PV4-Erstbelegung existieren noch nicht — Voll-Satz (Re-Freeze der Bestands-Areas) ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); PV7 friert nur die NEUEN Seiten ein.
- **chat**: Mobile-Baselines der PV4-Erstbelegung existieren noch nicht — Voll-Satz (Re-Freeze der Bestands-Areas) ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); PV7 friert nur die NEUEN Seiten ein.
- **settings**: Mobile-Baselines der PV4-Erstbelegung existieren noch nicht — Voll-Satz (Re-Freeze der Bestands-Areas) ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); PV7 friert nur die NEUEN Seiten ein.
- **admin-types**: Admin-Verwaltungsfläche, Ziel-Viewport dark+light × Desktop (design 04 §5.5-Zeile /admin/types); die Mobile-Baseline landet mit dem sequenzierten Voll-Satz-Re-Freeze (design 06 §9.3), wie die PV4-Erstbelegung.
- **tenant-backends**: Tenant-Verwaltungsfläche, Ziel-Viewport dark+light × Desktop (design 04 §5.5-Zeile /tenant/backends); die Mobile-Baseline landet mit dem sequenzierten Voll-Satz-Re-Freeze (design 06 §9.3), wie /admin/types.

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
| `/tenant` | color-contrast | light/desktop | `.own .status-cell .badge.ok` | 1 | 2026-07-02 | .project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §3.3 (interim ref until Achse-02 issues exist) — TenantPage own-key status badge: --ok text on --surface-2 (own-row highlight) misses 4.5:1 in the light theme only; PV7 surfaces it as the first /tenant contract's Ist-Verstoß |

Gesamt: 7 Einträge. Wachstum (neue Einträge oder Node-Zuwachs) ⇒ rot bzw.
`[baseline]`-Marker-Pflicht (.hooks/commit-msg); behobene Einträge MÜSSEN raus (stale ⇒ rot).

## Ausstehende Kontrakte (Matrix-Gate: jede Route trägt Kontrakt XOR Pending-Eintrag)

| Route | Grund |
|---|---|
| `/guard` | Guard review queue (needs_review pipeline W4) — page shipped dark-launched; its PageContract (list/pair/resolve flows against a seeded flagged corpus) lands with the guard e2e wave. |
