// COVERAGE.md generator (design 06 §3.4, wave PV4): the living artifact the
// USER reads; the registry is the artifact the MACHINE enforces. Regenerate:
//
//   bun run e2e:matrix
//
// Drift is pinned twice: the vitest test (coverage.test.ts) and the matrix
// meta-test (matrix.spec.ts) both fail when the committed file differs from
// the regenerate — a hand edit can never survive (PV4 gate c).

import { readFileSync } from 'node:fs'
import type { A11yLedger } from './a11y'
import { isScaleState, type PageContract } from './contract'
import { contracts, pendingContracts, type PendingContract } from './registry'

/** The committed a11y debt ledger — rendered into the Ausnahmen-Ausweis. */
function loadLedgerForCoverage(): A11yLedger {
  return JSON.parse(readFileSync(new URL('../a11y-baseline.json', import.meta.url), 'utf8')) as A11yLedger
}

function scaleCell(c: PageContract): string {
  return isScaleState(c.scale) ? `\`${c.scale.name}\` (Deckel ${c.scale.domCap.max} @ \`${c.scale.domCap.selector}\`)` : `EXEMPT`
}

function mobileCell(c: PageContract): string {
  return c.mobile === undefined ? '✓ (390×844)' : 'OPT-OUT'
}

function denyCell(c: PageContract): string {
  if (c.role === 'member') return '— (role member: guard gated ist nur /admin,/tenant)'
  return `✓ generiert (${c.role} → nächst-niedrigere Rolle)`
}

