package ingest

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ChunkResult represents a single chunk from a document.
type ChunkResult struct {
	Content     string // Cleaned text (WITHOUT header — header only for embedding prefix)
	Header      string // Context header for embedding prefix
	Title       string // Chunk title ("filename — Heading")
	Index       int    // Position in document (0-based)
	Total       int    // Total number of chunks from this document
	HeadingPath string // Title text after "filename — ": the section heading, with its parent H1 in front when the heading repeats (headingPaths)
}

// MaxChunkChars is the maximum content size per chunk (~1800 tokens).
const MaxChunkChars = 7200

// MinChunkChars is the minimum content size — smaller sections get merged.
const MinChunkChars = 200

// MaxTitleBytes is the longest title POST /api/ingest accepts (maxTitleLen in
// internal/handler/ingest.go, measured with len, so in bytes). A longer title
// makes the server refuse the whole request, the other files' chunks in it too.
const MaxTitleBytes = 500

// headingRe matches H1/H2 at line start.
var headingRe = regexp.MustCompile(`(?m)^(#{1,2})\s+(.+)$`)

// headingSep joins a parent H1 and a repeated heading in a heading path.
const headingSep = " > "

// section is an intermediate representation of a heading-delimited block.
type section struct {
	level   int    // 1 or 2 (0 = preamble before any heading)
	heading string // heading text without # prefix
	body    string // content after the heading line
	parent  string // heading of the H1 an H2 sits under ("" for an H1, the preamble, an H2 before any H1)
	origin  int    // position of the section before splitOversized; its parts share it
	part    int    // 1-based part number splitOversized gives (1 = whole or first part)
}

// Chunk splits a parsed document into chunks for embedding.
// filename: document name without extension (for title generation); the CLI
// passes the vault-relative path so same-named files in different folders
// get distinct titles
// content: cleaned markdown text (after Parse())
// tags: for context header
func Chunk(filename, content string, tags []string) []ChunkResult {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	// Whole-document: fits in single chunk
	if len(content) <= MaxChunkChars {
		return wholeDoc(filename, content, tags)
	}

	// Split by headings
	sections := splitByHeadings(content)

	// Merge small sections with their successor
	sections = mergeSections(sections)

	// Split oversized sections by paragraphs
	sections = splitOversized(sections)

	// Filter out empty sections (headings without content)
	sections = filterEmpty(sections)

	if len(sections) == 0 {
		return nil
	}

	// Build ChunkResults
	paths := headingPaths(filename, sections)
	results := make([]ChunkResult, len(sections))
	for i, sec := range sections {
		results[i] = ChunkResult{
			Content:     strings.TrimSpace(sec.body),
			Title:       buildTitle(filename, paths[i]),
			HeadingPath: paths[i],
			Index:       i,
		}
	}

	// Set Total on all chunks
	total := len(results)
	for i := range results {
		results[i].Total = total
		results[i].Header = buildHeader(filename, results[i], tags)
	}

	return results
}

// wholeDoc returns a single chunk for content that fits within MaxChunkChars.
// Also handles the headings-only edge case.
func wholeDoc(filename, content string, tags []string) []ChunkResult {
	// Check if there's any meaningful non-heading content
	stripped := headingRe.ReplaceAllString(content, "")
	stripped = strings.TrimSpace(stripped)
	if len(stripped) < MinChunkChars {
		// Only headings or negligible content
		return nil
	}

	c := ChunkResult{
		Content: content,
		Title:   filename,
		Index:   0,
		Total:   1,
	}
	c.Header = buildHeader(filename, c, tags)
	return []ChunkResult{c}
}

// splitByHeadings splits content at H1/H2 boundaries.
func splitByHeadings(content string) []section {
	locs := headingRe.FindAllStringIndex(content, -1)
	matches := headingRe.FindAllStringSubmatch(content, -1)

	if len(locs) == 0 {
		// No headings — return as single section for paragraph splitting
		return []section{{level: 0, heading: "", body: content}}
	}

	var sections []section

	// Preamble before first heading
	if locs[0][0] > 0 {
		pre := strings.TrimSpace(content[:locs[0][0]])
		if pre != "" {
			sections = append(sections, section{level: 0, heading: "", body: pre})
		}
	}

	h1 := "" // the H1 the following H2 sections sit under
	for i, loc := range locs {
		level := len(matches[i][1]) // number of # chars
		heading := matches[i][2]
		parent := h1
		if level == 1 {
			parent, h1 = "", heading
		}

		// Body: from end of heading line to start of next heading (or EOF)
		bodyStart := loc[1]
		var bodyEnd int
		if i+1 < len(locs) {
			bodyEnd = locs[i+1][0]
		} else {
			bodyEnd = len(content)
		}

		body := content[bodyStart:bodyEnd]
		sections = append(sections, section{
			level:   level,
			heading: heading,
			body:    strings.TrimSpace(body),
			parent:  parent,
		})
	}

	return sections
}

