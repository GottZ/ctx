package armsweep

import (
	"fmt"
	"os"
	"strings"
)

// RenderMarkdown renders the human-readable half of a report.
//
// Deterministic like the JSON body: the generation timestamp is the FIRST line
// and nothing below it depends on a clock, a map order or a filesystem listing.
// The determinism gate compares from line 2 on.
//
// No query text appears anywhere. A case is cited as slice + index + the first
// twelve hex characters of its digest, which is enough to look it up inside the
// gold directory and worth nothing outside it.
func RenderMarkdown(generatedAt string, body ReportBody) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- generated: %s -->\n", generatedAt)
	b.WriteString("# ctx-armsweep — Arm-Gewichts-Sweep\n\n")

	writeVerdict(&b, body)
	writeEnvSection(&b, body.Env)
	writeSliceSection(&b, body.Slices)
	writeNoiseSection(&b, body.Noise)
	writeConfigSection(&b, body.Configs)
	writeComparisonSection(&b, body.Comparisons)
	writeWinSection(&b, body.Wins)
	writeExcludedSection(&b, body.Excluded)
	writeNotesSection(&b, body.Notes)
	writeScopeSection(&b)
	return b.String()
}

func writeVerdict(b *strings.Builder, body ReportBody) {
	b.WriteString("## Urteil\n\n")
	if body.Interpretable {
		b.WriteString("G-NOISE bestanden — die Varianten-Tabellen sind lesbar.\n\n")
	} else {
		b.WriteString("**G-NOISE nicht bestanden oder nicht auswertbar. Keine Zahl unter „Varianten\" ist ein Ergebnis.**\n\n")
	}
	confirmed, candidates := 0, 0
	for _, w := range body.Wins {
		switch {
		case w.Confirmed:
			confirmed++
		case w.Candidate:
			candidates++
		}
	}
	fmt.Fprintf(b, "Bestätigte Gewinner: %d · Kandidaten (unbestätigt): %d\n\n", confirmed, candidates)
}

func writeEnvSection(b *strings.Builder, env EnvStamp) {
	b.WriteString("## Provenienz\n\n")
	fmt.Fprintf(b, "- Werkzeug: `%s`, git `%s`, Seed `%d`\n", env.Tool, orDash(env.GitRevision), env.Seed)
	fmt.Fprintf(b, "- Gold-Set: sha256 `%s` (Stempel `%s`), Sample-Seed `%d`, Split-Seed `%d`\n",
		ShortSHA(env.GoldSHA256), ShortSHA(env.GoldStampSHA256), env.SampleSeed, env.SplitSeed)
	fmt.Fprintf(b, "- Korpus-Stand bei Ziehung: `%s`\n", orDash(env.CorpusMaxCreatedAt))
	fmt.Fprintf(b, "- Schema-Generation der Instanz: Migration `%d`\n", env.MigrationsMax)
	if env.Generator != nil {
		fmt.Fprintf(b, "- G-Q-Generator: `%s` auf `%s` (%s, %s)\n",
			env.Generator.Model, env.Generator.Endpoint, env.Generator.Backend, env.Generator.Locality)
	}
	b.WriteString("- Post-Fusion-Stufen zur Messzeit: ")
	for i, k := range PostStageKeys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "`%s`=%v", k, env.PostFusionStages[k])
	}
	b.WriteString("\n")
	fmt.Fprintf(b, "- Pfad-Guard-Override `--allow-outside-goldset`: %s\n", yesNo(env.AllowOutsideGoldset))
	for _, d := range env.Dumps {
		fmt.Fprintf(b, "- Dump %s: `%s` (Lauf `%s`, %d Fälle, Pins `%s`/`%s`, p50 %d ms, p95 %d ms)%s\n",
			d.Role, d.File, d.RunID, d.Records, d.PinFile, ShortSHA(d.PinSHA256),
			d.Latency.P50, d.Latency.P95, abortSuffix(d.Drift))
		for _, r := range d.Drift.Reasons {
			fmt.Fprintf(b, "  - ABBRUCH: %s\n", r)
		}
		for _, n := range d.Drift.Notes {
			fmt.Fprintf(b, "  - Drift: %s\n", n)
		}
	}
	b.WriteString("\n")
}

func abortSuffix(v DriftVerdict) string {
	if v.Abort {
		return " — **VERWORFEN (Drift-Protokoll)**"
	}
	return ""
}

func writeSliceSection(b *strings.Builder, slices []SliceProfile) {
	b.WriteString("## Slices\n\n| Slice | n | gelabelt | temporal | Rollout-Kriterium | Hinweis |\n|---|---:|---:|---:|---|---|\n")
	for _, s := range slices {
		fmt.Fprintf(b, "| %s | %d | %d | %.1f %% | %s | %s |\n",
			s.Slice, s.N, s.Labelled, 100*s.TemporalShare, rolloutMark(s.RolloutCriterion), orDash(s.Note))
	}
	b.WriteString("\n")
}

// rolloutMark spells the floor-check role out in the table rather than leaving
// it to whoever remembers which slice name is a floor.
func rolloutMark(ok bool) string {
	if ok {
		return "ja"
	}
	return "**nein (Boden-Check)**"
}

func writeNoiseSection(b *strings.Builder, gates []NoiseGate) {
	b.WriteString("## G-NOISE (V0 gegen V0′)\n\n")
	if len(gates) == 0 {
		b.WriteString("Nicht ausgewertet — kein zweiter Dump.\n\n")
		return
	}
	b.WriteString("| Slice | n | Diskordanz Recall@5 | Schwelle | CI ΔnDCG@10 | Urteil |\n|---|---:|---:|---:|---|---|\n")
	for _, g := range gates {
		fmt.Fprintf(b, "| %s | %d | %.4f | %.2f | [%.5f, %.5f] | %s |\n",
			g.Slice, g.N, g.Discordance, g.Threshold, g.CILo, g.CIHi, passFail(g.Pass))
	}
	b.WriteString("\n")
	for _, g := range gates {
		for _, r := range g.Reasons {
			fmt.Fprintf(b, "- %s: %s\n", g.Slice, r)
		}
	}
	b.WriteString("\n")
}

