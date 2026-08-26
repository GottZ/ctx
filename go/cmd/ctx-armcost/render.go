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

	fmt.Fprintf(&b, "\nNicht-Störungs-Kennzahl: %s\n", rep.Interactive.Note)
	b.WriteString("\nFußzeilen (Pflicht):\n")
	for _, f := range rep.Footnotes {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	_, err := io.WriteString(w, b.String())
	return err
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
