package rootmap

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// labelMaxRunes is the line-budget contract for a topic label (§4.3). Axis 01
// clamps at the source; the renderer clamps again — double clamping is
// tolerable, an unclamped line is not.
const labelMaxRunes = 60

// tsLayout drops seconds on purpose: the map is a 6-hour artefact, and every
// byte in a per-line-budgeted document has to earn itself.
const tsLayout = "2006-01-02T15:04Z"

// phrases is the renderer's COMPLETE string table: every byte of scaffolding
// the map writes, plus the three number-format separators. Nothing outside this
// struct may hold prose — a literal left behind in a writer is exactly the
// mixed-language artefact issue #34 reports.
//
// What deliberately stays OUT of the table (identifiers, not prose): tsLayout,
// gamma, cadence, the "n/a" share, the label-source vocabulary
// (llm/heuristic/repr_title), the "scope:", "repr=" and "scopes=" field markers,
// and the config-key names embedded in the phrases themselves
// (graph_overview.enabled, rebuild_timeout, root_map.super_max_nodes). Those are
// grep handles for operators and must survive translation verbatim.
type phrases struct {
	// Cap block (R2 — the freeze warning above everything else).
	capFrozen        string // ts + clause
	capSinceUnknown  string
	capCandidatesCap string
	capCandidates    string
	capStateOld      string
	capNeverBuilt    string

	// freeze maps a skip_reason to its plain-language clause; freezeUnknown is
	// the fallback template for a reason the table does not know.
	freeze        map[string]string
	freezeUnknown string

	// Head lines.
	headLine          string
	clusterState      string
	clusterStateNever string
	lastAttempt       string
	tenantOne         string
	tenantMany        string
	cadence           string
	labelsPrefix      string
	unstableIdentity  string

	// Coverage section.
	coverageNoTotal   string
	coverageKnowledge string
	coverageRaw       string
	operationalPrefix string
	operationalBlocks string
	operationalPolicy string
	operationalClose  string
	corpusRaw         string
	excludedPrefix    string
	afterClusterState string
	scopeOne          string
	scopeMany         string
	staleScopes       string

	// Sections and rows.
	topicsHeading string
	superSkipped  string
	superHeading  string
	superRow      string
	superCut      string
	footerHeading string
	footerSmall   string
	footerCapped  string
	noTitle       string

	// Empty states (D4 and its neighbours).
	emptyStale      string
	emptyNeverBuilt string
	emptyRebuilding string
	emptyNoClusters string

	// Number formatting. The map prints thousands groups and one decimal, and
	// which glyph separates them is part of the language, not of the layout.
	thousandsSep string
	decimalSep   string
	pctSuffix    string
}

