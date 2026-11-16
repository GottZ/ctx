package ingest

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Issue #316: block-mode ingest upserts by (category, title, scope), so two
// chunks of one file that share a title address one block and the later one
// replaces the earlier. Every chunk of a file needs a title of its own.

// titleBody is a section body of roughly n bytes in paragraphs of about
// 1000 bytes, so splitOversized can cut it at paragraph boundaries. A
// negative n gives one paragraph of -n bytes (a large table), which
// splitOversized cannot cut.
func titleBody(n int) string {
	if n < 0 {
		return strings.Repeat("| row | cell |", -n/14)
	}
	if n < MinChunkChars {
		return "A short note."
	}
	para := strings.Repeat("Check the counts and compare them with the source. ", 19)
	var b strings.Builder
	b.WriteString(para)
	for b.Len() < n {
		b.WriteString("\n\n" + para)
	}
	return b.String()
}

// doc joins heading lines ("# A", "## B") and body sizes into a document.
func doc(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteString(titleBody(v))
		}
	}
	return b.String()
}

func chunkTitles(chunks []ChunkResult) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Title
	}
	return out
}

// checkTitleInvariants is what every file must satisfy: titles pairwise
// distinct (the unique index compares them byte for byte), the title is the
// name plus the heading path, valid UTF-8 and within MaxTitleBytes.
func checkTitleInvariants(t *testing.T, name string, chunks []ChunkResult) {
	t.Helper()
	seen := make(map[string]int, len(chunks))
	for i, c := range chunks {
		if j, dup := seen[c.Title]; dup {
			t.Errorf("chunks %d and %d share title %q (all: %q)", j, i, c.Title, chunkTitles(chunks))
		}
		seen[c.Title] = i
		if want := buildTitle(name, c.HeadingPath); c.Title != want {
			t.Errorf("chunk %d: title %q, want buildTitle(name, HeadingPath) = %q", i, c.Title, want)
		}
		if len(c.Title) > MaxTitleBytes {
			t.Errorf("chunk %d: title is %d bytes, over MaxTitleBytes %d", i, len(c.Title), MaxTitleBytes)
		}
		if !utf8.ValidString(c.Title) {
			t.Errorf("chunk %d: title %q is not valid UTF-8", i, c.Title)
		}
	}
}

