// Package ingest — Obsidian Markdown Parser Tests (TDD: written before implementation)
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Source: https://github.com/GottZ/ctx
package ingest

import (
	"sort"
	"strings"
	"testing"
)

// --- helpers ---

func assertStringEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func assertStringSlice(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s length = %d, want %d; got %v", field, len(got), len(want), got)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", field, i, got[i], want[i])
		}
	}
}

func assertContains(t *testing.T, field, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s should contain %q, got %q", field, needle, haystack)
	}
}

func assertNotContains(t *testing.T, field, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s should NOT contain %q, got %q", field, needle, haystack)
	}
}

func assertBool(t *testing.T, field string, got, want bool) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

func sortedCopy(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}

// =============================================================================
// Frontmatter
// =============================================================================

func TestParse_Frontmatter_Tags(t *testing.T) {
	input := "---\ntags: [alpha, beta]\n---\nSome content."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Tags", sortedCopy(r.Tags), []string{"alpha", "beta"})
}

func TestParse_Frontmatter_Date(t *testing.T) {
	input := "---\ndate: 2026-03-15\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	found := false
	for _, d := range r.Dates {
		if d == "2026-03-15" {
			found = true
		}
	}
	if !found {
		t.Errorf("Dates should contain 2026-03-15, got %v", r.Dates)
	}
}

func TestParse_Frontmatter_Title(t *testing.T) {
	input := "---\ntitle: Override Title\n---\nContent."
	r := Parse("filename", []byte(input), "notes/filename.md")
	assertStringEqual(t, "Title", r.Title, "Override Title")
}

func TestParse_Frontmatter_Aliases(t *testing.T) {
	input := "---\naliases: [shortname, alt]\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Aliases", r.Aliases, []string{"shortname", "alt"})
}

func TestParse_Frontmatter_CustomFields(t *testing.T) {
	input := "---\nfoo: bar\nstatus: draft\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	if r.Metadata["foo"] != "bar" {
		t.Errorf("Metadata[foo] = %v, want bar", r.Metadata["foo"])
	}
	if r.Metadata["status"] != "draft" {
		t.Errorf("Metadata[status] = %v, want draft", r.Metadata["status"])
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	input := "Just some content without frontmatter."
	r := Parse("note", []byte(input), "notes/note.md")
	assertContains(t, "Content", r.Content, "Just some content")
	assertStringEqual(t, "Title", r.Title, "note")
}

// Frontmatter: dash-list format for tags
func TestParse_Frontmatter_Tags_DashList(t *testing.T) {
	input := "---\ntags:\n- alpha\n- beta\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Tags", sortedCopy(r.Tags), []string{"alpha", "beta"})
}

// Frontmatter: created field as date source
func TestParse_Frontmatter_CreatedDate(t *testing.T) {
	input := "---\ncreated: 2026-01-10\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	found := false
	for _, d := range r.Dates {
		if d == "2026-01-10" {
			found = true
		}
	}
	if !found {
		t.Errorf("Dates should contain 2026-01-10, got %v", r.Dates)
	}
}

// =============================================================================
// Wikilinks
// =============================================================================

func TestParse_Wikilinks(t *testing.T) {
	input := "See [[Some Page]] for details."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Links", r.Links, []string{"Some Page"})
	assertContains(t, "Content", r.Content, "Some Page")
	assertNotContains(t, "Content", r.Content, "[[")
}

func TestParse_Wikilinks_WithAlias(t *testing.T) {
	input := "See [[Some Page|the alias]] for details."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Links", r.Links, []string{"Some Page"})
	assertContains(t, "Content", r.Content, "the alias")
	assertNotContains(t, "Content", r.Content, "[[")
}

func TestParse_Wikilinks_WithHeading(t *testing.T) {
	input := "See [[Page#Section]] here."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Links", r.Links, []string{"Page"})
}

func TestParse_Embeds(t *testing.T) {
	input := "Image: ![[photo.png]] here."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Embeds", r.Embeds, []string{"photo.png"})
	assertNotContains(t, "Content", r.Content, "![[")
}

