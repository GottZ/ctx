package goldbench

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// WriteJSON schreibt den Report als eingerücktes JSON nach path.
func WriteJSON(report *Report, path string) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("goldbench: marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("goldbench: write report: %w", err)
	}
	return nil
}

// WriteMarkdown schreibt den Report als kompakte Markdown-Tabelle nach path.
func WriteMarkdown(report *Report, path string) error {
	if err := os.WriteFile(path, []byte(Markdown(report)), 0o600); err != nil {
		return fmt.Errorf("goldbench: write markdown: %w", err)
	}
	return nil
}

// Markdown rendert den Report als Markdown-Text.
func Markdown(report *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ctx-goldbench — %s\n\n", report.Env.Model)
	fmt.Fprintf(&b, "- Endpoint: `%s`\n- Dataset: `%s`\n- Git: `%s`\n- Zeit: %s\n- Seed: %d\n",
		report.Env.Endpoint, report.Env.DatasetSHA256, report.Env.GitRev,
		report.Env.Timestamp, report.Env.Seed)
	if report.Env.DryRun {
		b.WriteString("- **DRY-RUN** (keine LLM-Calls)\n")
	}
	if report.Env.MaxTokensMult > 1 {
		fmt.Fprintf(&b, "- **Budget ×%.3g** — max_tokens der Achsen skaliert (Abweichung von der Pipeline-Treue; nur mit gleich skalierten Läufen vergleichbar)\n", report.Env.MaxTokensMult)
	}
	b.WriteString("\n| Achse | n | parse_rate | Primär-Metrik | Score | CI95 | Qualität |\n")
	b.WriteString("|---|---:|---:|---|---:|---:|---|\n")

	axes := make([]string, 0, len(report.Axes))
	for a := range report.Axes {
		axes = append(axes, a)
	}
	sort.Strings(axes)
	for _, a := range axes {
		res := report.Axes[a]
		name := a
		if res.Prospective {
			name += " *(prospective)*"
		}
		fmt.Fprintf(&b, "| %s | %d | %.3f | %s | %.4f | [%.3f–%.3f] | %s |\n",
			name, res.N, res.ParseRate, res.PrimaryMetric, res.PrimaryScore, res.CI95Low, res.CI95High, res.LabelQuality)
	}

	if report.Throughput.WallSeconds > 0 {
		fmt.Fprintf(&b, "\nDurchsatz: in %.0f tok/s · out %.0f tok/s · Wall %.0fs · Tokens %d/%d\n",
			report.Throughput.PromptTokPerSec, report.Throughput.CompletionTokPerSec,
			report.Throughput.WallSeconds, report.Throughput.PromptTokens, report.Throughput.CompletionTokens)
	}
	fmt.Fprintf(&b, "\nRisse: context %d · truncated %d · transport %d\n",
		report.FailStats.ContextErrors, report.FailStats.TruncatedOutputs, report.FailStats.TransportErrors)
	fmt.Fprintf(&b, "\n**Composite: %.4f**", report.Composite)
	if report.CompositeGold != nil {
		fmt.Fprintf(&b, " · gold-Achsen: %.4f", *report.CompositeGold)
	}
	if report.CompositeSilver != nil {
		fmt.Fprintf(&b, " · silver-Achsen: %.4f", *report.CompositeSilver)
	}
	b.WriteString("\n\n## Sekundär-Metriken\n\n")
	for _, a := range axes {
		res := report.Axes[a]
		if len(res.Secondary) == 0 {
			continue
		}
		keys := make([]string, 0, len(res.Secondary))
		for k := range res.Secondary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "- **%s**:", a)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%.4f", k, res.Secondary[k])
		}
		if res.TransportErrors > 0 {
			fmt.Fprintf(&b, " transport_errors=%d", res.TransportErrors)
		}
		if res.ContextErrors > 0 {
			fmt.Fprintf(&b, " context_errors=%d", res.ContextErrors)
		}
		if res.TruncatedOutputs > 0 {
			fmt.Fprintf(&b, " truncated_outputs=%d", res.TruncatedOutputs)
		}
		b.WriteString("\n")
	}
	return b.String()
}
