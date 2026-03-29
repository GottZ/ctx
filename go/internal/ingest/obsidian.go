// Package ingest — Obsidian Markdown Parser
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Parses Obsidian Markdown files: frontmatter, wikilinks, embeds,
// inline tags, comments, dataview, highlights, callouts, templates.
// No external YAML or Markdown dependencies — regex-based.
//
// Source: https://github.com/GottZ/ctx
package ingest

import (
	"regexp"
	"strings"

	"github.com/GottZ/ctx/internal/store"
)

// ParseResult holds all extracted metadata from an Obsidian Markdown file.
type ParseResult struct {
	Title      string         // Filename oder Frontmatter-Override
	Content    string         // Bereinigter Markdown-Text
	Tags       []string       // Merged: Frontmatter + Inline (dedupliziert)
	Links      []string       // [[Wikilink]] Targets
	Embeds     []string       // ![[Embed]] Targets
	Dates      []string       // ISO-Daten aus Frontmatter + Content
	Aliases    []string       // Frontmatter aliases
	Category   string         // Aus Ordner-Pfad (Top-Level)
	Metadata   map[string]any // Frontmatter Catch-all
	VaultPath  string         // Relativer Pfad im Vault
	IsTemplate bool           // Templater-Detection (>3x <%)
}

// Compiled patterns
var (
	frontmatterRe = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n?`)
	embedRe       = regexp.MustCompile(`!\[\[([^\]]+)\]\]`)
	wikilinkRe    = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	commentRe     = regexp.MustCompile(`(?s)%%.*?%%`)
	dataviewRe    = regexp.MustCompile("(?s)```dataview\\n.*?```")
	highlightRe   = regexp.MustCompile(`==([^=]+)==`)
	calloutMarkRe = regexp.MustCompile(`(?m)^> \[![^\]]*\]\s*`)
	calloutContRe = regexp.MustCompile(`(?m)^> `)
	templaterRe   = regexp.MustCompile(`<%`)
	codeBlockRe   = regexp.MustCompile("(?s)```.*?```")
	codeSpanRe    = regexp.MustCompile("`[^`]+`")
	inlineTagRe   = regexp.MustCompile(`(?:^|[ \t])#([a-zA-Z_][a-zA-Z0-9_/]*)`)
	yamlArrayRe   = regexp.MustCompile(`^\[(.+)\]$`)
)

