package armsweep

import (
	"fmt"
	"strings"
)

// RenderCompareMarkdown renders the human-readable half of a compare report.
//
// Deterministic like the JSON body: the generation timestamp is the FIRST line
// and nothing below it depends on a clock, a map order or a filesystem listing
// (gate (d)). No query text appears anywhere — a case is cited as its key, the
// same rule RenderMarkdown follows.
func RenderCompareMarkdown(generatedAt string, body CompareBody) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- generated: %s -->\n", generatedAt)
	b.WriteString("# ctx-armsweep — Bedingungs-Vergleich\n\n")

	writeCompareVerdict(&b, body)
	writeCompareCondition(&b, body.Condition)
	writeCompareEnv(&b, body.Env)
	writeCompareSlices(&b, body.Slices, body.Paired, body.UnpairedTotal, body.Unpaired)
	writeCompareNoise(&b, body.Noise)
	writeCompareMDE(&b, body.MDE)
	writeCompareEffects(&b, body.Effects)
	writeCompareDisplacement(&b, body.Displacement)
	writeNotesSection(&b, body.Notes)
	writeCompareScope(&b, body.Condition)
	return b.String()
}

// writeCompareCondition renders the declared condition directly beneath the
// verdict and above everything else — it is the interpretation of every number
// that follows, not a footnote to them (wave X-W3a). Absent without a
// declaration, and then the report is the one M-W3d wrote.
func writeCompareCondition(b *strings.Builder, d *ConditionDeclaration) {
	if d == nil {
		return
	}
	b.WriteString("## Deklarierte Bedingung\n\n")
	fmt.Fprintf(b, "**Feld `%s` — Basis der Kennzahlen: `%s`. Bedingung eingetreten: %s.**\n\n",
		d.Field, d.Basis, jaNein(d.Applies))
	b.WriteString("| Rolle | Wert des deklarierten Feldes |\n|---|---|\n")
	fmt.Fprintf(b, "| %s | `%s` |\n", RoleBase, d.BaseValue)
	fmt.Fprintf(b, "| %s | `%s` |\n", RoleCond, d.CondValue)
	fmt.Fprintf(b, "| Rausch-Paar | `%s` |\n", d.NoiseValue)
	b.WriteString("\n")
	if d.DeliveredMaxLen > 0 {
		fmt.Fprintf(b, "Längste gemessene Lieferliste: **%d** Einträge.\n\n", d.DeliveredMaxLen)
	}
	for _, n := range d.Notes {
		fmt.Fprintf(b, "- %s\n", n)
	}
	b.WriteString("\n")
}

func writeCompareVerdict(b *strings.Builder, body CompareBody) {
	b.WriteString("## Urteil\n\n")
	if body.Refused {
		b.WriteString("**Vergleich verweigert — G-NOISE nicht bestanden oder nicht auswertbar. Keine Zahl unten ist ein Ergebnis.**\n\n")
		for _, r := range body.RefusalReasons {
			fmt.Fprintf(b, "- %s\n", r)
		}
		b.WriteString("\n")
		return
	}
	readable := 0
	for _, e := range body.Effects {
		if e.Readable {
			readable++
		}
	}
	fmt.Fprintf(b, "G-NOISE bestanden. Lesbare Effekte (CI ohne 0, über MDE, vom Rauschen trennbar): %d von %d Slices.\n\n",
		readable, len(body.Effects))
}

