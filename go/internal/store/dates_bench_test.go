package store

import (
	"strings"
	"testing"
	"time"
)

var datesBenchSink []time.Time

// BenchmarkExtractDates measures the extraction on real-shaped contents
// (datesDoc) at 2 KB, 20 KB (about the median stored content), 50 KB (the
// client maximum) and 400 KB, with few and with many dates, and on worst-case
// contents that consist of nothing but date candidates of all four forms
// (datesWorstDoc). The dates metric is the number of timestamps the
// extraction returned.
func BenchmarkExtractDates(b *testing.B) {
	densities := []struct {
		name  string
		dense bool
	}{{"few", false}, {"many", true}, {"worst", false}}
	sizes := []struct {
		name string
		n    int
	}{{"2KB", 2 << 10}, {"20KB", 20 << 10}, {"50KB", 50 << 10}, {"400KB", 400 << 10}}
	for _, d := range densities {
		for _, sz := range sizes {
			var content string
			if d.name == "worst" {
				content = datesWorstDoc(sz.n)
			} else {
				content = datesDoc(1, sz.n, d.dense)
			}
			b.Run(d.name+"/"+sz.name, func(b *testing.B) {
				b.SetBytes(int64(len(content)))
				b.ReportAllocs()
				for b.Loop() {
					datesBenchSink = ExtractDates(content)
				}
				b.ReportMetric(float64(len(datesBenchSink)), "dates")
			})
		}
	}
}

// datesWorstDoc returns n bytes made of back-to-back candidates of all four
// date forms: every year token is a match or a near miss of each form, and
// whitespace runs sit between month names and years.
func datesWorstDoc(n int) string {
	const unit = "Mai 2026 2026-01-01T01:01:01Z 01.01.2026 01:01 M\u00e4rz   2030 2020-13-01 31.04.2026 " +
		"May\t2027 december 2025 2026-02-29 2029\n"
	s := strings.Repeat(unit, n/len(unit)+1)
	return s[:n]
}