// Parse extracts metadata from an Obsidian Markdown file.
// filename: basename ohne .md Extension
// content: raw file bytes
// vaultPath: relativer Pfad im Vault (z.B. "projects/ctx/note.md")
func Parse(filename string, content []byte, vaultPath string) *ParseResult {
	r := &ParseResult{
		Title:     filename,
		Tags:      []string{},
		Links:     []string{},
		Embeds:    []string{},
		Dates:     []string{},
		Aliases:   []string{},
		Metadata:  make(map[string]any),
		VaultPath: vaultPath,
	}

	// Category from path
	r.Category = categoryFromPath(vaultPath)

	text := string(content)
	if len(text) == 0 {
		r.Content = ""
		return r
	}

	// Stage 1: Normalize line endings, strip UTF-8 BOM
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")

	// Stage 2: Extract frontmatter
	fmDates := parseFrontmatter(text, r)
	if m := frontmatterRe.FindStringIndex(text); m != nil {
		text = text[m[1]:]
	}

	// Stage 3: Template detection (on body before cleanup)
	matches := templaterRe.FindAllStringIndex(text, -1)
	if len(matches) > 3 {
		r.IsTemplate = true
	}

	// Stage 4: Remove comments (before wikilink/tag extraction)
	text = commentRe.ReplaceAllString(text, "")

	// Stage 5: Remove dataview blocks
	text = dataviewRe.ReplaceAllString(text, "")

	// Stage 6: Mask code blocks and code spans for link/tag extraction
	// We replace them with placeholders, extract links/tags, then restore
	var codeBlocks []string
	text = codeBlockRe.ReplaceAllStringFunc(text, func(m string) string {
		idx := len(codeBlocks)
		codeBlocks = append(codeBlocks, m)
		return placeholderBlock(idx)
	})
	var codeSpans []string
	text = codeSpanRe.ReplaceAllStringFunc(text, func(m string) string {
		idx := len(codeSpans)
		codeSpans = append(codeSpans, m)
		return placeholderSpan(idx)
	})

	// Stage 7: Extract embeds (before wikilinks, since ![[x]] contains [[x]])
	for _, m := range embedRe.FindAllStringSubmatch(text, -1) {
		target := m[1]
		// Strip heading/block refs
		if i := strings.Index(target, "#"); i >= 0 {
			target = target[:i]
		}
		if i := strings.Index(target, "|"); i >= 0 {
			target = target[:i]
		}
		target = strings.TrimSpace(target)
		if target != "" {
			r.Embeds = appendUnique(r.Embeds, target)
		}
	}
	// Remove embeds from text
	text = embedRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[3 : len(m)-2] // strip ![[...]]
		if i := strings.Index(inner, "|"); i >= 0 {
			return inner[i+1:]
		}
		if i := strings.Index(inner, "#"); i >= 0 {
			return inner[:i]
		}
		return inner
	})

	// Stage 8: Extract wikilinks
	for _, m := range wikilinkRe.FindAllStringSubmatch(text, -1) {
		raw := m[1]
		target := raw
		if i := strings.Index(target, "|"); i >= 0 {
			target = target[:i]
		}
		if i := strings.Index(target, "#"); i >= 0 {
			target = target[:i]
		}
		target = strings.TrimSpace(target)
		if target != "" {
			r.Links = appendUnique(r.Links, target)
		}
	}
	// Replace wikilinks with display text
	text = wikilinkRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[2 : len(m)-2] // strip [[...]]
		if i := strings.Index(inner, "|"); i >= 0 {
			return inner[i+1:]
		}
		return inner
	})

	// Stage 9: Extract inline tags
	// Must happen after code masking but on text with wikilinks already resolved
	for _, m := range inlineTagRe.FindAllStringSubmatch(text, -1) {
		tag := m[1]
		r.Tags = appendUnique(r.Tags, tag)
	}
	// Remove inline tags from text (but keep # at line start as headings)
	text = replaceInlineTags(text)

	// Stage 10: Strip highlight markers
	text = highlightRe.ReplaceAllString(text, "$1")

	// Stage 11: Normalize callouts (strip marker, keep content)
	text = calloutMarkRe.ReplaceAllString(text, "")
	text = calloutContRe.ReplaceAllString(text, "")

	// Stage 12: Restore code blocks and spans
	for i, cb := range codeBlocks {
		text = strings.Replace(text, placeholderBlock(i), cb, 1)
	}
	for i, cs := range codeSpans {
		text = strings.Replace(text, placeholderSpan(i), cs, 1)
	}

	// Merge frontmatter tags with inline tags (already appended, just dedup)
	// Tags from frontmatter were already added during parseFrontmatter

	// Stage 13: Extract dates from content
	contentDates := store.ExtractDateStrings(text)
	allDates := make(map[string]bool)
	for _, d := range fmDates {
		allDates[d] = true
	}
	for _, d := range contentDates {
		allDates[d] = true
	}
	r.Dates = make([]string, 0, len(allDates))
	for d := range allDates {
		r.Dates = append(r.Dates, d)
	}
	// Sort dates
	sortStrings(r.Dates)

	// Collapse excessive whitespace
	text = collapseWhitespace(text)

	r.Content = text
	return r
}