// phrasesDE is the FROZEN legacy table: byte-identical to what the renderer
// emitted before the language became configurable. That is the regression
// contract of this wave — an unset or German dream.language must not rewrite a
// single live root-map block, and the golden test proves it.
//
// The freeze clauses are a deliberately CLOSED set: advisory-lock is absent
// because contention is not a freeze (another instance is building this very
// partition, successfully) and an unknown reason renders a generic clause rather
// than nothing — a cap the map cannot name is still a cap. Migration 126 added
// the sixth value: the freeze that reads like a bug and is not one, where the
// node set is the INTERSECTION of the retrieval-visible types and
// overview.include and an empty intersection selects nothing, so the rebuild
// refuses rather than retiring every topic the partition has.
var phrasesDE = phrases{
	capFrozen:        "!! Partition eingefroren: Rebuild seit %s %s\n",
	capSinceUnknown:  "unbekannt",
	capCandidatesCap: "!! (%s Kandidaten in diesem Scope; der globale Knoten-Cap wurde überschritten).\n",
	capCandidates:    "!! (%s Kandidaten in diesem Scope).\n",
	capStateOld:      "!! Cluster-Stand %s ist ALT.\n",
	capNeverBuilt:    "!! Diese Partition wurde nie erfolgreich gebaut.\n",

	freeze: map[string]string{
		"node-cap":         "per node-cap übersprungen",
		"timeout":          "im rebuild_timeout abgebrochen",
		"error":            "mit einem Fehler abgebrochen",
		"disabled":         "abgeschaltet (graph_overview.enabled=false)",
		"registry-unwired": "nicht gelaufen: Typ-Registry nicht verdrahtet",
		"empty-node-cut":   "abgebrochen: der Knotenschnitt war leer (Typ-Policy) — die Partition behält ihre Themen",
	},
	freezeUnknown: "übersprungen (%s)",

	headLine:          "ctx Root Map v%d | scope:%s | %s/%s Themen | %s/%s Blöcke geführt",
	clusterState:      "Cluster-Stand ",
	clusterStateNever: "Cluster-Stand: nie erfolgreich gebaut",
	lastAttempt:       "letzter Versuch ",
	tenantOne:         "Tenant",
	tenantMany:        "Tenants",
	cadence:           "erwartete Kadenz ~%s (%d %s)",
	labelsPrefix:      "Labels: ",
	unstableIdentity:  "Themen ohne stabile ID",

	coverageNoTotal:   "Deckung: %s Blöcke geclustert; Korpusgröße in dieser Runde nicht ermittelt.\n",
	coverageKnowledge: "Deckung: %s von %s Wissens-Blöcken geclustert (%s).\n",
	coverageRaw:       "Deckung: %s von %s aktiven Blöcken geclustert (%s; Rohzahl inkl. operativer Typen).\n",
	operationalPrefix: "Operativ, bewusst außerhalb der Themenkarte: ",
	operationalBlocks: " — %s Blöcke",
	operationalPolicy: " (Typ-Policy, keine Lücke",
	operationalClose:  ").\n",
	corpusRaw:         "Korpus-Rohzahl: %s aktive Blöcke im Lesefenster.\n",
	excludedPrefix:    "Außerhalb des Cluster-Schnitts per Typ-Policy: ",
	afterClusterState: "Alles nach dem Cluster-Stand ist hier NICHT enthalten.\n",
	scopeOne:          "Scope",
	scopeMany:         "Scopes",
	staleScopes:       "Hinweis: %d %s ohne aktive Cluster-Zeilen mit altem Erfolgs-Stempel (%s) — weder Deckung noch Lücke.\n",

	topicsHeading: "\n## Themen\n",
	superSkipped: "\nMeta-Ebene: übersprungen — der Supergraph liegt über root_map.super_max_nodes;\n" +
		"die Karte bleibt flach (der Haupt-Rebuild ist davon unberührt).\n",
	superHeading:  "\n## Themen-Gruppen (Meta-Ebene, γ=%s)\n",
	superRow:      "%s %s %s Blöcke · %s Themen\n",
	superCut:      "%s weitere Gruppen (Zeilenbudget).\n",
	footerHeading: "\n## Nicht einzeln geführt\n",
	footerSmall:   "%s Cluster mit ≤%d Blöcken (%s Blöcke) — link-arm, kein eigenes Thema.\n",
	footerCapped:  "%s weitere Cluster (%s Blöcke) wegen Zeilenbudget gekappt.\n",
	noTitle:       "(ohne Titel)",

	emptyStale:      "Keine Cluster in diesem Lesefenster — die vorhandenen Meta-Zeilen sind alte Erfolgs-Stempel ohne Partition.\n",
	emptyNeverBuilt: "Noch keine Cluster gebaut — diese Karte ist leer, nicht unvollständig.\n",
	emptyRebuilding: "Rebuild gelaufen (%s) und meldet %s Themen, im Lesefenster ist derzeit keines sichtbar — Partition wird gerade neu gebaut.\n",
	emptyNoClusters: "Rebuild gelaufen (%s), keine Cluster in diesem Scope.\n",

	thousandsSep: ".",
	decimalSep:   ",",
	pctSuffix:    " %",
}