func TestParse_Wikilinks_InCodeBlock(t *testing.T) {
	input := "```\n[[not a link]]\n```\nOutside."
	r := Parse("note", []byte(input), "notes/note.md")
	if len(r.Links) != 0 {
		t.Errorf("Links should be empty in code blocks, got %v", r.Links)
	}
}

// =============================================================================
// Tags
// =============================================================================

func TestParse_InlineTags(t *testing.T) {
	input := "Some text #tag1 more text #tag2 end."
	r := Parse("note", []byte(input), "notes/note.md")
	sorted := sortedCopy(r.Tags)
	assertStringSlice(t, "Tags", sorted, []string{"tag1", "tag2"})
}

func TestParse_InlineTags_Nested(t *testing.T) {
	input := "Nested #parent/child tag."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringSlice(t, "Tags", r.Tags, []string{"parent/child"})
}

func TestParse_InlineTags_NotHeading(t *testing.T) {
	input := "# Heading\nContent with no tags."
	r := Parse("note", []byte(input), "notes/note.md")
	if len(r.Tags) != 0 {
		t.Errorf("Tags should be empty (# is heading, not tag), got %v", r.Tags)
	}
}

func TestParse_Tags_Merged(t *testing.T) {
	input := "---\ntags: [alpha, beta]\n---\nText #beta #gamma."
	r := Parse("note", []byte(input), "notes/note.md")
	sorted := sortedCopy(r.Tags)
	// alpha, beta (from FM), gamma (inline) — beta dedupliziert
	assertStringSlice(t, "Tags", sorted, []string{"alpha", "beta", "gamma"})
}

// =============================================================================
// Cleanup
// =============================================================================

func TestParse_Comments_Removed(t *testing.T) {
	input := "Before %%hidden comment%% after."
	r := Parse("note", []byte(input), "notes/note.md")
	assertNotContains(t, "Content", r.Content, "hidden comment")
	assertNotContains(t, "Content", r.Content, "%%")
	assertContains(t, "Content", r.Content, "Before")
	assertContains(t, "Content", r.Content, "after")
}

func TestParse_Dataview_Removed(t *testing.T) {
	input := "Before\n```dataview\nTABLE file.name\nFROM #tag\n```\nAfter."
	r := Parse("note", []byte(input), "notes/note.md")
	assertNotContains(t, "Content", r.Content, "TABLE")
	assertNotContains(t, "Content", r.Content, "dataview")
	assertContains(t, "Content", r.Content, "Before")
	assertContains(t, "Content", r.Content, "After")
}

func TestParse_Templater_Detected(t *testing.T) {
	input := "<% tp.date %> <% tp.file %> <% tp.web %> <% tp.system %>"
	r := Parse("note", []byte(input), "notes/note.md")
	assertBool(t, "IsTemplate", r.IsTemplate, true)
}

func TestParse_Highlights_Stripped(t *testing.T) {
	input := "This is ==highlighted text== in a sentence."
	r := Parse("note", []byte(input), "notes/note.md")
	assertContains(t, "Content", r.Content, "highlighted text")
	assertNotContains(t, "Content", r.Content, "==")
}

func TestParse_Callout_Preserved(t *testing.T) {
	input := "> [!note] Important\n> This is callout content.\n> Second line."
	r := Parse("note", []byte(input), "notes/note.md")
	assertContains(t, "Content", r.Content, "This is callout content")
	assertContains(t, "Content", r.Content, "Second line")
}

// =============================================================================
// Edge Cases
// =============================================================================

func TestParse_EmptyContent(t *testing.T) {
	r := Parse("empty", []byte(""), "notes/empty.md")
	assertStringEqual(t, "Title", r.Title, "empty")
	assertStringEqual(t, "Content", r.Content, "")
}

