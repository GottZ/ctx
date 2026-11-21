package store

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

var datesBenchSink []time.Time

// BenchmarkExtractDates measures the extraction on real-shaped contents
// (datesDoc) at 2 KB, 20 KB (about the median stored content), 50 KB (the
// client maximum) and 400 KB, with few and with many dates, and on worst-case
// contents that consist of nothing but date candidates of all four forms
// (datesWorstDoc), the same with every year outside 2020-2039, the token of
// the year window before #437 (wideworst, datesWideWorstDoc), and on
// number-heavy contents without a date (loglines, datesLogLinesDoc; years,
// datesYearsDoc), issue #451. The dates metric is the number of timestamps the
// extraction returned.
func BenchmarkExtractDates(b *testing.B) {
	densities := []struct {
		name  string
		dense bool
	}{{"few", false}, {"many", true}, {"worst", false}, {"wideworst", false}, {"loglines", false}, {"years", false}}
	sizes := []struct {
		name string
		n    int
	}{{"2KB", 2 << 10}, {"20KB", 20 << 10}, {"50KB", 50 << 10}, {"400KB", 400 << 10}}
	for _, d := range densities {
		for _, sz := range sizes {
			var content string
			switch d.name {
			case "worst":
				content = datesWorstDoc(sz.n)
			case "wideworst":
				content = datesWideWorstDoc(sz.n)
			case "loglines":
				content = datesLogLinesDoc(sz.n)
			case "years":
				content = datesYearsDoc(sz.n)
			default:
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

// datesWideWorstDoc is datesWorstDoc with every year outside 2020-2039: the
// year tokens the window of #437 adds, each one a match or a near miss.
func datesWideWorstDoc(n int) string {
	const unit = "Mai 2046 2046-01-01T01:01:01Z 01.01.2046 01:01 M\u00e4rz   2099 2000-13-01 31.04.2046 " +
		"May\t2047 december 2015 2046-02-29 2049\n"
	s := strings.Repeat(unit, n/len(unit)+1)
	return s[:n]
}

// datesLogLinesDoc returns n bytes of log lines whose numbers 2000-2099 are no
// dates: an id behind "=", a port behind a space and a word that ends like a
// month name ("port", "august"), a five-digit size.
func datesLogLinesDoc(n int) string {
	r := rand.New(rand.NewPCG(451, 1))
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("id=20" + pad2(r.IntN(100)) + " port 20" + pad2(r.IntN(100)) +
			" size=20" + pad2(r.IntN(100)) + strconv.Itoa(r.IntN(10)) + "\n")
	}
	return b.String()[:n]
}

// datesYearsDoc returns n bytes of "2087 ": a year token of the window behind
// every space, and no date.
func datesYearsDoc(n int) string {
	s := strings.Repeat("2087 ", n/5+1)
	return s[:n]
}
