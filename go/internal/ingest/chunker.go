package ingest

import (
	"regexp"
	"strings"
)

// ChunkResult represents a single chunk from a document.
type ChunkResult struct {
	Content     string // Cleaned text (WITHOUT header — header only for embedding prefix)
	Header      string // Context header for embedding prefix
	Title       string // Chunk title ("filename — Heading")
	Index       int    // Position in document (0-based)
	Total       int    // Total number of chunks from this document
	HeadingPath string // H1 > H2 path of the section
}

// MaxChunkChars is the maximum content size per chunk (~1800 tokens).
const MaxChunkChars = 7200

// MinChunkChars is the minimum content size — smaller sections get merged.
const MinChunkChars = 200

// headingRe matches H1/H2 at line start.
var headingRe = regexp.MustCompile(`(?m)^(#{1,2})\s+(.+)$`)

// section is an intermediate representation of a heading-delimited block.
type section struct {
	level   int    // 1 or 2 (0 = preamble before any heading)
	heading string // heading text without # prefix
	body    string // content after the heading line
}

// Chunk splits a parsed document into chunks for embedding.
// filename: basename without extension (for title generation)
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
	results := make([]ChunkResult, len(sections))
	for i, sec := range sections {
		results[i] = ChunkResult{
			Content:     strings.TrimSpace(sec.body),
			Title:       buildTitle(filename, sec.heading),
			HeadingPath: sec.heading,
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

	for i, loc := range locs {
		level := len(matches[i][1]) // number of # chars
		heading := matches[i][2]

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
func splitOversized(sections []section) []section {
	var result []section
	for _, sec := range sections {
		if len(sec.body) <= MaxChunkChars {
			result = append(result, sec)
			continue
		}

		// Split by double newlines (paragraphs)
		paragraphs := splitParagraphs(sec.body)
		var current strings.Builder
		partNum := 0

		for _, para := range paragraphs {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}

			// Would adding this paragraph exceed the limit?
			if current.Len() > 0 && current.Len()+len(para)+2 > MaxChunkChars {
				// Emit current
				partNum++
				heading := sec.heading
				if partNum > 1 {
					heading = sec.heading + " (Teil " + itoa(partNum) + ")"
				}
				result = append(result, section{
					level:   sec.level,
					heading: heading,
					body:    strings.TrimSpace(current.String()),
				})
				current.Reset()
			}

			if current.Len() > 0 {
				current.WriteString("\n\n")
			}
			current.WriteString(para)
		}

		// Emit remainder
		if current.Len() > 0 {
			partNum++
			heading := sec.heading
			if partNum > 1 {
				heading = sec.heading + " (Teil " + itoa(partNum) + ")"
			}
			result = append(result, section{
				level:   sec.level,
				heading: heading,
				body:    strings.TrimSpace(current.String()),
			})
		}
	}
	return result
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

// buildTitle generates a chunk title from filename and heading.
func buildTitle(filename, heading string) string {
	if heading == "" {
		return filename
	}
	return filename + " — " + heading
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