func TestParse_FrontmatterOnly(t *testing.T) {
	input := "---\ntitle: Only Meta\ntags: [a]\n---\n"
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringEqual(t, "Title", r.Title, "Only Meta")
	assertStringEqual(t, "Content", strings.TrimSpace(r.Content), "")
}

func TestParse_CRLF(t *testing.T) {
	input := "---\r\ntitle: Win\r\n---\r\nContent line.\r\n"
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringEqual(t, "Title", r.Title, "Win")
	assertNotContains(t, "Content", r.Content, "\r")
}

func TestParse_UTF8BOM(t *testing.T) {
	input := "\xef\xbb\xbf---\ntitle: BOM Test\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	assertStringEqual(t, "Title", r.Title, "BOM Test")
}

func TestParse_CategoryFromPath(t *testing.T) {
	r := Parse("note", []byte("Content."), "projects/ctx/note.md")
	assertStringEqual(t, "Category", r.Category, "projects")
}

func TestParse_CategoryFromPath_RootFile(t *testing.T) {
	r := Parse("note", []byte("Content."), "note.md")
	assertStringEqual(t, "Category", r.Category, "uncategorized")
}

// =============================================================================
// Dates
// =============================================================================

func TestParse_DatesFromContent(t *testing.T) {
	input := "Am 2026-03-15 passierte etwas wichtiges."
	r := Parse("note", []byte(input), "notes/note.md")
	found := false
	for _, d := range r.Dates {
		if d == "2026-03-15" {
			found = true
		}
	}
	if !found {
		t.Errorf("Dates should contain 2026-03-15, got %v", r.Dates)
	}
}

func TestParse_DatesFromFrontmatter(t *testing.T) {
	input := "---\ncreated: 2026-01-01\ndate: 2026-02-15\n---\nContent."
	r := Parse("note", []byte(input), "notes/note.md")
	sorted := sortedCopy(r.Dates)
	assertStringSlice(t, "Dates", sorted, []string{"2026-01-01", "2026-02-15"})
}

// =============================================================================
// Additional edge cases
// =============================================================================

func TestParse_MultipleWikilinks(t *testing.T) {
	input := "See [[Page A]] and [[Page B]] and [[Page A]] again."
	r := Parse("note", []byte(input), "notes/note.md")
	// Links should be deduplicated
	sorted := sortedCopy(r.Links)
	assertStringSlice(t, "Links", sorted, []string{"Page A", "Page B"})
}

func TestParse_Templater_NotDetected_Normal(t *testing.T) {
	input := "Normal note with some <angle> brackets and <% one template %> tag."
	r := Parse("note", []byte(input), "notes/note.md")
	assertBool(t, "IsTemplate", r.IsTemplate, false)
}

func TestParse_VaultPath(t *testing.T) {
	r := Parse("note", []byte("Content."), "projects/ctx/note.md")
	assertStringEqual(t, "VaultPath", r.VaultPath, "projects/ctx/note.md")
}

func TestParse_MultilineComment(t *testing.T) {
	input := "Before\n%%\nmultiline\ncomment\n%%\nAfter."
	r := Parse("note", []byte(input), "notes/note.md")
	assertNotContains(t, "Content", r.Content, "multiline")
	assertNotContains(t, "Content", r.Content, "comment")
	assertContains(t, "Content", r.Content, "Before")
	assertContains(t, "Content", r.Content, "After")
}

func TestParse_InlineTagNotInUrl(t *testing.T) {
	input := "Visit https://example.com/#section for info."
	r := Parse("note", []byte(input), "notes/note.md")
	if len(r.Tags) != 0 {
		t.Errorf("Tags should be empty (# in URL is not a tag), got %v", r.Tags)
	}
}

func TestParse_InlineTagNotInCodeSpan(t *testing.T) {
	input := "Use `#not-a-tag` in code."
	r := Parse("note", []byte(input), "notes/note.md")
	if len(r.Tags) != 0 {
		t.Errorf("Tags should be empty (# in inline code is not a tag), got %v", r.Tags)
	}
}
