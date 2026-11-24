// Issue #518 unit gates: the topic map is capped independently of the corpus,
// says what it leaves out, keeps memory bounded by the cap while it streams,
// and is byte for byte the uncapped map wherever that fits.
package digest

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/util"
)

const testDate = "2026-11-23"

// uncappedMap is the map as RunDigest rendered it before issue #518, copied
// from the code of that version, with one change of issue #525: lines whose
// titles are byte for byte equal follow their ids (that version kept the order
// the source query returned them in, through a sort that is not stable). It
// is the reference the capped builder must match below the cap.
func uncappedMap(homeScope string, rows []mapRow) string {
	categories := make(map[string][]mapRow)
	for _, b := range rows {
		categories[b.category] = append(categories[b.category], b)
	}
	catNames := make([]string, 0, len(categories))
	for cat := range categories {
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Context Store Index | scope:%s | %d blocks | %d categories | %s\n",
		homeScope, len(rows), len(catNames), testDate)
	for _, cat := range catNames {
		catBlocks := categories[cat]
		sort.Slice(catBlocks, func(i, j int) bool {
			if catBlocks[i].title != catBlocks[j].title {
				return catBlocks[i].title < catBlocks[j].title
			}
			return catBlocks[i].id < catBlocks[j].id
		})
		fmt.Fprintf(&sb, "\n%s (%d)\n", cat, len(catBlocks))
		for _, b := range catBlocks {
			idPrefix := b.id
			if len(idPrefix) > 8 {
				idPrefix = idPrefix[:8]
			}
			title := util.TruncateRunes(b.title, 70)
			scopeAnnotation := ""
			if b.scope != homeScope {
				scopeAnnotation = " [" + b.scope + "]"
			}
			fmt.Fprintf(&sb, "  %s %s%s\n", idPrefix, title, scopeAnnotation)
		}
	}
	return sb.String()
}

// sourceOrder sorts rows in one order the source query may return them in:
// grouped by category, which is all it promises (mapSourceQuery); within a
// category by title here, while the database leaves that order to its plan.
func sourceOrder(rows []mapRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].category != rows[j].category {
			return rows[i].category < rows[j].category
		}
		return rows[i].title < rows[j].title
	})
}

func testID(r *rand.Rand) string {
	return fmt.Sprintf("%08x-%04x-7%03x-8%03x-%012x", r.Uint32(), r.Uint32()&0xffff, r.Uint32()&0xfff, r.Uint32()&0xfff, r.Uint64()&0xffffffffffff)
}

var testWords = []string{"Index", "Retrieval", "Prüfung", "Übergabe", "größe", "naïve", "漢字", "emoji 🎯", "Tenant", "x", "Ⱥtlas", "deploy", "a-b-c"}

func testTitle(r *rand.Rand) string {
	n := 1 + r.IntN(14)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = testWords[r.IntN(len(testWords))]
	}
	return strings.Join(parts, " ")
}

// testCorpus builds n rows over the given categories and scopes, with
// repeated titles (ties within a category) and multi-byte runes at the 70-rune
// cut, in source order.
func testCorpus(r *rand.Rand, n int, categories, scopes []string, weights []int) []mapRow {
	total := 0
	for _, w := range weights {
		total += w
	}
	rows := make([]mapRow, n)
	for i := range rows {
		x, c := r.IntN(total), 0
		for x >= weights[c] {
			x -= weights[c]
			c++
		}
		rows[i] = mapRow{id: testID(r), title: testTitle(r), category: categories[c], scope: scopes[r.IntN(len(scopes))]}
	}
	sourceOrder(rows)
	return rows
}

func build(home string, budget int, rows []mapRow) (mapText, *mapBuilder) {
	b := newMapBuilder(home, budget)
	for _, r := range rows {
		b.add(r)
	}
	return b.render(testDate), b
}

