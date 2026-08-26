package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// de formatiert eine Fließkommazahl mit deutschem Dezimalkomma.
func de(v float64, prec int) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', prec, 64), ".", ",", 1)
}

// pct formatiert einen Anteil als Prozentwert mit deutschem Komma.
func pct(v float64) string {
	return de(v*100, 1) + " %"
}

// tokenCell rendert eine Token-Summe zusammen mit der Zahl der Zeilen, in
// denen der Wert NULL ist — die nullInt-Lücke ist damit je Gruppe ablesbar.
func tokenCell(sum, null int64) string {
	return fmt.Sprintf("%d (%d)", sum, null)
}

// renderTable schreibt den Report als Klartext-Tabelle. Die Fußzeilen sind
// Teil der Ausgabe, nicht optionales Beiwerk: ohne sie ist keine Zahl dieses
// Reports interpretierbar.
func renderTable(w io.Writer, rep Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "ctx-armcost — Belegungs-Bilanz in GPU-Sekunden\n")
	fmt.Fprintf(&b, "Fenster:  %s .. %s  (%s)\n",
		rep.Since.UTC().Format(time.RFC3339), rep.Until.UTC().Format(time.RFC3339),
		de(rep.Until.Sub(rep.Since).Hours()/24, 2)+" d")
	fmt.Fprintf(&b, "Zeilen:   %d (Zähl-Gate count(*) = %d)\n", rep.RowsInWindow, rep.CountGate)
	fmt.Fprintf(&b, "%s\n\n", rep.CostUSDNote)

	writeBuckets(&b, "pipeline", rep.Pipelines)
	if len(rep.Classes) > 0 {
		b.WriteString("\n")
		writeBuckets(&b, "dispatch_class", rep.Classes)
	}
	if rep.PerTopic != nil {
		writePerTopic(&b, *rep.PerTopic)
	}

	fmt.Fprintf(&b, "\nNicht-Störungs-Kennzahl: %s\n", rep.Interactive.Note)
	b.WriteString("\nFußzeilen (Pflicht):\n")
	for _, f := range rep.Footnotes {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// stamp rendert einen optionalen Zeitpunkt; „—" statt einer erfundenen Null.
func stamp(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format(time.RFC3339)
}

// hours rendert eine optionale Stundenzahl.
func hours(h *float64) string {
	if h == nil {
		return "—"
	}
	return de(*h, 1)
}

// clip kürzt ein Label auf Tabellenbreite. Das JSON trägt es vollständig.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// writePerTopic rendert die V-W5-Sicht: Kopf mit Fenster und Zuordnungs-Bilanz,
// die Topic-Tabelle (Topics OHNE Call stehen mit 0 darin, nicht draußen), die
// Verteilung und die erschöpften Topics als eigene Sektion.
func writePerTopic(b *strings.Builder, pt PerTopicReport) {
	a := pt.Assignment
	fmt.Fprintf(b, "\nLabel-Arm-Telemetrie je Topic — arm=%s\n", pt.Arm)
	fmt.Fprintf(b, "Topic-Fenster: %s .. %s  (%s d, ab der Geburt des ältesten lebenden Topics)\n",
		pt.Since.UTC().Format(time.RFC3339), pt.Until.UTC().Format(time.RFC3339),
		de(pt.Until.Sub(pt.Since).Hours()/24, 2))
	fmt.Fprintf(b, "Zeilen des Arms: %d · zugeordnet %d · mehrdeutig %d (max %d Topics/Zeile) · "+
		"nicht zugeordnet %d (nur pensioniert %d, ohne block_ids %d)\n",
		a.ArmRows, a.AssignedRows, a.AmbiguousRows, a.MaxTopicsPerRow,
		a.UnassignedRows, a.UnassignedRetiredOnly, a.RowsWithoutBlockIDs)

	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "topic_id\tscope\tcalls\texakt\tmehrd.\tbelegung_s\twire_s\tcalls/h\tleben_h\tletzter_call\tatt\tstale\tsrc\tcore_n\tlabel\n")
	var sum TopicCalls
	for _, t := range pt.Topics {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%d\t%t\t%s\t%d\t%s\n",
			t.TopicID, t.Scope, t.Calls, t.CallsExact, t.CallsAmbiguous,
			de(t.OccupancySeconds, 1), de(t.WireSeconds, 1), de(t.CallsPerHour, 3),
			de(t.LifetimeHours, 1), stamp(t.LastCall), t.LabelAttempts, t.LabelStale,
			t.LabelSource, t.CoreN, clip(t.Label, 40))
		sum.Calls += t.Calls
		sum.CallsExact += t.CallsExact
		sum.CallsAmbiguous += t.CallsAmbiguous
		sum.OccupancySeconds += t.OccupancySeconds
		sum.WireSeconds += t.WireSeconds
	}
	// Σ zählt mehrdeutige Zeilen mehrfach — genau deshalb steht die
	// Zuordnungs-Bilanz darüber und die Mehrdeutigkeit in einer eigenen Spalte.
	_, _ = fmt.Fprintf(tw, "Σ (%d Topics)\t\t%d\t%d\t%d\t%s\t%s\t\t\t\t\t\t\t\t\n",
		pt.LivingTopics, sum.Calls, sum.CallsExact, sum.CallsAmbiguous,
		de(sum.OccupancySeconds, 1), de(sum.WireSeconds, 1))
	_ = tw.Flush()

	fmt.Fprintf(b, "\nVerteilung über %d lebende Topics (Nullen zählen mit): "+
		"Calls je Topic p50 %s / p95 %s · Calls je Lebensstunde p50 %s / p95 %s (max %s)\n",
		pt.LivingTopics, de(pt.CallsP50, 1), de(pt.CallsP95, 1),
		de(pt.CallsPerHourP50, 3), de(pt.CallsPerHourP95, 3), de(pt.CallsPerHourMax, 3))

	fmt.Fprintf(b, "\nErschöpfte Topics (lebend, label_stale AND label_attempts >= %d): %d von %d\n",
		pt.ExhaustedAttempts, len(pt.ExhaustedTopics), pt.LivingTopics)
	etw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(etw, "topic_id\tscope\tatt\tsrc\tlabel_built_at\talter_h\tcalls\tletzter_call\tcore_n\tlabel\n")
	for _, t := range pt.ExhaustedTopics {
		_, _ = fmt.Fprintf(etw, "%s\t%s\t%d\t%s\t%s\t%s\t%d\t%s\t%d\t%s\n",
			t.TopicID, t.Scope, t.LabelAttempts, t.LabelSource, stamp(t.LabelBuiltAt),
			hours(t.LabelAgeHours), t.Calls, stamp(t.LastCall), t.CoreN, clip(t.Label, 40))
	}
	_ = etw.Flush()

	b.WriteString("\nNotizen zur Per-Topic-Sicht:\n")
	for _, n := range pt.Notes {
		fmt.Fprintf(b, "  - %s\n", n)
	}
}