// phrasesEN is the table every non-German tag renders. It is the SECOND and last
// table by decision: a third one is a translation project, while two tables are
// the difference between a mixed-language artefact and a consistent one.
//
// Two length constraints are load-bearing rather than stylistic, and both are
// pinned by tests: superCut must fit superCutReserve including a five-digit
// count, or renderSuper omits groups without saying so; the footer must fit
// FooterReserveBytes, or Render refuses the map outright.
var phrasesEN = phrases{
	capFrozen:        "!! Partition frozen: rebuild since %s %s\n",
	capSinceUnknown:  "unknown",
	capCandidatesCap: "!! (%s candidates in this scope; the global node cap was exceeded).\n",
	capCandidates:    "!! (%s candidates in this scope).\n",
	capStateOld:      "!! Cluster state %s is OLD.\n",
	capNeverBuilt:    "!! This partition has never been built successfully.\n",

	freeze: map[string]string{
		"node-cap":         "skipped per node-cap",
		"timeout":          "aborted in rebuild_timeout",
		"error":            "aborted with an error",
		"disabled":         "disabled (graph_overview.enabled=false)",
		"registry-unwired": "never ran: type registry not wired",
		"empty-node-cut":   "aborted: the node cut was empty (type policy) — the partition keeps its topics",
	},
	freezeUnknown: "skipped (%s)",

	headLine:          "ctx Root Map v%d | scope:%s | %s/%s topics | %s/%s blocks listed",
	clusterState:      "cluster state ",
	clusterStateNever: "cluster state: never built successfully",
	lastAttempt:       "last attempt ",
	tenantOne:         "tenant",
	tenantMany:        "tenants",
	cadence:           "expected cadence ~%s (%d %s)",
	labelsPrefix:      "labels: ",
	unstableIdentity:  "topics without a stable id",

	coverageNoTotal:   "Coverage: %s blocks clustered; corpus size not determined this round.\n",
	coverageKnowledge: "Coverage: %s of %s knowledge blocks clustered (%s).\n",
	coverageRaw:       "Coverage: %s of %s active blocks clustered (%s; raw count incl. operational types).\n",
	operationalPrefix: "Operational, deliberately outside the topic map: ",
	operationalBlocks: " — %s blocks",
	operationalPolicy: " (type policy, not a gap",
	operationalClose:  ").\n",
	corpusRaw:         "Raw corpus count: %s active blocks in the read window.\n",
	excludedPrefix:    "Outside the cluster cut per type policy: ",
	afterClusterState: "Anything newer than the cluster state is NOT contained here.\n",
	scopeOne:          "scope",
	scopeMany:         "scopes",
	staleScopes:       "Note: %d %s without live cluster rows but with an old success stamp (%s) — neither coverage nor gap.\n",

	topicsHeading: "\n## Topics\n",
	superSkipped: "\nMeta level: skipped — the supergraph is above root_map.super_max_nodes;\n" +
		"the map stays flat (the main rebuild is unaffected).\n",
	superHeading:  "\n## Topic Groups (meta level, γ=%s)\n",
	superRow:      "%s %s %s blocks · %s topics\n",
	superCut:      "%s more groups (line budget).\n",
	footerHeading: "\n## Not listed individually\n",
	footerSmall:   "%s clusters with ≤%d blocks (%s blocks) — link-poor, no topic of their own.\n",
	footerCapped:  "%s further clusters (%s blocks) cut by the line budget.\n",
	noTitle:       "(untitled)",

	emptyStale:      "No clusters in this read window — the meta rows present are old success stamps without a partition.\n",
	emptyNeverBuilt: "No clusters built yet — this map is empty, not incomplete.\n",
	emptyRebuilding: "Rebuild ran (%s) and reports %s topics, none of them visible in the read window right now — the partition is being rebuilt.\n",
	emptyNoClusters: "Rebuild ran (%s), no clusters in this scope.\n",

	thousandsSep: ",",
	decimalSep:   ".",
	pctSuffix:    "%",
}

// mapLanguage reduces a dream.language value to its PRIMARY SUBTAG: trim +
// lower, then everything before the first "-" (so "de-CH" is German). A regional
// variant must never fall out of its language's branch.
//
// A LOCAL copy of dream.reportLanguage / topiclabel.promptLanguage on purpose,
// twice over: depguard bars this package from internal/config, and
// topiclabel/prompt.go documents the non-sharing of the language SURFACE as a
// decision — the key is shared (E3-01, one knob per corpus), the code is not.
//
// The normalization is not redundant with config.Validate (V14), which already
// trims and lowercases the stored value: Render is a pure function with callers
// that bypass the config path entirely (tests, a handler passing an Input
// through), and a total function is what keeps them honest.
func mapLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexByte(lang, '-'); i >= 0 {
		lang = lang[:i]
	}
	return lang
}

// phrasesFor picks the table. Exactly two branches exist: the empty tag and
// German keep the frozen legacy scaffolding, every other primary subtag renders
// English. An unknown tag (fr, tr, ja) therefore gets a CONSISTENT English map
// rather than German scaffolding around English labels — which is the defect,
// not the fallback.
func phrasesFor(lang string) phrases {
	switch mapLanguage(lang) {
	case "", "de":
		return phrasesDE
	default:
		return phrasesEN
	}
}

