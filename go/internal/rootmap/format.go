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

// freezeReasons maps a skip_reason to its plain-language cap clause. It is
// deliberately a CLOSED set: 'advisory-lock' is absent because contention is
// not a freeze (another instance is building this very partition, successfully)
// and an unknown reason renders a generic clause rather than nothing — a cap
// the map cannot name is still a cap.
var freezeReasons = map[string]string{
	"node-cap":         "per node-cap übersprungen",
	"timeout":          "im rebuild_timeout abgebrochen",
	"error":            "mit einem Fehler abgebrochen",
	"disabled":         "abgeschaltet (graph_overview.enabled=false)",
	"registry-unwired": "nicht gelaufen: Typ-Registry nicht verdrahtet",
	// Migration 126 added the sixth value after this table was first written.
	// It is the freeze that reads like a bug and is not one: the node set is the
	// INTERSECTION of the retrieval-visible types and overview.include, and an
	// empty intersection selects nothing — so the rebuild refuses rather than
	// retiring every topic the partition has.
	"empty-node-cut": "abgebrochen: der Knotenschnitt war leer (Typ-Policy) — die Partition behält ihre Themen",
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
func renderPrefix(in Input, cov Coverage, super string) string {
	var b strings.Builder
	writeCapBlock(&b, in)
	writeHead(&b, in, cov)
	writeCoverage(&b, in, cov)
	b.WriteString(super)
	b.WriteString("\n## Themen\n")
	return b.String()
}

// writeCapBlock puts the cap ABOVE everything else (rule R2): a truncated
// context window must still contain the warning. advisory-lock never gets one.
func writeCapBlock(b *strings.Builder, in Input) {
	f := in.Freshness
	if !isFreeze(f.SkipReason) {
		return
	}
	clause, ok := freezeReasons[f.SkipReason]
	if !ok {
		clause = "übersprungen (" + f.SkipReason + ")"
	}
	since := "unbekannt"
	if f.LastAttemptAt != nil {
		since = f.LastAttemptAt.UTC().Format(tsLayout)
	}
	fmt.Fprintf(b, "!! Partition eingefroren: Rebuild seit %s %s\n", since, clause)
	if f.SkipReason == "node-cap" {
		fmt.Fprintf(b, "!! (%s Kandidaten in diesem Scope; der globale Knoten-Cap wurde überschritten).\n", num(f.CandidateN))
	} else {
		fmt.Fprintf(b, "!! (%s Kandidaten in diesem Scope).\n", num(f.CandidateN))
	}
	if f.ComputedAt != nil {
		fmt.Fprintf(b, "!! Cluster-Stand %s ist ALT.\n", f.ComputedAt.UTC().Format(tsLayout))
	} else {
		b.WriteString("!! Diese Partition wurde nie erfolgreich gebaut.\n")
	}
}

// writeHead renders the two head lines. Neither carries a wall clock (rule R3):
// only computed_at and last_attempt_at appear, both of which describe the
// partition instead of the moment of rendering. That is the precondition for
// content idempotency — a "generated_at" or an age in hours would make every
// cycle a write.
func writeHead(b *strings.Builder, in Input, c Coverage) {
	fmt.Fprintf(b, "ctx Root Map v%d | scope:%s | %s/%s Themen | %s/%s Blöcke geführt",
		in.FormatVersion, in.Scope,
		num(c.RenderedRows), num(c.ClusterTotal),
		num(c.RenderedBlocks), num(c.ClusteredBlocks))
	if in.Freshness.Modularity != 0 || in.Freshness.Resolution != 0 {
		fmt.Fprintf(b, " | Q=%.3f γ=%s", in.Freshness.Modularity, gamma(in.Freshness.Resolution))
	}
	b.WriteString("\n")
	b.WriteString(strings.Join(headParts(in), " · "))
	b.WriteString("\n\n")
}

func headParts(in Input) []string {
	f := in.Freshness
	parts := make([]string, 0, 5)
	if f.ComputedAt != nil {
		parts = append(parts, "Cluster-Stand "+f.ComputedAt.UTC().Format(tsLayout))
	} else {
		parts = append(parts, "Cluster-Stand: nie erfolgreich gebaut")
	}
	if f.LastAttemptAt != nil {
		parts = append(parts, "letzter Versuch "+f.LastAttemptAt.UTC().Format(tsLayout))
	}
	// The cadence is Interval × TenantCount, never the raw config value: the
	// rebuild loop serves ONE tenant per tick (round robin), so at 20 tenants a
	// printed "6h" would promise freshness over a partition that is five days
	// old on average. No tenant count, no cadence claim.
	if f.Interval > 0 && f.TenantCount > 0 {
		unit := "Tenants"
		if f.TenantCount == 1 {
			unit = "Tenant"
		}
		parts = append(parts, fmt.Sprintf("erwartete Kadenz ~%s (%d %s)",
			cadence(f.Interval*time.Duration(f.TenantCount)), f.TenantCount, unit))
	}
	if src := labelSources(in); src != "" {
		parts = append(parts, "Labels: "+src)
	}
	if hasUnstableIdentity(in) {
		parts = append(parts, "Themen ohne stabile ID")
	}
	return parts
}

// labelSources reports which label provenances the topic lines carry (D0/D1/D2).
// The map states HOW it was labelled, always — a heuristic label that passes as
// an LLM topic is the drift this line exists to prevent.
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
func writeCoverage(b *strings.Builder, in Input, c Coverage) {
	switch {
	case !c.ActiveKnown:
		// No denominator beats a wrong one: pg_class.reltuples can filter
		// neither scope nor is_archived, and an estimate in a persisted block
		// would freeze the GLOBAL corpus size into one tenant's data.
		fmt.Fprintf(b, "Deckung: %s Blöcke geclustert; Korpusgröße in dieser Runde nicht ermittelt.\n",
			num(c.ClusteredBlocks))
	case c.OperationalKnown && c.ActiveBlocks-c.OperationalBlocks > 0:
		known := c.ActiveBlocks - c.OperationalBlocks
		fmt.Fprintf(b, "Deckung: %s von %s Wissens-Blöcken geclustert (%s).\n",
			num(c.ClusteredBlocks), num(known), pct(c.ClusteredBlocks, known))
	default:
		fmt.Fprintf(b, "Deckung: %s von %s aktiven Blöcken geclustert (%s; Rohzahl inkl. operativer Typen).\n",
			num(c.ClusteredBlocks), num(c.ActiveBlocks), pct(c.ClusteredBlocks, c.ActiveBlocks))
	}

	if len(c.OperationalTypes) > 0 {
		b.WriteString("Operativ, bewusst außerhalb der Themenkarte: " + strings.Join(c.OperationalTypes, ", "))
		if c.OperationalKnown {
			fmt.Fprintf(b, " — %s Blöcke", num(c.OperationalBlocks))
		}
		b.WriteString(" (Typ-Policy, keine Lücke")
		if in.OperationalRationale != "" {
			b.WriteString(": " + in.OperationalRationale)
		}
		b.WriteString(").\n")
	}
	if c.ActiveKnown {
		fmt.Fprintf(b, "Korpus-Rohzahl: %s aktive Blöcke im Lesefenster.\n", num(c.ActiveBlocks))
	}
	if len(c.ExcludedTypes) > 0 {
		b.WriteString("Außerhalb des Cluster-Schnitts per Typ-Policy: " + strings.Join(c.ExcludedTypes, ", ") + ".\n")
	}
	if s := staleNote(in.Freshness.StaleScopes); s != "" {
		b.WriteString(s)
	}
	b.WriteString("Alles nach dem Cluster-Stand ist hier NICHT enthalten.\n")
	// The "## Themen" heading moved to renderPrefix in W-F — the meta section
	// belongs BETWEEN the coverage block and the topic list, and with an empty
	// section the byte sequence is unchanged.
}

// superLine renders one meta-cluster row: group id, the lead topic's name, and
// the two numbers that make the group worth a line at all.
func superLine(r SuperRow) string {
	name := truncateRunes(r.Label, labelMaxRunes)
	if name == "" {
		name = truncateRunes(r.Title, labelMaxRunes)
	}
	if name == "" {
		name = "(ohne Titel)"
	}
	return fmt.Sprintf("%s %s %s Blöcke · %s Themen\n", r.ID, name, num(r.Size), num(r.TopicN))
}

// renderSuper is the W-F section (design/02 §4.7 step 5) with its own measuring
// loop. It returns "" when there is nothing to say — which is the shipped
// default, and the reason this wave changes not one byte of a live map.
//
// Three states, three outputs, and telling them apart is the point:
//
//   - not attempted (root_map.super_enabled off) ⇒ nothing at all
//   - attempted and CAPPED (supergraph above root_map.super_max_nodes) ⇒ one
//     line naming the cap. A cap that renders as absence is a cap nobody sees,
//     and the whole liveness line of this axis exists to end that
//   - built ⇒ the section, cut at half the remaining budget with an honest
//     "N more" line whenever the cut bites
func renderSuper(in Input, headLen int) (string, int) {
	f := in.Freshness
	if !f.SuperKnown {
		return "", 0
	}
	if f.SuperN == 0 {
		return "\nMeta-Ebene: übersprungen — der Supergraph liegt über root_map.super_max_nodes;\n" +
			"die Karte bleibt flach (der Haupt-Rebuild ist davon unberührt).\n", 0
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

	head := fmt.Sprintf("\n## Themen-Gruppen (Meta-Ebene, γ=%s)\n", gamma(f.SuperResolution))
	var b strings.Builder
	b.WriteString(head)
	used := len(head)
	shown := 0
	for _, r := range in.SuperRows {
		line := superLine(r)
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
		fmt.Fprintf(&b, "%s weitere Gruppen (Zeilenbudget).\n", num(rest))
	}
	return b.String(), shown
}

// superCutReserve is the space the section keeps free for its own "N weitere
// Gruppen" line — the same reflex as the footer reserve one level up: a section
// that cannot afford to say it was cut must not cut.
const superCutReserve = 48

// staleNote names meta rows that outlived their partition — neither coverage
// nor gap. Without this line such a scope is invisible: its numbers are (
// correctly) excluded from every sum, and a reader comparing the map against
// `ctx stats` would find a difference with no explanation.
func staleNote(stale []string) string {
	if len(stale) == 0 {
		return ""
	}
	word := "Scopes"
	if len(stale) == 1 {
		word = "Scope"
	}
	return fmt.Sprintf("Hinweis: %d %s ohne aktive Cluster-Zeilen mit altem Erfolgs-Stempel (%s) — weder Deckung noch Lücke.\n",
		len(stale), word, strings.Join(stale, ", "))
}

// renderRows is the measuring loop (§4.5 steps 2–3): append while the footer
// reserve still fits. It returns the body plus what it managed to show, so the
// caller can compute the cut bucket from the totals instead of guessing it.
//
// Clusters at or below SmallClusterMax never become topic lines — they ARE the
// collector line. That is what makes "rendered beats small beats cut" hold by
// construction instead of by convention.
func renderRows(in Input, headLen int) (body string, rows, blocks int) {
	var b strings.Builder
	used := headLen
	for _, r := range in.Rows {
		if r.Size <= in.SmallClusterMax {
			continue
		}
		line := renderRow(in, r)
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
func renderRow(in Input, r Row) string {
	var b strings.Builder
	if r.StableID != "" {
		b.WriteString(r.StableID)
		b.WriteString(" ")
	}
	b.WriteString(label(r))
	b.WriteString(" ")
	b.WriteString(num(r.Size))
	if cats := categories(r); cats != "" {
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
func label(r Row) string {
	if s := truncateRunes(r.Label, labelMaxRunes); s != "" {
		return s
	}
	if s := truncateRunes(r.ReprTitle, labelMaxRunes); s != "" {
		return s
	}
	return "(ohne Titel)"
}

func categories(r Row) string {
	if len(r.TopCats) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.TopCats))
	for i, c := range r.TopCats {
		if i < len(r.CatCounts) {
			parts = append(parts, c+" "+num(r.CatCounts[i]))
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
func renderFooter(in Input, c Coverage) string {
	var b strings.Builder
	b.WriteString("\n## Nicht einzeln geführt\n")
	fmt.Fprintf(&b, "%s Cluster mit ≤%d Blöcken (%s Blöcke) — link-arm, kein eigenes Thema.\n",
		num(c.SmallClusterN), in.SmallClusterMax, num(c.SmallClusterSize))
	fmt.Fprintf(&b, "%s weitere Cluster (%s Blöcke) wegen Zeilenbudget gekappt.\n",
		num(c.CappedClusterN), num(c.CappedBlocks))
	return b.String()
}

// writeEmptyStatement is D4 and its neighbours. The three cases are told apart
// in words because they need different reactions: wait, investigate, or ignore.
func emptyStatement(in Input) string {
	var sb strings.Builder
	b := &sb
	f := in.Freshness
	switch {
	case len(f.StaleScopes) > 0 && f.ClusterN == 0:
		b.WriteString("Keine Cluster in diesem Lesefenster — die vorhandenen Meta-Zeilen sind alte Erfolgs-Stempel ohne Partition.\n")
	case f.ComputedAt == nil:
		b.WriteString("Noch keine Cluster gebaut — diese Karte ist leer, nicht unvollständig.\n")
	case f.ClusterN > 0:
		fmt.Fprintf(b, "Rebuild gelaufen (%s) und meldet %s Themen, im Lesefenster ist derzeit keines sichtbar — Partition wird gerade neu gebaut.\n",
			f.ComputedAt.UTC().Format(tsLayout), num(f.ClusterN))
	default:
		fmt.Fprintf(b, "Rebuild gelaufen (%s), keine Cluster in diesem Scope.\n",
			f.ComputedAt.UTC().Format(tsLayout))
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

// num formats an integer with German thousands separators.
func num(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune('.')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// pct formats a share with one decimal and a German decimal comma.
func pct(part, total int) string {
	if total <= 0 {
		return "n/a"
	}
	v := float64(part) * 100 / float64(total)
	return strings.Replace(strconv.FormatFloat(v, 'f', 1, 64), ".", ",", 1) + " %"
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