// parseFrontmatter extracts YAML frontmatter and populates the result.
// Returns any dates found in frontmatter fields.
func parseFrontmatter(text string, r *ParseResult) []string {
	var fmDates []string

	m := frontmatterRe.FindStringSubmatch(text)
	if m == nil {
		return fmDates
	}

	yaml := m[1]
	lines := strings.Split(yaml, "\n")

	var currentKey string
	var listItems []string
	inList := false

	flushList := func() {
		if !inList || currentKey == "" {
			return
		}
		switch currentKey {
		case "tags":
			for _, item := range listItems {
				r.Tags = appendUnique(r.Tags, item)
			}
		case "aliases":
			r.Aliases = append(r.Aliases, listItems...)
		}
		inList = false
		listItems = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Dash-list continuation
		if strings.HasPrefix(trimmed, "- ") && inList {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			val = unquoteYAML(val)
			listItems = append(listItems, val)
			continue
		}

		// Flush any pending list
		flushList()

		// Key: value pair
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])

		if key == "" {
			continue
		}
		currentKey = key

		// Empty value = start of dash-list
		if value == "" {
			inList = true
			listItems = nil
			continue
		}

		// Inline array [a, b, c]
		if am := yamlArrayRe.FindStringSubmatch(value); am != nil {
			items := parseInlineArray(am[1])
			switch key {
			case "tags":
				for _, item := range items {
					r.Tags = appendUnique(r.Tags, item)
				}
			case "aliases":
				r.Aliases = append(r.Aliases, items...)
			default:
				r.Metadata[key] = items
			}
			continue
		}

		// Scalar value
		value = unquoteYAML(value)
		switch key {
		case "title":
			r.Title = value
		case "tags":
			// Single tag as string
			r.Tags = appendUnique(r.Tags, value)
		case "aliases":
			r.Aliases = append(r.Aliases, value)
		case "date", "created", "modified":
			// Try to extract date
			dates := store.ExtractDateStrings(value)
			fmDates = append(fmDates, dates...)
			r.Metadata[key] = value
		default:
			r.Metadata[key] = value
		}
	}
	flushList()

	return fmDates
}

// parseInlineArray parses "a, b, c" from YAML inline array notation.
func parseInlineArray(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		v = unquoteYAML(v)
		if v != "" {
			result = append(result, v)
		}
	}
	return result
}

// unquoteYAML strips surrounding quotes from YAML values.
func unquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// categoryFromPath extracts the top-level directory from a vault path.
func categoryFromPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) <= 1 {
		return "uncategorized"
	}
	return parts[0]
}

// replaceInlineTags removes inline tags from text (not headings).
func replaceInlineTags(text string) string {
	// Replace tags preceded by whitespace or at start of line content
	// but NOT # at start of line (headings)
	return inlineTagRe.ReplaceAllStringFunc(text, func(m string) string {
		// Keep leading whitespace
		if len(m) > 0 && (m[0] == ' ' || m[0] == '\t') {
			return string(m[0])
		}
		return ""
	})
}

// placeholderBlock returns a code block placeholder.
func placeholderBlock(idx int) string {
	return "\x00CB" + string(rune(idx+'0')) + "\x00"
}

// placeholderSpan returns a code span placeholder.
func placeholderSpan(idx int) string {
	return "\x00CS" + string(rune(idx+'0')) + "\x00"
}

// appendUnique adds s to slice only if not already present.
func appendUnique(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}

// sortStrings sorts a string slice in place.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// collapseWhitespace reduces multiple blank lines to at most one.
func collapseWhitespace(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	blankCount := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blankCount++
			if blankCount <= 1 {
				result = append(result, line)
			}
		} else {
			blankCount = 0
			result = append(result, line)
		}
	}
	// Trim leading/trailing blank lines
	for len(result) > 0 && strings.TrimSpace(result[0]) == "" {
		result = result[1:]
	}
	for len(result) > 0 && strings.TrimSpace(result[len(result)-1]) == "" {
		result = result[:len(result)-1]
	}
	return strings.Join(result, "\n")
}
