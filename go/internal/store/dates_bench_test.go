package store

import (
	"testing"
	"time"
)

var datesBenchSink []time.Time

// BenchmarkExtractDates measures the extraction on real-shaped contents
// (datesDoc) at 2 KB, 20 KB (about the median stored content), 50 KB (the
// client maximum) and 400 KB, with few and with many dates. The dates metric
// is the number of timestamps the extraction returned.
func BenchmarkExtractDates(b *testing.B) {
	densities := []struct {
		name  string
		dense bool
	}{{"few", false}, {"many", true}}
	sizes := []struct {
		name string
		n    int
	}{{"2KB", 2 << 10}, {"20KB", 20 << 10}, {"50KB", 50 << 10}, {"400KB", 400 << 10}}
	for _, d := range densities {
		for _, sz := range sizes {
			content := datesDoc(1, sz.n, d.dense)
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