// TestMapBelowTheCapIsTheUncappedMap: wherever the uncapped map fits, the
// capped builder renders exactly its bytes — ties, scope annotations and
// multi-byte cuts included. RED against a builder that changes the full path
// in any byte.
func TestMapBelowTheCapIsTheUncappedMap(t *testing.T) {
	r := rand.New(rand.NewPCG(518, 1))
	cats := []string{"decisions", "learnings", "Ärger", "reference", "z"}
	for trial := 0; trial < 300; trial++ {
		n := 1 + r.IntN(400)
		rows := testCorpus(r, n, cats, []string{"private", "work", "shared"}, []int{5, 9, 1, 3, 1})
		want := uncappedMap("private", append([]mapRow(nil), rows...))
		got, _ := build("private", maxMapBytes, rows)
		if got.text != want {
			t.Fatalf("trial %d (%d rows): the map below the cap differs from the uncapped one:\n--- got ---\n%.600s\n--- want ---\n%.600s", trial, n, got.text, want)
		}
		if got.truncated || got.listed != n || got.blocks != n {
			t.Fatalf("trial %d: truncated=%v listed=%d blocks=%d, want false/%d/%d", trial, got.truncated, got.listed, got.blocks, n, n)
		}
	}
	// The largest corpus whose uncapped map still fits stays uncapped too: the
	// bound is the exact length, not an estimate.
	all := testCorpus(r, 6000, cats, []string{"private"}, []int{1, 1, 1, 1, 1})
	fits := func(n int) bool {
		rows := append([]mapRow(nil), all[:n]...)
		sourceOrder(rows)
		return len(uncappedMap("private", rows)) <= maxMapBytes
	}
	lo, hi := 1, len(all)
	if fits(hi) {
		t.Fatal("fixture too small: the whole corpus fits the cap")
	}
	for lo < hi-1 { // fits(lo), !fits(hi)
		if mid := (lo + hi) / 2; fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	rows := append([]mapRow(nil), all[:lo]...)
	sourceOrder(rows)
	want := uncappedMap("private", append([]mapRow(nil), rows...))
	if got, _ := build("private", maxMapBytes, rows); got.text != want {
		t.Fatalf("a map of %d bytes (cap %d) was not rendered uncapped", len(want), maxMapBytes)
	}
}

// TestMapDoesNotDependOnTheRowOrderWithinACategory (issue #525): the source
// query promises only that rows arrive grouped by category, so the map must be
// the same bytes for every order of the rows within a category — titles that
// are byte for byte equal included (the same title in several scopes of the
// window), which follow their ids. RED before #525: the full map ordered such
// lines with a sort that is not stable, so the order the database returned
// them in decided the map. Checked below the cap (renderFull) and above it
// (the capped heaps, which order by title and id already).
func TestMapDoesNotDependOnTheRowOrderWithinACategory(t *testing.T) {
	r := rand.New(rand.NewPCG(525, 1))
	var scopes []string
	for i := 0; i < 40; i++ {
		scopes = append(scopes, fmt.Sprintf("s%02d", i))
	}
	corpus := func(n int, title func(i int) string) []mapRow {
		rows := make([]mapRow, n)
		for i := range rows {
			rows[i] = mapRow{id: testID(r), title: title(i), category: []string{"decisions", "learnings"}[i%2], scope: scopes[i%len(scopes)]}
		}
		return rows
	}
	shuffledWithinCategories := func(rows []mapRow) []mapRow {
		out := append([]mapRow(nil), rows...)
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		sort.SliceStable(out, func(i, j int) bool { return out[i].category < out[j].category })
		return out
	}
	for _, c := range []struct {
		name   string
		budget int
		rows   []mapRow
	}{
		// 80 lines, four titles: 20 byte-equal titles per title and category pair.
		{"below the cap", maxMapBytes, corpus(160, func(i int) string { return []string{"Übergabe", "deploy", "漢字", "x"}[(i/2)%4] })},
		// The same shape past a small cap.
		{"above the cap", 2048, corpus(400, func(i int) string { return []string{"Übergabe", "deploy", "漢字", "x"}[(i/2)%4] })},
	} {
		first, _ := build("s00", c.budget, shuffledWithinCategories(c.rows))
		if c.budget == maxMapBytes && first.truncated {
			t.Fatalf("%s: fixture is truncated", c.name)
		}
		if c.budget != maxMapBytes && !first.truncated {
			t.Fatalf("%s: fixture is not truncated", c.name)
		}
		for trial := 0; trial < 20; trial++ {
			got, _ := build("s00", c.budget, shuffledWithinCategories(c.rows))
			if got.text != first.text {
				t.Fatalf("%s, trial %d: another order of the rows within their categories renders another map:\n--- got ---\n%.800s\n--- first ---\n%.800s", c.name, trial, got.text, first.text)
			}
		}
		// Byte-equal titles follow their ids.
		lines := strings.Split(first.text, "\n")
		for i := 1; i < len(lines); i++ {
			a, b := lines[i-1], lines[i]
			if !strings.HasPrefix(a, "  ") || !strings.HasPrefix(b, "  ") || len(a) < 11 || len(b) < 11 {
				continue
			}
			if ta, tb := strings.SplitN(a[11:], " [", 2)[0], strings.SplitN(b[11:], " [", 2)[0]; ta == tb && a[2:10] > b[2:10] {
				t.Fatalf("%s: lines of the title %q are not in id order: %q before %q", c.name, ta, a, b)
			}
		}
	}
}

var cappedHeaderRe = regexp.MustCompile(`^(.*) \((\d+)(?:, (\d+) listed)?\)$`)

// checkCappedMap verifies a capped map against its source rows: the length
// bound, the header counts, the truncation note, categories in byte order with
// their true counts, each listing the first of its titles in title order, and
// a last line for the categories left out.
func checkCappedMap(t *testing.T, budget int, home string, rows []mapRow, m mapText) {
	t.Helper()
	if len(m.text) > budget {
		t.Fatalf("map of %d bytes passes the cap of %d", len(m.text), budget)
	}
	if !utf8.ValidString(m.text) {
		t.Fatal("map is not valid UTF-8")
	}
	byCat := map[string][]mapRow{}
	for _, r := range rows {
		byCat[r.category] = append(byCat[r.category], r)
	}
	names := make([]string, 0, len(byCat))
	for n := range byCat {
		names = append(names, n)
	}
	sort.Strings(names)
	if !m.truncated {
		t.Fatal("map past the cap is not marked truncated")
	}
	lines := strings.Split(strings.TrimSuffix(m.text, "\n"), "\n")
	wantHeader := strings.TrimSuffix(mapHeader(home, len(rows), len(names), testDate), "\n")
	if lines[0] != wantHeader {
		t.Fatalf("header %q, want %q", lines[0], wantHeader)
	}
	wantNote := strings.TrimSuffix(truncationNote(m.listed, len(rows), budget), "\n")
	if lines[1] != wantNote {
		t.Fatalf("second line %q, want the truncation note %q", lines[1], wantNote)
	}
	listed, ci, shownBlocks := 0, -1, 0
	var cat string
	var catLines []string
	flush := func() {
		if ci < 0 {
			return
		}
		src := append([]mapRow(nil), byCat[cat]...)
		sort.Slice(src, func(i, j int) bool {
			return lineBefore(mapLine{title: src[i].title, id: src[i].id}, mapLine{title: src[j].title, id: src[j].id})
		})
		b := mapBuilder{home: home}
		for k, l := range catLines {
			if want := strings.TrimSuffix(b.line(src[k]), "\n"); l != want {
				t.Fatalf("category %q line %d = %q, want %q (the first titles in title order)", cat, k, l, want)
			}
		}
	}
	more := regexp.MustCompile(`^(\d+) more categories with (\d+) blocks not listed$`)
	for i := 2; i < len(lines); i++ {
		l := lines[i]
		switch {
		case l == "":
			continue
		case strings.HasPrefix(l, "  "):
			catLines = append(catLines, l)
			listed++
		case more.MatchString(l):
			flush()
			g := more.FindStringSubmatch(l)
			nc, _ := strconv.Atoi(g[1])
			nb, _ := strconv.Atoi(g[2])
			if ci+1+nc != len(names) || shownBlocks+nb != len(rows) {
				t.Fatalf("%q after %d categories with %d blocks; source has %d categories, %d blocks", l, ci+1, shownBlocks, len(names), len(rows))
			}
			if i != len(lines)-1 {
				t.Fatalf("the categories line is not the last line")
			}
			ci = len(names) // all accounted for
		default:
			flush()
			g := cappedHeaderRe.FindStringSubmatch(l)
			if g == nil {
				t.Fatalf("unexpected line %q", l)
			}
			ci++
			if ci >= len(names) || g[1] != names[ci] {
				t.Fatalf("category %d is %q, want %q (byte order)", ci, g[1], names[ci])
			}
			cat, catLines = g[1], nil
			n, _ := strconv.Atoi(g[2])
			if n != len(byCat[cat]) {
				t.Fatalf("category %q says %d blocks, source has %d", cat, n, len(byCat[cat]))
			}
			shownBlocks += n
			if g[3] != "" {
				if k, _ := strconv.Atoi(g[3]); k >= n {
					t.Fatalf("category %q: %q lists all its blocks in the partial form", cat, l)
				}
			}
		}
	}
	flush()
	if ci != len(names)-1 && ci != len(names) {
		t.Fatalf("map shows %d of %d categories without a line for the rest", ci+1, len(names))
	}
	if listed != m.listed {
		t.Fatalf("map carries %d lines, reports %d listed", listed, m.listed)
	}
	// Each partially listed category lists as many lines as its header says.
	for _, block := range strings.Split(m.text, "\n\n")[1:] {
		head, body, _ := strings.Cut(block, "\n")
		g := cappedHeaderRe.FindStringSubmatch(head)
		if g == nil {
			continue
		}
		n, _ := strconv.Atoi(g[2])
		k := n
		if g[3] != "" {
			k, _ = strconv.Atoi(g[3])
		}
		got := 0
		for _, l := range strings.Split(body, "\n") {
			if strings.HasPrefix(l, "  ") {
				got++
			}
		}
		if got != k {
			t.Fatalf("category %q says %d listed, carries %d lines", g[1], k, got)
		}
	}
}

// TestMapCapHoldsAtAMillionBlocks: a million blocks over five scopes and forty
// categories of very different sizes. The map stays under the cap, says what
// it leaves out, gives every category a share of it, and while it streams the
// builder never holds more than a bound set by the cap — not by the corpus.
func TestMapCapHoldsAtAMillionBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("a million rows")
	}
	r := rand.New(rand.NewPCG(518, 2))
	cats := make([]string, 40)
	weights := make([]int, 40)
	for i := range cats {
		cats[i] = fmt.Sprintf("category-%02d", i)
		weights[i] = 1 + i*i // from 1 to 1522: some categories small, some large
	}
	rows := testCorpus(r, 1_000_000, cats, []string{"private", "work", "shared", "team-a", "team-b"}, weights)

	b := newMapBuilder("private", maxMapBytes)
	rowBound := maxMapBytes/minLineBytes + 1
	for i, row := range rows {
		b.add(row)
		if len(b.rows) > rowBound {
			t.Fatalf("after %d rows the builder keeps %d whole rows, bound %d", i+1, len(b.rows), rowBound)
		}
		if limit := 2*(maxMapBytes/minLineBytes+b.held.Len()+1) + 1; b.kept > limit {
			t.Fatalf("after %d rows the builder holds %d lines, bound %d", i+1, b.kept, limit)
		}
	}
	m := b.render(testDate)
	checkCappedMap(t, maxMapBytes, "private", rows, m)
	if m.listed == 0 || m.listed >= len(rows) {
		t.Fatalf("listed %d of %d", m.listed, len(rows))
	}
	// Every category gets lines; the smallest ones are listed whole.
	if strings.Contains(m.text, ", 0 listed)") {
		t.Fatalf("a category got no share of the cap:\n%.2000s", m.text)
	}
	if smallest := fullCategoryHeader("category-00", countCat(rows, "category-00")); !strings.Contains(m.text, smallest) {
		t.Fatalf("the smallest category is not listed whole (want %q)", smallest)
	}
	t.Logf("1,000,000 blocks: map %d bytes, %d lines listed, %d lines held at the end", len(m.text), m.listed, b.kept)
}