func isFreeze(reason string) bool {
	return reason != "" && reason != "advisory-lock"
}

// renderPrefix is cap block + head + coverage section + the W-F meta section —
// everything above the topic lines. One function so the measuring loop and the
// final render always agree on what the prefix is.
//
// super arrives ALREADY RENDERED (see Render): it has its own measuring loop and
// must be byte-identical across the two prefix passes, which it cannot be if it
// is measured against two different head lengths.
func renderPrefix(in Input, cov Coverage, super string, p phrases) string {
	var b strings.Builder
	writeCapBlock(&b, in, p)
	writeHead(&b, in, cov, p)
	writeCoverage(&b, in, cov, p)
	b.WriteString(super)
	b.WriteString(p.topicsHeading)
	return b.String()
}

// writeCapBlock puts the cap ABOVE everything else (rule R2): a truncated
// context window must still contain the warning. advisory-lock never gets one.
func writeCapBlock(b *strings.Builder, in Input, p phrases) {
	f := in.Freshness
	if !isFreeze(f.SkipReason) {
		return
	}
	clause, ok := p.freeze[f.SkipReason]
	if !ok {
		clause = fmt.Sprintf(p.freezeUnknown, f.SkipReason)
	}
	since := p.capSinceUnknown
	if f.LastAttemptAt != nil {
		since = f.LastAttemptAt.UTC().Format(tsLayout)
	}
	fmt.Fprintf(b, p.capFrozen, since, clause)
	if f.SkipReason == "node-cap" {
		fmt.Fprintf(b, p.capCandidatesCap, num(f.CandidateN, p))
	} else {
		fmt.Fprintf(b, p.capCandidates, num(f.CandidateN, p))
	}
	if f.ComputedAt != nil {
		fmt.Fprintf(b, p.capStateOld, f.ComputedAt.UTC().Format(tsLayout))
	} else {
		b.WriteString(p.capNeverBuilt)
	}
}

// writeHead renders the two head lines. Neither carries a wall clock (rule R3):
// only computed_at and last_attempt_at appear, both of which describe the
// partition instead of the moment of rendering. That is the precondition for
// content idempotency — a "generated_at" or an age in hours would make every
// cycle a write.
func writeHead(b *strings.Builder, in Input, c Coverage, p phrases) {
	fmt.Fprintf(b, p.headLine,
		in.FormatVersion, in.Scope,
		num(c.RenderedRows, p), num(c.ClusterTotal, p),
		num(c.RenderedBlocks, p), num(c.ClusteredBlocks, p))
	if in.Freshness.Modularity != 0 || in.Freshness.Resolution != 0 {
		fmt.Fprintf(b, " | Q=%.3f γ=%s", in.Freshness.Modularity, gamma(in.Freshness.Resolution))
	}
	b.WriteString("\n")
	b.WriteString(strings.Join(headParts(in, p), " · "))
	b.WriteString("\n\n")
}

func headParts(in Input, p phrases) []string {
	f := in.Freshness
	parts := make([]string, 0, 5)
	if f.ComputedAt != nil {
		parts = append(parts, p.clusterState+f.ComputedAt.UTC().Format(tsLayout))
	} else {
		parts = append(parts, p.clusterStateNever)
	}
	if f.LastAttemptAt != nil {
		parts = append(parts, p.lastAttempt+f.LastAttemptAt.UTC().Format(tsLayout))
	}
	// The cadence is Interval × TenantCount, never the raw config value: the
	// rebuild loop serves ONE tenant per tick (round robin), so at 20 tenants a
	// printed "6h" would promise freshness over a partition that is five days
	// old on average. No tenant count, no cadence claim.
	if f.Interval > 0 && f.TenantCount > 0 {
		unit := p.tenantMany
		if f.TenantCount == 1 {
			unit = p.tenantOne
		}
		parts = append(parts, fmt.Sprintf(p.cadence,
			cadence(f.Interval*time.Duration(f.TenantCount)), f.TenantCount, unit))
	}
	if src := labelSources(in); src != "" {
		parts = append(parts, p.labelsPrefix+src)
	}
	if hasUnstableIdentity(in) {
		parts = append(parts, p.unstableIdentity)
	}
	return parts
}