// mergeSections merges sections smaller than MinChunkChars with their successor.
func mergeSections(sections []section) []section {
	if len(sections) <= 1 {
		return sections
	}

	var merged []section
	i := 0
	for i < len(sections) {
		sec := sections[i]
		bodyLen := len(strings.TrimSpace(sec.body))

		// If this section is small, merge with the next one
		if bodyLen < MinChunkChars && i+1 < len(sections) {
			next := sections[i+1]
			combinedBody := sec.body
			if sec.heading != "" {
				combinedBody = "## " + sec.heading + "\n\n" + sec.body
			}
			combinedBody = combinedBody + "\n\n"
			if next.heading != "" {
				combinedBody = combinedBody + "## " + next.heading + "\n\n"
			}
			combinedBody = combinedBody + next.body

			// Use the next section's heading as the representative
			mergedSec := section{
				level:   next.level,
				heading: next.heading,
				body:    strings.TrimSpace(combinedBody),
				parent:  next.parent,
			}
			// Replace next with merged
			sections[i+1] = mergedSec
			i++
			continue
		}

		merged = append(merged, sec)
		i++
	}

	return merged
}

// splitOversized splits sections exceeding MaxChunkChars by paragraph boundaries.
// Every result carries the position of the section it came from (origin) and
// its part number; partSuffix turns parts 2.. into " (Teil N)" in the title.
func splitOversized(sections []section) []section {
	var result []section
	for origin, sec := range sections {
		sec.origin, sec.part = origin, 1
		if len(sec.body) <= MaxChunkChars {
			result = append(result, sec)
			continue
		}

		// Split by double newlines (paragraphs)
		paragraphs := splitParagraphs(sec.body)
		var current strings.Builder

		for _, para := range paragraphs {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}

			// Would adding this paragraph exceed the limit?
			if current.Len() > 0 && current.Len()+len(para)+2 > MaxChunkChars {
				// Emit current
				part := sec
				part.body = strings.TrimSpace(current.String())
				result = append(result, part)
				sec.part++
				current.Reset()
			}

			if current.Len() > 0 {
				current.WriteString("\n\n")
			}
			current.WriteString(para)
		}

		// Emit remainder
		if current.Len() > 0 {
			sec.body = strings.TrimSpace(current.String())
			result = append(result, sec)
		}
	}
	return result
}

// partSuffix names part 2.. of a section splitOversized cut into several.
func partSuffix(part int) string {
	if part <= 1 {
		return ""
	}
	return " (Teil " + itoa(part) + ")"
}

// splitParagraphs splits text at double newlines.
func splitParagraphs(text string) []string {
	// Normalize line endings
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(text, "\n\n")
}

// filterEmpty removes sections with no meaningful content.
func filterEmpty(sections []section) []section {
	var result []section
	for _, sec := range sections {
		body := strings.TrimSpace(sec.body)
		if body == "" {
			continue
		}
		// Check if body is only heading markers (no real content)
		stripped := headingRe.ReplaceAllString(body, "")
		stripped = strings.TrimSpace(stripped)
		if stripped == "" {
			continue
		}
		result = append(result, sec)
	}
	return result
}

// titleSep joins the document name and the heading path in a title.
const titleSep = " — "

// buildTitle generates a chunk title from filename and heading.
func buildTitle(filename, heading string) string {
	if heading == "" {
		return filename
	}
	return filename + titleSep + heading
}