// TestMapCapHoldsOneLargeCategoryBounded: the bound on whole rows holds
// inside a category too — a corpus that is one category never reaches a
// category boundary, where the builder also checks the full map's size.
func TestMapCapHoldsOneLargeCategoryBounded(t *testing.T) {
	r := rand.New(rand.NewPCG(518, 6))
	rows := make([]mapRow, 200_000)
	for i := range rows {
		rows[i] = mapRow{id: testID(r), title: testTitle(r), category: "learnings", scope: "private"}
	}
	sourceOrder(rows)
	b := newMapBuilder("private", maxMapBytes)
	rowBound := maxMapBytes/minLineBytes + 1
	for i, row := range rows {
		b.add(row)
		if len(b.rows) > rowBound {
			t.Fatalf("after %d rows of one category the builder keeps %d whole rows, bound %d", i+1, len(b.rows), rowBound)
		}
	}
	checkCappedMap(t, maxMapBytes, "private", rows, b.render(testDate))
}

func countCat(rows []mapRow, cat string) int {
	n := 0
	for _, r := range rows {
		if r.category == cat {
			n++
		}
	}
	return n
}

// TestMapCapWithMoreCategoriesThanFit: when not even every category's header
// fits, the map lists the categories that do, without lines, names how many it
// left out — and the builder holds no more categories than any map can show.
func TestMapCapWithMoreCategoriesThanFit(t *testing.T) {
	r := rand.New(rand.NewPCG(518, 3))
	const budget = 4096
	rows := make([]mapRow, 3000)
	for i := range rows {
		rows[i] = mapRow{id: testID(r), title: testTitle(r), category: fmt.Sprintf("cat-%05d", r.IntN(2500)), scope: "private"}
	}
	// Source order is the database's: grouped, but not in byte order.
	sort.SliceStable(rows, func(i, j int) bool {
		return reverse(rows[i].category) < reverse(rows[j].category)
	})
	m, b := build("private", budget, rows)
	checkCappedMap(t, budget, "private", rows, m)
	if m.listed != 0 || !strings.Contains(m.text, " more categories with ") {
		t.Fatalf("expected a categories-only map, got listed=%d:\n%s", m.listed, m.text)
	}
	if b.held.Len() > budget/minCategoryBytes {
		t.Fatalf("builder holds %d categories, bound %d", b.held.Len(), budget/minCategoryBytes)
	}
}

