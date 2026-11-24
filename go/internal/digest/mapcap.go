package digest

import (
	"container/heap"
	"fmt"
	"sort"

	"github.com/GottZ/ctx/internal/util"
)

// maxMapBytes caps the text of the topic map (issue #518). The map has one
// line per block, so without a cap it grew with the corpus — about 48 bytes a
// block — until the full-text vectors of its row no longer fitted:
// context_blocks keeps ts_de and ts_en over title and content, and
// PostgreSQL refuses a tsvector whose lexemes and positions pass 1 MB
// (SQLSTATE 54000, "string is too long for tsvector"), at about 60,000 to
// 70,000 blocks of realistic titles. The project's target is 1 M–10 M blocks.
//
// How many vector bytes a byte of map costs depends on the titles: about 0.6
// for realistic ones, up to 2.8 for titles built to cost the most (hyphenated
// compounds of distinct parts, where the parser emits the whole compound and
// every part, with two-byte capitals in the parts whose lower case takes
// three bytes; URLs, emitted as URL, host and path), counted as PostgreSQL
// counts — lexemes and positions — on the production image with both
// configurations. The largest map of such titles that still fits is about
// 366 KiB; a quarter of a mebibyte keeps a margin of about 1.4 over the
// costliest pattern measured, and a map at the cap passes the limit only
// from 4.0 vector bytes a byte on. Fixed by that limit, not a tuning knob: the limit is
// PostgreSQL's, and a smaller map is what the root map (root_map.*) is for.
//
// Below the cap the map is byte for byte what it was. Above it the map says
// so in its second line, in each category it lists in part and, when not
// every category fits, in a last line; the categories share the cap (see
// mapBuilder).
const maxMapBytes = 256 << 10

// mapTitleRunes is how many runes of a title a map line shows.
const mapTitleRunes = 70

// minLineBytes is the shortest line a map can carry: two spaces, the eight
// characters of an id prefix (ids are UUIDs), a space, an empty title (the
// database does not refuse one) and the newline. minCategoryBytes is the
// shortest category header: a newline, an empty name, " (1)" and a newline.
// Both are lower bounds, so the line and category counts derived from them are
// upper bounds of what a map can list (see mapBuilder.prune).
const (
	minLineBytes     = len("  ") + 8 + len(" ") + len("\n")
	minCategoryBytes = len("\n (1)\n")
)

// mapRow is what a map line is made of.
type mapRow struct{ id, title, category, scope string }

// mapLine is a rendered line with the key it is ordered by.
type mapLine struct{ title, id, text string }

// lineBefore orders lines by title, then id — byte order, as the full map
// orders its titles.
func lineBefore(a, b mapLine) bool {
	if a.title != b.title {
		return a.title < b.title
	}
	return a.id < b.id
}

// lineHeap holds the first lines of a category: a max-heap, so the line that
// sorts last is the one a full heap gives up.
type lineHeap []mapLine