func writeConfigSection(b *strings.Builder, cfgs []ConfigResult) {
	b.WriteString("## Konfigurationen\n\n| Konfiguration | Dump | Gewichte (sem/de/en/tri) | k | Slice | n | nDCG@10 | Recall@5 | MRR@10 |\n")
	b.WriteString("|---|---|---|---:|---|---:|---:|---:|---:|\n")
	for _, c := range cfgs {
		w := c.Config.Weights
		weights := fmt.Sprintf("%.4f/%.4f/%.4f/%.4f", w.Semantic, w.FTSDe, w.FTSEn, w.Trigram)
		for _, s := range c.Slices {
			if s.Unlabelled {
				fmt.Fprintf(b, "| %s | %s | %s | %g | %s | %d | — | — | — |\n",
					c.Config.Name, c.Dump, weights, c.Config.K, s.Slice, s.N)
				continue
			}
			fmt.Fprintf(b, "| %s | %s | %s | %g | %s | %d | %.4f | %.4f | %.4f |\n",
				c.Config.Name, c.Dump, weights, c.Config.K, s.Slice, s.N, s.NDCG10, s.Recall5, s.MRR10)
		}
	}
	b.WriteString("\n")
	for _, c := range cfgs {
		if c.Config.Note != "" {
			fmt.Fprintf(b, "- %s: %s\n", c.Config.Name, c.Config.Note)
		}
	}
	b.WriteString("\n")
}

func writeComparisonSection(b *strings.Builder, cmps []Comparison) {
	b.WriteString("## Varianten gegen V0\n\n| Konfiguration | Slice | n | Niveau | ΔnDCG@10 | CI | McNemar b/c | p | Diskordanz |\n")
	b.WriteString("|---|---|---:|---:|---:|---|---|---:|---:|\n")
	for _, c := range cmps {
		fmt.Fprintf(b, "| %s | %s | %d | %.4f | %+.5f | [%+.5f, %+.5f] | %d/%d | %.4f | %.4f |\n",
			c.Config, c.Slice, c.N, c.Level, c.DeltaNDCG, c.CILo, c.CIHi,
			c.McNemar.B, c.McNemar.C, c.McNemar.P, c.Discordance)
	}
	b.WriteString("\n")
}

func writeWinSection(b *strings.Builder, wins []WinGate) {
	b.WriteString("## G-WIN (nur " + SliceQHold + ")\n\n| Konfiguration | Niveau | Urteil |\n|---|---:|---|\n")
	for _, w := range wins {
		fmt.Fprintf(b, "| %s | %.4f | %s |\n", w.Config, w.Level, w.Label)
	}
	b.WriteString("\n")
	for _, w := range wins {
		for _, r := range w.Reasons {
			fmt.Fprintf(b, "- %s: %s\n", w.Config, r)
		}
	}
	b.WriteString("\n")
}

func writeExcludedSection(b *strings.Builder, ex []ExcludedCase) {
	b.WriteString("## Ausgeschlossene Fälle\n\n")
	if len(ex) == 0 {
		b.WriteString("Keine.\n\n")
		return
	}
	b.WriteString("Vereinigung über das Dump-Paar; ausgeschlossen, nicht ersetzt.\n\n")
	b.WriteString("| Slice | Index | sha256 | Versuche | Grund |\n|---|---:|---|---:|---|\n")
	for _, e := range ex {
		fmt.Fprintf(b, "| %s | %d | %s | %d | %s |\n", e.Slice, e.Index, ShortSHA(e.QuerySHA256), e.Attempts, e.Reason)
	}
	b.WriteString("\n")
}

func writeNotesSection(b *strings.Builder, notes []string) {
	if len(notes) == 0 {
		return
	}
	b.WriteString("## Anmerkungen\n\n")
	for _, n := range notes {
		fmt.Fprintf(b, "- %s\n", n)
	}
	b.WriteString("\n")
}

// writeScopeSection states what the instrument does NOT measure, in the report
// itself rather than only in the README — a table of numbers travels further
// than the documentation next to it.
func writeScopeSection(b *strings.Builder) {
	b.WriteString("## Was hier NICHT gemessen wird\n\n")
	b.WriteString("Alles nach `ctx_rrf`. Gravity, Cluster-Injektion, Graph-Expansion, der " +
		"Aggregat-Fold und die Rerank-Stufe laufen auf dem Live-Pfad, stehen als " +
		"ausgelieferte Reihenfolge im Dump und als Stufen-Zustand im Env-Stempel — " +
		"nachgerechnet wird keine davon. Ein Gewicht, das hier gewinnt, gewinnt an " +
		"der Fusionsstufe; ob es die Post-Stufen übersteht, ist eine andere Frage " +
		"und braucht ein anderes Instrument.\n")
}

// WriteMarkdown persists the rendered report at mode 0600.
func WriteMarkdown(path, generatedAt string, body ReportBody) error {
	return os.WriteFile(path, []byte(RenderMarkdown(generatedAt, body)), fileMode)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(v bool) string {
	if v {
		return "gesetzt"
	}
	return "nicht gesetzt"
}

func passFail(v bool) string {
	if v {
		return "bestanden"
	}
	return "**durchgefallen**"
}