func reverse(s string) string {
	rs := []rune(s)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return string(rs)
}

// TestMapCapSharesTheCap: a large category does not take the cap from the
// small ones — they are listed whole, the large one in part, with its first
// titles.
func TestMapCapSharesTheCap(t *testing.T) {
	r := rand.New(rand.NewPCG(518, 4))
	var rows []mapRow
	for i := 0; i < 20000; i++ {
		rows = append(rows, mapRow{id: testID(r), title: fmt.Sprintf("big %06d", r.IntN(1_000_000)), category: "a-big", scope: "private"})
	}
	for i := 0; i < 5; i++ {
		rows = append(rows, mapRow{id: testID(r), title: fmt.Sprintf("small %d", i), category: "b-small", scope: "work"})
		rows = append(rows, mapRow{id: testID(r), title: fmt.Sprintf("tiny %d", i), category: "c-tiny", scope: "private"})
	}
	sourceOrder(rows)
	const budget = 16 << 10
	m, _ := build("private", budget, rows)
	checkCappedMap(t, budget, "private", rows, m)
	for _, want := range []string{"\nb-small (5)\n", "\nc-tiny (5)\n", "\na-big (20000, "} {
		if !strings.Contains(m.text, want) {
			t.Fatalf("map lacks %q:\n%s", want, m.text)
		}
	}
	// The budget is used: less than one more line of the big category is left.
	if left := budget - len(m.text); left > len("  01234567 big 000000\n")+8 {
		t.Fatalf("%d bytes of the cap left unused", left)
	}
}

// TestMapCapProperties: random corpora and caps — every map either is the
// uncapped one or passes checkCappedMap.
func TestMapCapProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(518, 5))
	for trial := 0; trial < 200; trial++ {
		budget := 1024 + r.IntN(24<<10)
		nCats := 1 + r.IntN(60)
		cats := make([]string, nCats)
		weights := make([]int, nCats)
		for i := range cats {
			cats[i] = fmt.Sprintf("c%03d-%s", r.IntN(1000), testWords[r.IntN(len(testWords))])
			weights[i] = 1 + r.IntN(50)
		}
		rows := testCorpus(r, 1+r.IntN(3000), cats, []string{"private", "work"}, weights)
		m, _ := build("private", budget, rows)
		if want := uncappedMap("private", append([]mapRow(nil), rows...)); len(want) <= budget {
			if m.text != want {
				t.Fatalf("trial %d: a map that fits (%d of %d bytes) is not the uncapped one", trial, len(want), budget)
			}
			continue
		}
		checkCappedMap(t, budget, "private", rows, m)
	}
}