func writeCompareEnv(b *strings.Builder, env CompareEnv) {
	b.WriteString("## Provenienz\n\n")
	fmt.Fprintf(b, "- Werkzeug: `%s`, git `%s`, Seed `%d`\n", env.Tool, orDash(env.GitRevision), env.Seed)
	fmt.Fprintf(b, "- Kampagnen-Anker (Pin-Lauf): `%s`\n", orDash(env.CampaignPinRunID))
	fmt.Fprintf(b, "- Gold-Set: sha256 `%s` (Stempel `%s`), Sample-Seed `%d`, Split-Seed `%d`\n",
		ShortSHA(env.GoldSHA256), ShortSHA(env.GoldStampSHA256), env.SampleSeed, env.SplitSeed)
	fmt.Fprintf(b, "- Schema-Generation der Instanz: Migration `%d`\n", env.MigrationsMax)
	fmt.Fprintf(b, "- Instanz-Art (`%s`): `%s`\n", SettingInstanceKind, orDash(env.InstanceKind))
	fmt.Fprintf(b, "- Schatten-Typen der Bedingung: %s\n", orDash(joinTicked(env.ShadowTypes)))
	b.WriteString("- Post-Fusion-Stufen zur Messzeit: ")
	for i, k := range PostStageKeys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "`%s`=%v", k, env.PostFusionStages[k])
	}
	b.WriteString("\n")
	for _, g := range env.GUCs {
		fmt.Fprintf(b, "- GUC `%s` = `%s` (%s)\n", g.Name, g.Value, g.Source)
	}
	fmt.Fprintf(b, "- Mess-Kopie-Override `-allow-live-instance`: %s · Pfad-Guard-Override: %s\n",
		yesNo(env.AllowLiveInstance), yesNo(env.AllowOutsideGoldset))
	for _, d := range env.Dumps {
		fmt.Fprintf(b, "- Dump %s: `%s` (Lauf `%s`, %d Fälle, Pins `%s`/`%s`)%s\n",
			d.Role, d.File, d.RunID, d.Records, d.PinFile, ShortSHA(d.PinSHA256), abortSuffix(d.Drift))
	}
	b.WriteString("\n")
}

func writeCompareSlices(b *strings.Builder, slices []SliceProfile, paired, unpaired int, names []string) {
	b.WriteString("## Fall-Zensus\n\n")
	fmt.Fprintf(b, "Gepaart über alle vier Dumps: **%d** Fälle; ungepaart: **%d**.\n\n", paired, unpaired)
	b.WriteString("| Slice | n | gelabelt | Rollout-Kriterium | temporal | Hinweis |\n|---|---:|---:|:--:|---:|---|\n")
	for _, s := range slices {
		fmt.Fprintf(b, "| %s | %d | %d | %s | %.2f | %s |\n",
			s.Slice, s.N, s.Labelled, jaNein(s.RolloutCriterion), s.TemporalShare, orDash(s.Note))
	}
	b.WriteString("\n")
	if len(names) > 0 {
		b.WriteString("Ungepaarte Fälle (gekappt): ")
		b.WriteString(joinTicked(names))
		b.WriteString("\n\n")
	}
}