/** Render the full COVERAGE.md content — PURE, deterministic (drift-pinned). */
export function renderCoverage(cs: readonly PageContract[] = contracts, pending: readonly PendingContract[] = pendingContracts): string {
  const ledger = loadLedgerForCoverage()
  const lines: string[] = []
  lines.push('# e2e-Abdeckungs-Matrix — Seite × Prüfart (generiert)')
  lines.push('')
  lines.push('> GENERIERT von `bun run e2e:matrix` (e2e/contract/coverage.ts) — NICHT von Hand')
  lines.push('> editieren: der vitest-Drift-Test (coverage.test.ts) und der Matrix-Meta-Test')
  lines.push('> (matrix.spec.ts) vergleichen diese Datei gegen das Regenerat (design 06 §3.4).')
  lines.push('')
  lines.push('## Was ein grüner Mock-Tier-Lauf beweist — und was nicht (design 06 §5.1)')
  lines.push('')
  lines.push('**Beweist:** jede Registry-Seite rendert in jedem deklarierten Zustand pixel-identisch')
  lines.push('zur eingefrorenen Referenz (@visual, nur im digest-gepinnten Container); der primäre')
  lines.push('Nutzungspfad ist bedienbar (@flow); guard-gegatete Seiten weisen niedrigere Rollen ab')
  lines.push('(generierte Deny-Tests — CLIENT-Guard-Schicht); tenant-scoped Flächen rendern nie den')
  lines.push('Fremd-Sentinel (Leak-Probe mit Positiv-Kontrolle); Listen-Flächen halten den deklarierten')
  lines.push('DOM-Deckel bei 10k Fixture-Items (Scale-Zustand mit echtem Keyset-Cursor); kein `pageerror`.')
  lines.push('')
  lines.push('**Beweist NICHT:** Server-Enforcement (403/Scope-Isolation — der Mock liefert, was der')
  lines.push('Test erwartet), Shape-Aktualität der Fixtures (W10), SSE-Transport-Verhalten,')
  lines.push('Backend-Performance. Adressen: Go-Integration-Suite + Live-Tier (PV10), Achse 02 (Latenz-Budgets).')
  lines.push('')
  lines.push('**Noch nicht generiert (ehrlich):** ARIA-Snapshots (PV6), Live-Tier-Spalte (PV10) —')
  lines.push('als Spalten bereits ausgewiesen, Wert `— (PVn)` bis zur Welle. Das axe-Gate (WCAG 2.2 AA')
  lines.push('inkl. target-size) + der Fokus-Walk laufen seit PV5; axe-`incomplete`-Ergebnisse (Kontrast-')
  lines.push('Blindstellen: Gradients, überlappende Elemente) hängen als `axe-incomplete`-Attachment am')
  lines.push('Report und sind zu triagieren, nie stillschweigend grün (design 06 §4.5).')
  lines.push('')
  lines.push('## Matrix')
  lines.push('')
  lines.push('| Seite | Route | Rolle | States | Kern-Flow | Visual | ARIA | axe | Fokus-Walk | Mobile | Deny | Leak | Scale | Live |')
  lines.push('|---|---|---|---|---|---|---|---|---|---|---|---|---|---|')
  for (const c of cs) {
    const states = c.states.map((s) => `\`${s.name}\``).join(', ')
    const visualStates = c.states.length + (isScaleState(c.scale) ? 1 : 0)
    const vp = c.mobile === undefined ? 2 : 1
    const visual = `✓ ${visualStates * 2 * vp} Shots (states×dark/light×${vp} VP)`
    const debt = ledger.entries.filter((e) => e.page === c.route).length
    const axe = `✓ 4 Scans (dark/light × 2 VP)${debt > 0 ? ` — ${debt} Debt-Entr${debt === 1 ? 'y' : 'ies'}` : ''}`
    lines.push(
      `| ${c.name} | \`${c.route}\` | ${c.role} | ${states} | ✓ | ${visual} | — (PV6) | ${axe} | ✓ (1440, Tab-Walk) | ${mobileCell(c)} | ${denyCell(c)} | ${c.tenantScoped ? '✓ generiert (§5.6b)' : '—'} | ${scaleCell(c)} | — (PV10) |`,
    )
  }
  lines.push('')
  lines.push('## Kern-Pfade (flowDoc — W21: „verifiziert" meint prüfbar den Hauptpfad)')
  lines.push('')
  for (const c of cs) {
    lines.push(`- **${c.name}**: ${c.flowDoc}`)
  }
  lines.push('')
  lines.push('## Ausnahmen-Ausweis (design 06 §4.3c — Opt-outs sind sichtbare Entscheidungen)')
  lines.push('')
  lines.push('### Scale-Exempts')
  lines.push('')
  for (const c of cs) {
    if (!isScaleState(c.scale)) lines.push(`- **${c.name}**: ${c.scale.exempt}`)
  }
  lines.push('')
  lines.push('### Mobile-Opt-outs')
  lines.push('')
  for (const c of cs) {
    if (c.mobile !== undefined) lines.push(`- **${c.name}**: ${c.mobile.exempt}`)
  }
  lines.push('')
  lines.push('### Mask-Budget-Overrides (40 %-Deckel, §4.3b)')
  lines.push('')
  let anyMask = false
  for (const c of cs) {
    if (c.maskBudgetOverride !== undefined) {
      anyMask = true
      lines.push(`- **${c.name}**: ${c.maskBudgetOverride.reason} — Referenz: ${c.maskBudgetOverride.issue}`)
    }
  }
  if (!anyMask) lines.push('- keine')
  lines.push('')
  lines.push('### axe-Excludes (design 06 §4.3c — wirksam im PV5-Gate)')
  lines.push('')
  let anyAxe = false
  for (const c of cs) {
    for (const ex of c.axe?.exclude ?? []) {
      anyAxe = true
      lines.push(`- **${c.name}** \`${ex.selector}\`: ${ex.reason}`)
    }
  }
  if (!anyAxe) lines.push('- keine')
  lines.push('')
  lines.push('### a11y-Baseline-Debt (e2e/a11y-baseline.json — Node-Count-Freeze, shrink-only)')
  lines.push('')
  if (ledger.entries.length === 0) {
    lines.push('- keine Einträge — das axe-Gate läuft ohne eingefrorenen Debt.')
  } else {
    lines.push('| Seite | Regel | Kontext | Selektoren | Nodes | Seit | Referenz |')
    lines.push('|---|---|---|---|---|---|---|')
    for (const e of ledger.entries) {
      lines.push(
        `| \`${e.page}\` | ${e.rule} | ${e.theme}/${e.viewport} | \`${e.targets.join('`, `')}\` | ${e.nodes} | ${e.since} | ${e.issue} |`,
      )
    }
    lines.push('')
    lines.push(
      `Gesamt: ${ledger.entries.length} Einträge. Wachstum (neue Einträge oder Node-Zuwachs) ⇒ rot bzw.`,
    )
    lines.push('`[baseline]`-Marker-Pflicht (.hooks/commit-msg); behobene Einträge MÜSSEN raus (stale ⇒ rot).')
  }
  lines.push('')
  lines.push('## Ausstehende Kontrakte (PV7 leert diese Liste — Matrix-Gate)')
  lines.push('')
  lines.push('| Route | Grund |')
  lines.push('|---|---|')
  for (const p of pending) lines.push(`| \`${p.route}\` | ${p.reason} |`)
  lines.push('')
  return lines.join('\n')
}

// bun run e2e:matrix → (re)write e2e/COVERAGE.md next to the suite.
if (import.meta.main) {
  const { writeFileSync } = await import('node:fs')
  const target = new URL('../COVERAGE.md', import.meta.url)
  writeFileSync(target, renderCoverage())
  console.log(`coverage: wrote ${target.pathname} (${contracts.length} contracts, ${pendingContracts.length} pending)`)
}