func TestChunk_RepeatedHeadingTitles(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			// The issue's file: the same H2 under two H1 chapters takes the
			// chapter in front.
			"h2_under_two_h1",
			doc("# Alpha", 1000, "## Notes", 3000, "# Beta", 1000, "## Notes", 3000),
			[]string{"f — Alpha", "f — Alpha > Notes", "f — Beta", "f — Beta > Notes"},
		},
		{
			// No parent to tell two H1 apart: the second one is numbered.
			"repeated_h1",
			doc("# Notes", 4000, "# Notes", 4000),
			[]string{"f — Notes", "f — Notes (2)"},
		},
		{
			// An H2 before the first H1 has no parent either.
			"h2_before_any_h1",
			doc("## Notes", 4000, "## Notes", 4000),
			[]string{"f — Notes", "f — Notes (2)"},
		},
		{
			// Twice under the same parent: the parent alone does not separate
			// them, the counter does.
			"same_parent_twice",
			doc("# A", 1000, "## Notes", 2000, "## Notes", 2000, "# B", 1000, "## Notes", 2000),
			[]string{"f — A", "f — A > Notes", "f — A > Notes (2)", "f — B", "f — B > Notes"},
		},
		{
			// Each repeated section is over the chunk size; all its parts take
			// the parent, so the parts collide neither with each other nor with
			// the other section's parts.
			"oversized_repeated_section",
			doc("# Alpha", "## Notes", 6000, 1000, "# Beta", "## Notes", 6000, 1000),
			[]string{"f — Alpha > Notes", "f — Alpha > Notes (Teil 2)", "f — Beta > Notes", "f — Beta > Notes (Teil 2)"},
		},
		{
			// The same heading on two levels: the H1 has no parent and keeps
			// its title, the H2s take theirs.
			"same_heading_two_levels",
			doc("# Notes", 1000, "## Notes", 3000, "# Other", 1000, "## Notes", 3000),
			[]string{"f — Notes", "f — Notes > Notes", "f — Other", "f — Other > Notes"},
		},
		{
			// A heading that already reads like the counter's output keeps its
			// title; the counter skips the number it holds.
			"heading_looks_like_counter",
			doc("# Notes", 3000, "# Notes", 3000, "# Notes (2)", 3000),
			[]string{"f — Notes", "f — Notes (3)", "f — Notes (2)"},
		},
		{
			// A heading that already reads like a qualified title keeps it,
			// even after the section it would collide with: that section stays
			// unqualified rather than take the title away.
			"heading_looks_like_parent_path",
			doc("# Alpha", 1000, "## Notes", 2500, "# Beta", 1000, "## Notes", 2500, "# Alpha > Notes", 1000),
			[]string{"f — Alpha", "f — Notes", "f — Beta", "f — Beta > Notes", "f — Alpha > Notes"},
		},
		{
			// No heading repeats, but a heading reads like the part suffix of
			// an oversized section: the later chunk is numbered.
			"heading_looks_like_part_suffix",
			doc("# Big", 6000, 1000, "# Big (Teil 2)", 1000),
			[]string{"f — Big", "f — Big (Teil 2)", "f — Big (Teil 2) (2)"},
		},
		{
			// Case and trailing space make different bytes, so different
			// titles already; nothing changes.
			"case_and_space_differ",
			doc("## Notes", 3000, "## notes", 3000, "## Notes ", 3000),
			[]string{"f — Notes", "f — notes", "f — Notes "},
		},
		{
			// A file that fits one chunk has one title, repeated headings or not.
			"whole_document",
			doc("# Notes", 1000, "# Notes", 1000),
			[]string{"f"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := Chunk("f", tc.doc, nil)
			checkTitleInvariants(t, "f", chunks)
			if got := chunkTitles(chunks); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("titles\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The context header carries the same heading path as the title, so the
// embedded text of a repeated section names its chapter too.
func TestChunk_RepeatedHeadingHeader(t *testing.T) {
	chunks := Chunk("f", doc("# Alpha", 1000, "## Notes", 3000, "# Beta", 1000, "## Notes", 3000), []string{"t"})
	want := []string{
		"Datei: f | Sektion: Alpha",
		"Datei: f | Sektion: Alpha > Notes | Tags: t",
		"Datei: f | Sektion: Beta",
		"Datei: f | Sektion: Beta > Notes | Tags: t",
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks (%q), want %d", len(chunks), chunkTitles(chunks), len(want))
	}
	for i, c := range chunks {
		if c.Header != want[i] {
			t.Errorf("chunk %d header = %q, want %q", i, c.Header, want[i])
		}
	}
}

// A file in which no two chunks would share a title keeps its titles and
// headers byte for byte: a re-ingest must find the blocks it wrote before.
func TestChunk_TitlesUnchangedWithoutRepeatedHeading(t *testing.T) {
	cases := []struct {
		name          string
		doc           string
		titles, heads []string
	}{
		{
			"two_h1",
			doc("# Setup", 4000, "# Usage", 4000),
			[]string{"f — Setup", "f — Usage"},
			[]string{"Datei: f | Sektion: Setup | Tags: t", "Datei: f | Sektion: Usage | Tags: t"},
		},
		{
			"preamble_and_oversized",
			doc(1000, "# Big", 6000, 1000),
			[]string{"f", "f — Big", "f — Big (Teil 2)"},
			[]string{"Datei: f", "Datei: f | Sektion: Big | Tags: t", "Datei: f | Sektion: Big (Teil 2)"},
		},
		{
			// "Data" heads two sections, but the first one's opening part is
			// only heading lines and is dropped, so its chunk is "Data (Teil 2)"
			// and no two chunks share a title: nothing changes.
			"first_part_only_headings",
			doc("# Alpha", "## Data", -9000, "# Beta", "## Data", 1000),
			[]string{"f — Data (Teil 2)", "f — Data"},
			[]string{"Datei: f | Sektion: Data (Teil 2) | Tags: t", "Datei: f | Sektion: Data"},
		},
		{
			// The parts of one oversized H2 are one section, not a repeat:
			// they keep the plain heading, without the parent.
			"oversized_h2_under_h1",
			doc("# Guide", 1000, "## Big", 6000, 1000),
			[]string{"f — Guide", "f — Big", "f — Big (Teil 2)"},
			[]string{"Datei: f | Sektion: Guide", "Datei: f | Sektion: Big | Tags: t", "Datei: f | Sektion: Big (Teil 2)"},
		},
		{
			"no_headings_long",
			doc(6000, 3000),
			[]string{"f", "f —  (Teil 2)"},
			[]string{"Datei: f | Tags: t", "Datei: f | Sektion:  (Teil 2) | Tags: t"},
		},
		{
			// "Notes" stands twice in the text, but the small first one merges
			// into the next section and heads no chunk: one chunk is titled
			// Notes, so nothing collides and nothing changes.
			"repeat_merged_away",
			doc("# Alpha", 1000, "## Notes", 50, "# Beta", 1000, "## Notes", 4000, "# Gamma", 2500),
			[]string{"f — Alpha", "f — Beta", "f — Notes", "f — Gamma"},
			[]string{"Datei: f | Sektion: Alpha", "Datei: f | Sektion: Beta", "Datei: f | Sektion: Notes | Tags: t", "Datei: f | Sektion: Gamma | Tags: t"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := Chunk("f", tc.doc, []string{"t"})
			if len(chunks) != len(tc.titles) {
				t.Fatalf("got %d chunks (%q), want %d", len(chunks), chunkTitles(chunks), len(tc.titles))
			}
			for i, c := range chunks {
				if c.Title != tc.titles[i] {
					t.Errorf("chunk %d title = %q, want %q", i, c.Title, tc.titles[i])
				}
				if c.Header != tc.heads[i] {
					t.Errorf("chunk %d header = %q, want %q", i, c.Header, tc.heads[i])
				}
			}
		})
	}
}

// The parent in front would carry the title past MaxTitleBytes, which the
// server answers by refusing the whole request; the section is numbered
// instead, and the title stays as long as the unqualified one plus " (2)".
func TestChunk_RepeatedHeadingParentOverTitleLimit(t *testing.T) {
	name := strings.Repeat("d", 60) + "/" + strings.Repeat("e", 60) + "/" + strings.Repeat("f", 60) + "/README"
	parent := func(p string) string { return p + " " + strings.Repeat("p", 160) }
	notes := "Notes " + strings.Repeat("n", 140)
	chunks := Chunk(name, doc("# "+parent("Alpha"), 1000, "## "+notes, 3000, "# "+parent("Beta"), 1000, "## "+notes, 3000), nil)
	checkTitleInvariants(t, name, chunks)
	want := []string{parent("Alpha"), notes, parent("Beta"), notes + " (2)"}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks (%q), want %d", len(chunks), chunkTitles(chunks), len(want))
	}
	for i, c := range chunks {
		if c.HeadingPath != want[i] {
			t.Errorf("chunk %d heading path = %q, want %q", i, c.HeadingPath, want[i])
		}
	}
}

// The parent fits the title of a repeated section's first part but not that
// of its "(Teil 2)": the whole section keeps the plain heading, so its parts
// never read as two different sections.
func TestChunk_RepeatedHeadingPartsAgreeOnParent(t *testing.T) {
	name := strings.Repeat("x", MaxTitleBytes-20)
	chunks := Chunk(name, doc("# Alpha", "## Notes", 6000, 1000, "# Beta", "## Notes", 6000, 1000), nil)
	checkTitleInvariants(t, name, chunks)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks (%q), want 4", len(chunks), chunkTitles(chunks))
	}
	if q := len(buildTitle(name, "Alpha > Notes")); q > MaxTitleBytes {
		t.Fatalf("test setup: first part's qualified title is %d bytes, want it within %d", q, MaxTitleBytes)
	}
	for i, parent := range []string{"Alpha", "Alpha", "Beta", "Beta"} {
		if strings.HasPrefix(chunks[i].HeadingPath, parent+headingSep) {
			t.Errorf("chunk %d heading path = %q: the section's second part does not fit the parent, so no part may carry it", i, chunks[i].HeadingPath)
		}
	}
}

// A repeated heading whose title already sits at MaxTitleBytes: the counter
// would carry it over, so the heading is cut at a rune boundary to make room.
func TestChunk_RepeatedHeadingCounterAtTitleLimit(t *testing.T) {
	heading := "Notes zur Größe"
	name := strings.Repeat("x", MaxTitleBytes-len(" — ")-len(heading))
	if got := len(buildTitle(name, heading)); got != MaxTitleBytes {
		t.Fatalf("test setup: title is %d bytes, want %d", got, MaxTitleBytes)
	}
	chunks := Chunk(name, doc("# "+heading, 4000, "# "+heading, 4000), nil)
	checkTitleInvariants(t, name, chunks)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[0].HeadingPath != heading {
		t.Errorf("first chunk heading path = %q, want it unchanged %q", chunks[0].HeadingPath, heading)
	}
	if p := chunks[1].HeadingPath; !strings.HasSuffix(p, " (2)") || !strings.HasPrefix(heading, strings.TrimSuffix(p, " (2)")) {
		t.Errorf("second chunk heading path = %q, want a prefix of %q plus \" (2)\"", p, heading)
	}
}

// A filename that leaves less room than " (2)" before the limit: the counter
// takes the bare number as heading path, so with three repeats the title
// stays within MaxTitleBytes whenever the plain one was. The tenth repeat with
// one byte of room does not (TestChunk_TenthCounterPassesLimitWithOneByteOfRoom).
func TestChunk_RepeatedHeadingNameAtTitleLimit(t *testing.T) {
	for n := MaxTitleBytes - 12; n <= MaxTitleBytes-len(titleSep); n++ {
		name := strings.Repeat("x", n)
		chunks := Chunk(name, doc("# A", 4000, "# A", 4000, "# A", 4000), nil)
		if len(buildTitle(name, "A")) > MaxTitleBytes {
			continue // the plain title was over the limit already
		}
		checkTitleInvariants(t, name, chunks)
		if len(chunks) != 3 {
			t.Fatalf("name of %d bytes: got %d chunks, want 3", n, len(chunks))
		}
	}
}

// The limit numbered cannot keep (#318): the filename leaves one byte after
// " — ", so the heading "X" gives a 500-byte title, counters 2 to 9 fit as
// the bare number, and the tenth chunk's "10" makes a 501-byte title; the
// counter has no title within MaxTitleBytes for it, and `ctx ingest` holds
// the file back (internal/cli TestIngestHoldsBackFileOverServerLimit). This
// characterises numbered as it is and pins its doc comment: a numbered that
// finds a title within the limit for this chunk changes both.
func TestChunk_TenthCounterPassesLimitWithOneByteOfRoom(t *testing.T) {
	name := strings.Repeat("p", MaxTitleBytes-len(titleSep)-1)
	parts := make([]any, 0, 20)
	for range 10 {
		parts = append(parts, "## X", 800)
	}
	chunks := Chunk(name, doc(parts...), nil)
	if len(chunks) != 10 {
		t.Fatalf("got %d chunks, want 10: %q", len(chunks), chunkTitles(chunks))
	}
	for i, c := range chunks {
		want := MaxTitleBytes
		if i == 9 {
			want = MaxTitleBytes + 1
		}
		if len(c.Title) != want {
			t.Errorf("chunk %d: title is %d bytes, want %d (heading path %q)", i, len(c.Title), want, c.HeadingPath)
		}
	}
	if got, want := chunks[9].HeadingPath, "10"; got != want {
		t.Errorf("tenth chunk heading path = %q, want the bare counter %q", got, want)
	}
}

// numberRepeated remembers per path where to go on: m chunks with one title
// cost O(m), not O(m²) — a chat export holds thousands of "## User" sections.
// The bound is on the growth from 1000 to 8000 chunks (linear 8, quadratic
// 64), so it holds under -race and coverage alike.
func TestNumberRepeatedGrowsLinearly(t *testing.T) {
	cost := func(m int) time.Duration {
		best := time.Duration(math.MaxInt64)
		for range 5 {
			paths := make([]string, m)
			for i := range paths {
				paths[i] = "User"
			}
			taken := map[string]int{buildTitle("f", "User"): m}
			start := time.Now()
			numberRepeated("f", paths, taken)
			best = min(best, time.Since(start))
			if want := "User (" + itoa(m) + ")"; paths[m-1] != want {
				t.Fatalf("last of %d chunks has heading path %q, want %q", m, paths[m-1], want)
			}
		}
		return best
	}
	const passes = 3
	for pass := 1; pass <= passes; pass++ {
		small, large := cost(1000), cost(8000)
		ratio := float64(large) / float64(small)
		t.Logf("pass %d: %v for 1000 chunks with one title, %v for 8000 — growth %.1f (linear 8, quadratic 64)", pass, small, large, ratio)
		if ratio < 22 {
			return
		}
	}
	t.Errorf("numbering grows by 22 or more from 1000 to 8000 chunks with one title in each of %d passes, want linear", passes)
}

// plainTitles is what the file's titles were before #316: section heading
// plus part suffix, nil for the whole-document path.
func plainTitles(name, content string) []string {
	content = strings.TrimSpace(content)
	if len(content) <= MaxChunkChars {
		return nil
	}
	secs := filterEmpty(splitOversized(mergeSections(splitByHeadings(content))))
	out := make([]string, len(secs))
	for i, s := range secs {
		out[i] = buildTitle(name, s.heading+partSuffix(s.part))
	}
	return out
}

// Property: over many generated files — repeated, case-varied and
// suffix-shaped headings, both levels, preambles, empty sections, small
// sections that merge, big ones that split and single paragraphs too big to
// split, short and long names and headings — every file's chunk titles are
// pairwise distinct and within MaxTitleBytes, and a file whose plain titles
// were already distinct keeps them byte for byte.
func TestChunk_TitlesPairwiseDistinct(t *testing.T) {
	rng := rand.New(rand.NewPCG(316, 1))
	long := "Long heading " + strings.Repeat("h", 117)
	headings := []string{
		"Notes", "notes", "Notes ", "Notes (2)", "Notes (3)", "Notes (Teil 2)", "Notes > Notes",
		"Alpha", "Beta", "Alpha > Notes", "Überblick", "Überblick > Notes", long, "Alpha > " + long,
	}
	names := []string{"f", "docs/guide/README", strings.Repeat("deep/", 66)}
	sizes := []int{0, 50, 400, 2500, 6000, 9000, -9000} // 0: no body
	// The race stage runs -short, and under the detector 1000 files took 47 s
	// against 2.4 s without it; the full sweep runs without -short.
	files := 1000
	if testing.Short() {
		files = 200
	}
	for iter := range files {
		name := names[rng.IntN(len(names))]
		var parts []any
		if size := sizes[rng.IntN(len(sizes))]; size != 0 && rng.IntN(4) == 0 {
			parts = append(parts, size)
		}
		for range 2 + rng.IntN(7) {
			parts = append(parts, strings.Repeat("#", 1+rng.IntN(2))+" "+headings[rng.IntN(len(headings))])
			if size := sizes[rng.IntN(len(sizes))]; size != 0 {
				parts = append(parts, size)
			}
		}
		content := doc(parts...)
		chunks := Chunk(name, content, nil)
		checkTitleInvariants(t, name, chunks)
		if plain := plainTitles(name, content); len(plain) == len(chunks) && !hasDuplicate(plain) {
			if got := chunkTitles(chunks); strings.Join(got, "\n") != strings.Join(plain, "\n") {
				t.Errorf("no two plain titles were equal, yet titles changed\n got %q\nwant %q", got, plain)
			}
		}
		if t.Failed() {
			t.Fatalf("iteration %d, name %q, parts %v", iter, name, parts)
		}
	}
}

func hasDuplicate(titles []string) bool {
	seen := make(map[string]bool, len(titles))
	for _, s := range titles {
		if seen[s] {
			return true
		}
		seen[s] = true
	}
	return false
}