func writeCompareNoise(b *strings.Builder, gates []NoiseGate) {
	b.WriteString("## Rauschboden (G-NOISE, V0 gegen V0')\n\n")
	if len(gates) == 0 {
		b.WriteString("Nicht auswertbar.\n\n")
		return
	}
	b.WriteString("| Slice | n | Diskordanz Hit@5 | Schwelle | CI ΔnDCG@10 | Urteil |\n|---|---:|---:|---:|---|:--:|\n")
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

func writeCompareMDE(b *strings.Builder, rows []MDEReport) {
	b.WriteString("## Auflösung (MDE je Slice, §4.4b)\n\n")
	if len(rows) == 0 {
		b.WriteString("Keine Slice auswertbar.\n\n")
		return
	}
	b.WriteString("| Slice | n | CI des Rauschbodens | MDE ΔnDCG@10 | Schwelle | auflösbar |\n|---|---:|---|---:|---:|:--:|\n")
	for _, m := range rows {
		fmt.Fprintf(b, "| %s | %d | [%.5f, %.5f] | %.5f | %.2f | %s |\n",
			m.Slice, m.N, m.NoiseCILo, m.NoiseCIHi, m.MDE, m.Threshold, jaNein(m.Resolvable))
	}
	b.WriteString("\n")
}

func writeCompareEffects(b *strings.Builder, effects []CompareEffect) {
	b.WriteString("## Effekte (Bedingung gegen Basis)\n\n")
	if len(effects) == 0 {
		b.WriteString("Keine gepaarte Slice.\n\n")
		return
	}
	b.WriteString("| Slice | n | ΔnDCG@10 | CI | ΔRecall@5 | ΔMRR@10 | Diskordanz | Rausch-Diskordanz | trennbar | > MDE | lesbar |\n")
	b.WriteString("|---|---:|---:|---|---:|---:|---:|---:|:--:|:--:|:--:|\n")
	for _, e := range effects {
		if e.Unlabelled {
			fmt.Fprintf(b, "| %s | %d | — | — | — | — | — | — | — | — | ohne Labels |\n", e.Slice, e.N)
			continue
		}
		fmt.Fprintf(b, "| %s | %d | %+.5f | [%+.5f, %+.5f] | %+.5f | %+.5f | %.4f | %.4f | %s | %s | %s |\n",
			e.Slice, e.N, e.DeltaNDCG10, e.NDCGCILo, e.NDCGCIHi, e.DeltaRecall5, e.DeltaMRR10,
			e.Discordance, e.NoiseDiscordance, jaNein(e.Separable), jaNein(e.AboveMDE), jaNein(e.Readable))
	}
	b.WriteString("\n")
	for _, e := range effects {
		for _, r := range e.Reasons {
			fmt.Fprintf(b, "- %s: %s\n", e.Slice, r)
		}
	}
	b.WriteString("\n")
}

func writeCompareDisplacement(b *strings.Builder, rows []DisplacementRow) {
	b.WriteString("## Verdrängung im Top-5-Fenster\n\n")
	if len(rows) == 0 {
		b.WriteString("Keine gepaarte Slice.\n\n")
		return
	}
	b.WriteString("| Slice | Fälle | Schatten in Top-5 | davon Rang 1 | verdrängt | je Fall min/max | verdrängt & gelabelt | verdrängte Typen | eingetretene Typen |\n")
	b.WriteString("|---|---:|---:|---:|---:|---|---:|---|---|\n")
	for _, r := range rows {
		labelled := fmt.Sprintf("%d", r.DisplacedLabelled)
		if !r.LabelsAvailable {
			labelled = "—"
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d/%d | %s | %s | %s |\n",
			r.Slice, r.Cases, r.ShadowInTopK, r.ShadowAtRank1, r.Displaced,
			r.MinPerCase, r.MaxPerCase, labelled, typeHisto(r.DisplacedByType), typeHisto(r.EntrantsByType))
	}
	b.WriteString("\n")
	for _, r := range rows {
		if r.Note != "" {
			fmt.Fprintf(b, "- %s: %s\n", r.Slice, r.Note)
		}
	}
	b.WriteString("\n")
}

// jaNein renders a table cell. yesNo says "gesetzt"/"nicht gesetzt", which
// reads right for an override flag and wrong for a verdict column.
func jaNein(v bool) string {
	if v {
		return "ja"
	}
	return "nein"
}

func typeHisto(counts []TypeCount) string {
	if len(counts) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("`%s`×%d", c.TypeName, c.N))
	}
	return strings.Join(parts, ", ")
}

func writeCompareScope(b *strings.Builder, d *ConditionDeclaration) {
	b.WriteString("## Geltungsbereich\n\n")
	if d != nil && d.Basis == RankingBasisDelivered {
		b.WriteString("- Gewertet ist die AUSGELIEFERTE Reihenfolge, nicht die Offline-Fusion — so verlangt es die deklarierte Bedingung (siehe oben).\n")
	} else {
		b.WriteString("- Gewertet ist die OFFLINE-Fusion unter den Live-Gewichten, nicht die ausgelieferte Reihenfolge.\n")
	}
	b.WriteString("- Der Rauschboden stammt aus dem V0/V0'-Paar DERSELBEN Kampagne; ohne ihn verweigert dieses Unterkommando (§4.3).\n")
	b.WriteString("- Ein Effekt gilt nur als lesbar, wenn sein CI die 0 ausschließt, er über der MDE seiner Slice liegt und seine Diskordanz die des Rauschbodens übersteigt (F-32).\n")
	b.WriteString("- Boden-Slices sind ausgewiesen und tragen nie ein Rollout-Urteil.\n")
}