// headingPaths returns the heading path of every chunk, the text its title
// carries after "<filename> — ". Block-mode ingest upserts by
// (category, title, scope), so two chunks of one file with one title would
// share a block and the later one would replace the earlier (#316).
//
// The path is the section heading plus its part suffix, as it always was,
// unless that leaves two chunks with one title:
//
//   - A section that shares a title with another chunk, and whose heading
//     heads more than one section of the file, takes its parent H1 in front
//     ("Alpha > Notes") on every one of its parts. That title does not depend
//     on how many sections come before it. The section keeps its plain
//     heading when the parent would carry a title past MaxTitleBytes, or when
//     another chunk carries the result as its own title.
//   - A title that still repeats (an H1 heading twice, the same H2 twice
//     under one H1, a heading that reads like a part suffix) gets " (N)" on
//     its second and later chunk, with the first N no chunk title holds.
//
// A file in which no two chunks would share a title keeps every title and
// header byte for byte, so a re-ingest finds the blocks it wrote before.
func headingPaths(filename string, secs []section) []string {
	paths := make([]string, len(secs))
	taken := make(map[string]int, len(secs)) // chunks per title
	origins := make(map[string]map[int]bool)
	for i, s := range secs {
		paths[i] = s.heading + partSuffix(s.part)
		taken[buildTitle(filename, paths[i])]++
		if origins[s.heading] == nil {
			origins[s.heading] = make(map[int]bool)
		}
		origins[s.heading][s.origin] = true
	}
	qualifyRepeated(filename, secs, paths, origins, taken)
	numberRepeated(filename, paths, taken)
	return paths
}

// qualifyRepeated puts the parent H1 in front of the path of every part of a
// section that shares a title with another chunk and whose heading heads more
// than one section (see headingPaths). It decides per section, so all parts of
// one section agree. taken counts the chunks per plain title; it gains the
// qualified titles.
func qualifyRepeated(filename string, secs []section, paths []string, origins map[string]map[int]bool, taken map[string]int) {
	repeated := func(s section) bool { return s.parent != "" && len(origins[s.heading]) > 1 }
	qualified := func(s section) string { return s.parent + headingSep + s.heading + partSuffix(s.part) }
	shared := make(map[int]bool) // sections with a chunk whose plain title another chunk carries
	plain := make(map[int]bool)  // sections the parent must not go in front of
	for i, s := range secs {
		if taken[buildTitle(filename, paths[i])] > 1 {
			shared[s.origin] = true
		}
		if !repeated(s) {
			continue
		}
		if t := buildTitle(filename, qualified(s)); len(t) > MaxTitleBytes || taken[t] > 0 {
			plain[s.origin] = true
		}
	}
	for i, s := range secs {
		if repeated(s) && shared[s.origin] && !plain[s.origin] {
			paths[i] = qualified(s)
			taken[buildTitle(filename, paths[i])]++
		}
	}
}

// numberRepeated gives the second and later chunk of a title that still
// repeats a " (N)" counter, with the first N whose title is not taken. It
// remembers per path where to go on, so m chunks with one title cost O(m):
// a chat export holds thousands of "## User" sections.
func numberRepeated(filename string, paths []string, taken map[string]int) {
	seen := make(map[string]bool, len(paths))
	next := make(map[string]int) // per path: every counter below it is taken
	for i, p := range paths {
		if t := buildTitle(filename, p); !seen[t] {
			seen[t] = true
			continue
		}
		for n := max(next[p], 2); ; n++ {
			cand := numbered(filename, p, n)
			if t := buildTitle(filename, cand); taken[t] == 0 {
				paths[i], next[p] = cand, n+1
				taken[t]++
				seen[t] = true
				break
			}
		}
	}
}

// numbered returns path with the counter n as " (n)", within MaxTitleBytes:
// path is cut at a rune boundary to make room for the counter, and a
// filename that leaves less room than " (n)" takes the bare number as its
// heading path. Only a filename that leaves less room than the number itself
// stays over the limit; its plain title with a heading of that length did too.
func numbered(filename, path string, n int) string {
	suffix := " (" + itoa(n) + ")"
	room := MaxTitleBytes - len(filename) - len(titleSep)
	switch {
	case len(path)+len(suffix) <= room:
		return path + suffix
	case len(suffix) <= room:
		cut := room - len(suffix)
		for cut > 0 && !utf8.RuneStart(path[cut]) {
			cut--
		}
		return path[:cut] + suffix
	default:
		return itoa(n)
	}
}

// buildHeader generates a context header scaled by chunk size.
func buildHeader(filename string, chunk ChunkResult, tags []string) string {
	contentLen := len(chunk.Content)

	// Short: <500 chars → only filename
	if contentLen < 500 {
		return "Datei: " + filename
	}

	// Medium: 500-2000 → filename + section
	if contentLen <= 2000 {
		h := "Datei: " + filename
		if chunk.HeadingPath != "" {
			h += " | Sektion: " + chunk.HeadingPath
		}
		return h
	}

	// Long: >2000 → full header with tags
	h := "Datei: " + filename
	if chunk.HeadingPath != "" {
		h += " | Sektion: " + chunk.HeadingPath
	}
	if len(tags) > 0 {
		h += " | Tags: " + strings.Join(tags, ", ")
	}
	return h
}

// itoa converts int to string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