// labelSources reports which label provenances the topic lines carry (D0/D1/D2).
// The map states HOW it was labelled, always — a heuristic label that passes as
// an LLM topic is the drift this line exists to prevent.
//
// The vocabulary itself (llm, heuristic, repr_title) is NOT translated: it is
// the DB provenance vocabulary, an operator grep handle, and a localized copy
// would break the one line that has to be comparable across corpora.
//
// Computed over all CANDIDATE rows, not only the ones that survive the budget:
// that can over-declare (a heuristic row cut by the budget still names
// "heuristic") but never under-declare, and it keeps the head length independent
// of the measuring loop it is measured for.
func labelSources(in Input) string {
	seen := map[string]bool{}
	order := []string{"llm", "heuristic", "repr_title"}
	for _, r := range in.Rows {
		if r.Size <= in.SmallClusterMax {
			continue
		}
		switch {
		case r.Label == "":
			seen["repr_title"] = true
		case r.LabelSource == "":
			seen["repr_title"] = true
		default:
			seen[r.LabelSource] = true
			if !contains(order, r.LabelSource) {
				order = append(order, r.LabelSource)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, k := range order {
		if seen[k] {
			out = append(out, k)
		}
	}
	return strings.Join(out, ", ")
}

// hasUnstableIdentity flags D3 over the candidate rows, same over-declaring
// direction as labelSources.
func hasUnstableIdentity(in Input) bool {
	for _, r := range in.Rows {
		if r.Size > in.SmallClusterMax && r.StableID == "" {
			return true
		}
	}
	return false
}

// writeCoverage is the honesty section (§4.4a–c) under the E11-02 decision.
//
// The coverage figure is measured against the KNOWLEDGE corpus, not the raw
// one, and the operational types get their own line saying they are outside the
// map by decision. The raw corpus number stays visible right next to it so
// nothing is hidden — the earlier "1.190 of 7.096 (16.8 %)" phrasing was true
// and still misleading, because 83 % of that gap is machine-written compaction
// debris that has no topic to belong to.
func writeCoverage(b *strings.Builder, in Input, c Coverage, p phrases) {
	switch {
	case !c.ActiveKnown:
		// No denominator beats a wrong one: pg_class.reltuples can filter
		// neither scope nor is_archived, and an estimate in a persisted block
		// would freeze the GLOBAL corpus size into one tenant's data.
		fmt.Fprintf(b, p.coverageNoTotal, num(c.ClusteredBlocks, p))
	case c.OperationalKnown && c.ActiveBlocks-c.OperationalBlocks > 0:
		known := c.ActiveBlocks - c.OperationalBlocks
		fmt.Fprintf(b, p.coverageKnowledge,
			num(c.ClusteredBlocks, p), num(known, p), pct(c.ClusteredBlocks, known, p))
	default:
		fmt.Fprintf(b, p.coverageRaw,
			num(c.ClusteredBlocks, p), num(c.ActiveBlocks, p), pct(c.ClusteredBlocks, c.ActiveBlocks, p))
	}

	if len(c.OperationalTypes) > 0 {
		b.WriteString(p.operationalPrefix + strings.Join(c.OperationalTypes, ", "))
		if c.OperationalKnown {
			fmt.Fprintf(b, p.operationalBlocks, num(c.OperationalBlocks, p))
		}
		b.WriteString(p.operationalPolicy)
		if in.OperationalRationale != "" {
			b.WriteString(": " + in.OperationalRationale)
		}
		b.WriteString(p.operationalClose)
	}
	if c.ActiveKnown {
		fmt.Fprintf(b, p.corpusRaw, num(c.ActiveBlocks, p))
	}
	if len(c.ExcludedTypes) > 0 {
		b.WriteString(p.excludedPrefix + strings.Join(c.ExcludedTypes, ", ") + ".\n")
	}
	if s := staleNote(in.Freshness.StaleScopes, p); s != "" {
		b.WriteString(s)
	}
	b.WriteString(p.afterClusterState)
	// The topic heading moved to renderPrefix in W-F — the meta section belongs
	// BETWEEN the coverage block and the topic list, and with an empty section
	// the byte sequence is unchanged.
}

// superLine renders one meta-cluster row: group id, the lead topic's name, and
// the two numbers that make the group worth a line at all.
func superLine(r SuperRow, p phrases) string {
	name := truncateRunes(r.Label, labelMaxRunes)
	if name == "" {
		name = truncateRunes(r.Title, labelMaxRunes)
	}
	if name == "" {
		name = p.noTitle
	}
	return fmt.Sprintf(p.superRow, r.ID, name, num(r.Size, p), num(r.TopicN, p))
}

// renderSuper is the W-F section (design/02 §4.7 step 5) with its own measuring
// loop. It returns the empty string when there is nothing to say — which is the
// shipped default, and the reason this wave changes not one byte of a live map.
//
// Three states, three outputs, and telling them apart is the point:
//
//   - not attempted (root_map.super_enabled off) ⇒ nothing at all
//   - attempted and CAPPED (supergraph above root_map.super_max_nodes) ⇒ one
//     line naming the cap. A cap that renders as absence is a cap nobody sees,
//     and the whole liveness line of this axis exists to end that
//   - built ⇒ the section, cut at half the remaining budget with an honest
//     "N more" line whenever the cut bites
func renderSuper(in Input, headLen int, p phrases) (string, int) {
	f := in.Freshness
	if !f.SuperKnown {
		return "", 0
	}
	if f.SuperN == 0 {
		return p.superSkipped, 0
	}
	if len(in.SuperRows) == 0 {
		return "", 0
	}
	// Half of what the topic lines would otherwise have. The coarse level may
	// summarise the map, never replace it.
	room := (in.BudgetBytes - headLen - in.FooterReserveBytes) / 2
	if room <= 0 {
		return "", 0
	}

	head := fmt.Sprintf(p.superHeading, gamma(f.SuperResolution))
	var b strings.Builder
	b.WriteString(head)
	used := len(head)
	shown := 0
	for _, r := range in.SuperRows {
		line := superLine(r, p)
		// The cut line has to fit too, or the section would lie by omission at
		// exactly the moment it starts omitting.
		if used+len(line)+superCutReserve > room {
			break
		}
		b.WriteString(line)
		used += len(line)
		shown++
	}
	if shown == 0 {
		return "", 0 // no room for a single group: silence beats a bare heading
	}
	if rest := f.SuperN - shown; rest > 0 {
		fmt.Fprintf(&b, p.superCut, num(rest, p))
	}
	return b.String(), shown
}

// superCutReserve is the space the section keeps free for its own "N more
// groups" line — the same reflex as the footer reserve one level up: a section
// that cannot afford to say it was cut must not cut.
//
// It is a CONSTANT across languages, so every phrases table owes it the same
// promise: the rendered cut line, including a five-digit count, must fit into
// these bytes. TestSuperCutFitsReserve pins that for both tables — an overlong
// translation would not fail loudly, it would silently drop groups.
const superCutReserve = 48

// staleNote names meta rows that outlived their partition — neither coverage
// nor gap. Without this line such a scope is invisible: its numbers are (
// correctly) excluded from every sum, and a reader comparing the map against
// `ctx stats` would find a difference with no explanation.
func staleNote(stale []string, p phrases) string {
	if len(stale) == 0 {
		return ""
	}
	word := p.scopeMany
	if len(stale) == 1 {
		word = p.scopeOne
	}
	return fmt.Sprintf(p.staleScopes, len(stale), word, strings.Join(stale, ", "))
}

// renderRows is the measuring loop (§4.5 steps 2–3): append while the footer
// reserve still fits. It returns the body plus what it managed to show, so the
// caller can compute the cut bucket from the totals instead of guessing it.
//
// Clusters at or below SmallClusterMax never become topic lines — they ARE the
// collector line. That is what makes "rendered beats small beats cut" hold by
// construction instead of by convention.
func renderRows(in Input, headLen int, p phrases) (body string, rows, blocks int) {
	var b strings.Builder
	used := headLen
	for _, r := range in.Rows {
		if r.Size <= in.SmallClusterMax {
			continue
		}
		line := renderRow(in, r, p)
		if used+len(line)+in.FooterReserveBytes > in.BudgetBytes {
			break
		}
		b.WriteString(line)
		used += len(line)
		rows++
		blocks += r.Size
	}
	return b.String(), rows, blocks
}

// renderRow writes one topic line. Fixed single-space separation, never column
// alignment: alignment depends on the widest record in the set and cannot be
// pinned by a byte-exact golden test.
//
// Both identifiers are FULL (36 chars each). The old map's 8-char prefix could
// not be resolved — an agent cannot build a `ctx get` from it — and that is the
// difference between a map and a list.
func renderRow(in Input, r Row, p phrases) string {
	var b strings.Builder
	if r.StableID != "" {
		b.WriteString(r.StableID)
		b.WriteString(" ")
	}
	b.WriteString(label(r, p))
	b.WriteString(" ")
	b.WriteString(num(r.Size, p))
	if cats := categories(r, p); cats != "" {
		b.WriteString(" ")
		b.WriteString(cats)
	}
	if r.ReprID != "" {
		b.WriteString(" repr=")
		b.WriteString(r.ReprID)
	}
	if mix := scopeMix(in.Scope, r.ScopeMix); mix != "" {
		b.WriteString(" scopes=")
		b.WriteString(mix)
	}
	b.WriteString("\n")
	return b.String()
}

// label is the D0→D2 ladder: the axis-01 label, else the representative's
// title, else an explicit placeholder — the line always has a name.
func label(r Row, p phrases) string {
	if s := truncateRunes(r.Label, labelMaxRunes); s != "" {
		return s
	}
	if s := truncateRunes(r.ReprTitle, labelMaxRunes); s != "" {
		return s
	}
	return p.noTitle
}

func categories(r Row, p phrases) string {
	if len(r.TopCats) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.TopCats))
	for i, c := range r.TopCats {
		if i < len(r.CatCounts) {
			parts = append(parts, c+" "+num(r.CatCounts[i], p))
			continue
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " · ")
}

// scopeMix follows the old map's frugality rule: annotate the scope only when
// it differs from the map's own.
func scopeMix(own string, mix []string) string {
	if len(mix) == 0 || (len(mix) == 1 && mix[0] == own) {
		return ""
	}
	return strings.Join(mix, ",")
}

// renderFooter prints BOTH non-rendered buckets, always, even at zero: the
// accounting is the point, and a bucket that disappears when empty cannot be
// checked against the head line.
func renderFooter(in Input, c Coverage, p phrases) string {
	var b strings.Builder
	b.WriteString(p.footerHeading)
	fmt.Fprintf(&b, p.footerSmall,
		num(c.SmallClusterN, p), in.SmallClusterMax, num(c.SmallClusterSize, p))
	fmt.Fprintf(&b, p.footerCapped,
		num(c.CappedClusterN, p), num(c.CappedBlocks, p))
	return b.String()
}

// writeEmptyStatement is D4 and its neighbours. The three cases are told apart
// in words because they need different reactions: wait, investigate, or ignore.
func emptyStatement(in Input, p phrases) string {
	var sb strings.Builder
	b := &sb
	f := in.Freshness
	switch {
	case len(f.StaleScopes) > 0 && f.ClusterN == 0:
		b.WriteString(p.emptyStale)
	case f.ComputedAt == nil:
		b.WriteString(p.emptyNeverBuilt)
	case f.ClusterN > 0:
		fmt.Fprintf(b, p.emptyRebuilding,
			f.ComputedAt.UTC().Format(tsLayout), num(f.ClusterN, p))
	default:
		fmt.Fprintf(b, p.emptyNoClusters, f.ComputedAt.UTC().Format(tsLayout))
	}
	return sb.String()
}

// truncateRunes cuts at a RUNE boundary, never a byte one. Byte slicing splits
// a multi-byte character and PostgreSQL rejects the result with 22021
// (invalid byte sequence) — the same trap the old map's title truncation hit.
func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max]))
}

// num formats an integer with the table's thousands separator.
func num(n int, p phrases) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteString(p.thousandsSep)
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// pct formats a share with one decimal, the table's decimal separator and its
// percent suffix. "n/a" is not prose but the absent-value marker of the metadata
// contract's twin — it stays identical in every language.
func pct(part, total int, p phrases) string {
	if total <= 0 {
		return "n/a"
	}
	v := float64(part) * 100 / float64(total)
	return strings.Replace(strconv.FormatFloat(v, 'f', 1, 64), ".", p.decimalSep, 1) + p.pctSuffix
}

// gamma prints the resolution with at least one and at most two decimals — 1.0
// stays "1.0" (not "1"), 1.25 stays "1.25".
func gamma(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimSuffix(s, "0")
}

// cadence renders a duration the way an operator reads it: minutes below an
// hour, hours below two days, days beyond.
func cadence(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Hour).Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Round(24*time.Hour).Hours()/24))
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