func (h lineHeap) Len() int           { return len(h) }
func (h lineHeap) Less(i, j int) bool { return lineBefore(h[j], h[i]) }
func (h lineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *lineHeap) Push(x any)        { *h = append(*h, x.(mapLine)) }
func (h *lineHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// mapCategory is a category of a capped map: its block count and the lines
// that sort first, as many as can still be listed.
type mapCategory struct {
	name  string
	n     int
	lines lineHeap
}

// categoryHeap holds the categories a capped map can still list: a max-heap
// by name, so the category that sorts last is the one given up when there are
// more than any map can list.
type categoryHeap []*mapCategory

func (h categoryHeap) Len() int           { return len(h) }
func (h categoryHeap) Less(i, j int) bool { return h[i].name > h[j].name }
func (h categoryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *categoryHeap) Push(x any)        { *h = append(*h, x.(*mapCategory)) }
func (h *categoryHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// mapBuilder builds the topic map from rows that arrive grouped by category
// (the source query orders by category), holding memory bounded by the cap
// rather than by the corpus: before issue #518 the run kept every row of the
// read window in memory: a peak heap of 0.9 to 1.3 GB at 1 M blocks
// (measured), past the 768 MiB GOMEMLIMIT of the image; 16 to 80 MB now, by the shape of the corpus.
//
// While the full map can still fit, every row is kept, and if it fits in the
// end the map is rendered exactly as before (renderFull). Once it cannot, the
// builder keeps per category its block count and the lines that sort first,
// at most keep of them, and gives up the categories that sort last once more
// are held than any map can list. A capped map shares the cap between the
// categories it lists: each lists the same number of lines (or all of its
// own, when it has fewer), the largest number that fits, and what is left is
// given, one line each, to the first categories in order. When not even every
// category's header fits, the map lists the categories that fit, without
// lines, and names how many it left out.
type mapBuilder struct {
	home      string
	budget    int
	headerMin int // the header line with the smallest numbers

	blocks int          // rows added
	cats   int          // categories added
	cur    *mapCategory // the category the last row belonged to

	// While full: every row, and the bytes of the full map's lines and of the
	// headers of its finished categories.
	full      bool
	rows      []mapRow
	bodyBytes int

	// Once capped.
	held     categoryHeap
	evicted  int // categories given up
	evictedN int // their blocks
	keep     int // lines kept per category: an upper bound of what a map can list, plus one
	kept     int // lines held over all categories
}

// newMapBuilder starts a map of homeScope capped at budget bytes.
func newMapBuilder(homeScope string, budget int) *mapBuilder {
	return &mapBuilder{
		home:      homeScope,
		budget:    budget,
		headerMin: len(mapHeader(homeScope, 0, 0, "2006-01-02")),
		full:      true,
	}
}

func mapHeader(home string, blocks, categories int, date string) string {
	return fmt.Sprintf("Context Store Index | scope:%s | %d blocks | %d categories | %s\n", home, blocks, categories, date)
}

func fullCategoryHeader(name string, n int) string { return fmt.Sprintf("\n%s (%d)\n", name, n) }

func cappedCategoryHeader(name string, n, listed int) string {
	if listed == n {
		return fullCategoryHeader(name, n)
	}
	return fmt.Sprintf("\n%s (%d, %d listed)\n", name, n, listed)
}

func truncationNote(listed, blocks, budget int) string {
	return fmt.Sprintf("Truncated: %d of %d blocks listed, this map is capped at %d bytes. "+
		"A category lists its first titles in title order; `ctx search <category> query:<words>` finds the others.\n",
		listed, blocks, budget)
}

func moreCategoriesLine(categories, blocks int) string {
	return fmt.Sprintf("\n%d more categories with %d blocks not listed\n", categories, blocks)
}

// line renders a row as the map shows it: id prefix, the title cut to
// mapTitleRunes runes (rune-aware — a byte cut can split a multi-byte char,
// leaving invalid UTF-8 that fails the upsert: 22021), and the scope when it
// is not the home scope.
func (b *mapBuilder) line(r mapRow) string {
	idPrefix := r.id
	if len(idPrefix) > 8 {
		idPrefix = idPrefix[:8]
	}
	scope := ""
	if r.scope != b.home {
		scope = " [" + r.scope + "]"
	}
	return "  " + idPrefix + " " + util.TruncateRunes(r.title, mapTitleRunes) + scope + "\n"
}

// add takes the next row.
func (b *mapBuilder) add(r mapRow) {
	// A finish can switch to capped and replay the rows, which leaves the
	// replayed category current again; finishing it once more holds it.
	for b.cur != nil && b.cur.name != r.category {
		b.finish()
	}
	if b.cur == nil {
		b.cur = &mapCategory{name: r.category}
		b.cats++
	}
	b.cur.n++
	b.blocks++
	if b.full {
		b.rows = append(b.rows, r)
		b.bodyBytes += len(b.line(r))
		if b.bodyBytes+b.headerMin > b.budget {
			b.capped()
		}
		return
	}
	b.keepLine(b.cur, mapLine{title: r.title, id: r.id, text: b.line(r)})
}

// finish closes the current category.
func (b *mapBuilder) finish() {
	c := b.cur
	b.cur = nil
	if b.full {
		b.bodyBytes += len(fullCategoryHeader(c.name, c.n))
		if b.bodyBytes+b.headerMin > b.budget {
			b.capped()
		}
		return
	}
	heap.Push(&b.held, c)
	if b.held.Len() > b.budget/minCategoryBytes {
		// More categories than any map can show headers for: the one that
		// sorts last cannot be among those a map lists.
		e := heap.Pop(&b.held).(*mapCategory)
		b.evicted++
		b.evictedN += e.n
		b.kept -= e.lines.Len()
	}
}

// capped switches from keeping every row to the capped structures and replays
// the rows kept so far through them.
func (b *mapBuilder) capped() {
	rows := b.rows
	b.full, b.rows, b.bodyBytes = false, nil, 0
	b.blocks, b.cats, b.cur = 0, 0, nil
	b.keep = b.budget/minLineBytes + 1
	for _, r := range rows {
		b.add(r)
	}
}

// keepLine offers l to category c: kept while c holds fewer than keep lines
// or l sorts before the last one it holds.
func (b *mapBuilder) keepLine(c *mapCategory, l mapLine) {
	switch {
	case b.keep <= 0:
		return
	case c.lines.Len() < b.keep:
		heap.Push(&c.lines, l)
		b.kept++
	case lineBefore(l, c.lines[0]):
		c.lines[0] = l
		heap.Fix(&c.lines, 0)
	}
	if b.kept > 2*(b.budget/minLineBytes+b.held.Len()+1) {
		b.prune()
	}
}

// prune lowers keep to the most lines per category any map over the counts
// seen so far can list, plus one, and trims every category to it. The counts
// only grow and categories only come, so a later map can list no more: what is
// trimmed is never needed. After a prune at most budget/minLineBytes plus one
// line per category are held, half of what triggers the next one.
func (b *mapBuilder) prune() {
	cats := append([]*mapCategory(nil), b.held...)
	if b.cur != nil {
		cats = append(cats, b.cur)
	}
	avail := b.budget - b.headerMin
	fits := func(q int) bool {
		total := 0
		for _, c := range cats {
			total += len(fullCategoryHeader(c.name, c.n)) + min(c.n, q)*minLineBytes
			if total > avail {
				return false
			}
		}
		return true
	}
	if b.evicted > 0 || !fits(0) {
		b.keep = 0
	} else {
		// fits(keep-1) need not hold; search [0, keep-1] for the largest q.
		lo, hi := 0, b.keep-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if fits(mid) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		b.keep = lo + 1
	}
	b.kept = 0
	for _, c := range cats {
		for c.lines.Len() > b.keep {
			heap.Pop(&c.lines)
		}
		b.kept += c.lines.Len()
	}
}

// mapText is a rendered map.
type mapText struct {
	text       string
	blocks     int
	categories int
	listed     int // blocks the map lists by line
	truncated  bool
}

// render finishes the map, dated date.
func (b *mapBuilder) render(date string) mapText {
	for b.cur != nil {
		b.finish()
	}
	if b.full {
		text, cats := renderFull(b.home, date, b.rows)
		if len(text) <= b.budget {
			return mapText{text: text, blocks: b.blocks, categories: cats, listed: b.blocks}
		}
		b.capped()
		for b.cur != nil {
			b.finish()
		}
	}
	return b.renderCapped(date)
}

// renderFull is the map as it was before the cap, byte for byte: categories
// in byte order, titles in byte order within each. Two lines whose titles are
// byte for byte equal (the same title in two scopes of the window) follow
// their ids (lineBefore, the key the capped map orders by): before issue #525
// they kept the order the source query returned them in, through a sort that
// is not stable, so the database's plan decided it.
func renderFull(home, date string, rows []mapRow) (string, int) {
	categories := make(map[string][]mapRow)
	for _, r := range rows {
		categories[r.category] = append(categories[r.category], r)
	}
	catNames := make([]string, 0, len(categories))
	for cat := range categories {
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)

	b := mapBuilder{home: home}
	text := []byte(mapHeader(home, len(rows), len(catNames), date))
	for _, cat := range catNames {
		catRows := categories[cat]
		sort.Slice(catRows, func(i, j int) bool {
			if catRows[i].title != catRows[j].title {
				return catRows[i].title < catRows[j].title
			}
			return catRows[i].id < catRows[j].id
		})
		text = append(text, fullCategoryHeader(cat, len(catRows))...)
		for _, r := range catRows {
			text = append(text, b.line(r)...)
		}
	}
	return string(text), len(catNames)
}

// renderCapped renders a map that does not fit whole (see mapBuilder).
func (b *mapBuilder) renderCapped(date string) mapText {
	cats := []*mapCategory(b.held)
	sort.Slice(cats, func(i, j int) bool { return cats[i].name < cats[j].name })
	pre := make([][]int, len(cats)) // per category: bytes of its first k lines
	for i, c := range cats {
		sort.Slice(c.lines, func(x, y int) bool { return lineBefore(c.lines[x], c.lines[y]) })
		pre[i] = make([]int, len(c.lines)+1)
		for k, l := range c.lines {
			pre[i][k+1] = pre[i][k] + len(l.text)
		}
	}
	nCats := len(cats) + b.evicted
	header := mapHeader(b.home, b.blocks, nCats, date)
	avail := b.budget - len(header) - len(truncationNote(b.blocks, b.blocks, b.budget))

	headers := 0
	for _, c := range cats {
		headers += len(cappedCategoryHeader(c.name, c.n, 0))
	}
	listed := make([]int, len(cats))
	shown := len(cats)
	if b.evicted > 0 || headers > avail {
		// Not every category fits even without lines: list those that do.
		avail -= len(moreCategoriesLine(nCats, b.blocks))
		used := 0
		for i, c := range cats {
			if used+len(cappedCategoryHeader(c.name, c.n, 0)) > avail {
				shown = i
				break
			}
			used += len(cappedCategoryHeader(c.name, c.n, 0))
		}
	} else {
		b.share(cats, pre, listed, avail)
	}

	total := 0
	out := []byte(header)
	for _, k := range listed {
		total += k
	}
	out = append(out, truncationNote(total, b.blocks, b.budget)...)
	shownBlocks := 0
	for i, c := range cats[:shown] {
		out = append(out, cappedCategoryHeader(c.name, c.n, listed[i])...)
		for _, l := range c.lines[:listed[i]] {
			out = append(out, l.text...)
		}
		shownBlocks += c.n
	}
	if shown < nCats {
		out = append(out, moreCategoriesLine(nCats-shown, b.blocks-shownBlocks)...)
	}
	return mapText{text: string(out), blocks: b.blocks, categories: nCats, listed: total, truncated: true}
}

// share fills listed with the lines per category: the largest q such that
// every category listing min(n, q) lines fits avail, then one more line each
// to the first categories in order while it fits. The cost counts each header
// in the form it is rendered in, so the map is exactly as long as counted. It
// grows with q unless a category's last line is shorter than the ", k listed"
// its header drops when the category is listed whole; there the search may
// settle below the largest q, never above what fits.
func (b *mapBuilder) share(cats []*mapCategory, pre [][]int, listed []int, avail int) {
	cost := func(q int) (int, bool) {
		total := 0
		for i, c := range cats {
			k := min(q, c.n)
			if k > len(c.lines) {
				return 0, false // lines not kept: q is past what any map can list
			}
			total += len(cappedCategoryHeader(c.name, c.n, k)) + pre[i][k]
		}
		return total, total <= avail
	}
	lo, hi := 0, 0 // cost(0) fits: the caller checked the headers alone
	for _, c := range cats {
		hi = max(hi, min(c.n, len(c.lines)))
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, ok := cost(mid); ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	used, _ := cost(lo)
	for i, c := range cats {
		k := min(lo, c.n)
		listed[i] = k
		if k == c.n || k >= len(c.lines) {
			continue
		}
		more := used - len(cappedCategoryHeader(c.name, c.n, k)) + len(cappedCategoryHeader(c.name, c.n, k+1)) + len(c.lines[k].text)
		if more <= avail {
			listed[i], used = k+1, more
		}
	}
}