// writeBuckets rendert eine Gruppierung als ausgerichtete Tabelle.
func writeBuckets(b *strings.Builder, head string, buckets []Bucket) {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	// Der tabwriter puffert in den Builder — nur Flush kann überhaupt
	// fehlschlagen, deshalb sind die Zeilen-Writes bewusst verworfen.
	_, _ = fmt.Fprintf(tw, "%s\tn\tbelegung_s\twire_s\tp50_ms\tp95_ms\tprompt_tok (null)\tcompl_tok (null)\tfehler\tquote\tabort\tdur_null\tqw_null\n", head)
	var tot Bucket
	tot.Key = "Σ"
	for _, k := range buckets {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\t%d\n",
			k.Key, k.N, de(k.OccupancySeconds, 1), de(k.WireSeconds, 1),
			de(k.P50DurationMs, 0), de(k.P95DurationMs, 0),
			tokenCell(k.PromptTokens, k.PromptTokensNull),
			tokenCell(k.CompletionTokens, k.CompletionTokensNull),
			k.Errors, pct(k.ErrorRate), k.DispatchAborts, k.DurationNull, k.QueueWaitNull)
		tot.N += k.N
		tot.OccupancySeconds += k.OccupancySeconds
		tot.WireSeconds += k.WireSeconds
		tot.PromptTokens += k.PromptTokens
		tot.PromptTokensNull += k.PromptTokensNull
		tot.CompletionTokens += k.CompletionTokens
		tot.CompletionTokensNull += k.CompletionTokensNull
		tot.Errors += k.Errors
		tot.DispatchAborts += k.DispatchAborts
		tot.DurationNull += k.DurationNull
		tot.QueueWaitNull += k.QueueWaitNull
	}
	if tot.N > 0 {
		tot.ErrorRate = float64(tot.Errors) / float64(tot.N)
	}
	// Die Summenzeile führt bewusst KEINE Perzentile: p50/p95 sind über
	// Gruppen nicht addierbar und ein gemittelter Wert wäre eine erfundene
	// Zahl. Die Klassen-Tabelle liefert den aggregierten p95 dort, wo er
	// definiert ist.
	_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t\t\t%s\t%s\t%d\t%s\t%d\t%d\t%d\n",
		tot.Key, tot.N, de(tot.OccupancySeconds, 1), de(tot.WireSeconds, 1),
		tokenCell(tot.PromptTokens, tot.PromptTokensNull),
		tokenCell(tot.CompletionTokens, tot.CompletionTokensNull),
		tot.Errors, pct(tot.ErrorRate), tot.DispatchAborts, tot.DurationNull, tot.QueueWaitNull)
	_ = tw.Flush()
}
