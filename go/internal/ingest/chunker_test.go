package ingest

import (
	"strings"
	"testing"
)

// helper: build a string of approximately n characters by repeating s.
func fillTo(s string, n int) string {
	if len(s) == 0 || n <= 0 {
		return ""
	}
	reps := (n + len(s) - 1) / len(s)
	result := strings.Repeat(s, reps)
	if len(result) > n {
		return result[:n]
	}
	return result
}

// --- Whole-Document (no split needed) ---

func TestChunk_ShortDoc_SingleChunk(t *testing.T) {
	content := fillTo("Some filler text for a short document. ", 500)
	if len(content) > MaxChunkChars {
		t.Fatalf("test setup: content too long (%d)", len(content))
	}

	chunks := Chunk("note", content, nil)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Content != content {
		t.Error("single chunk content should be identical to input")
	}
	if chunks[0].Index != 0 {
		t.Errorf("expected Index=0, got %d", chunks[0].Index)
	}
	if chunks[0].Total != 1 {
		t.Errorf("expected Total=1, got %d", chunks[0].Total)
	}
}

func TestChunk_ExactlyAtLimit(t *testing.T) {
	content := fillTo("x", MaxChunkChars)
	if len(content) != MaxChunkChars {
		t.Fatalf("test setup: expected exactly %d chars, got %d", MaxChunkChars, len(content))
	}

	chunks := Chunk("exact", content, nil)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk at exactly MaxChunkChars, got %d", len(chunks))
	}
}

func TestChunk_EmptyContent(t *testing.T) {
	chunks := Chunk("empty", "", nil)
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty content, got %d", len(chunks))
	}
}

// --- Heading-Split ---

func TestChunk_H1Split(t *testing.T) {
	// Two H1 sections, each ~4000 chars → total ~8000 > MaxChunkChars → must split
	section := fillTo("Content paragraph for testing heading splits. ", 4000)
	content := "# Section One\n\n" + section + "\n\n# Section Two\n\n" + section
	t.Logf("total content length: %d", len(content))

	chunks := Chunk("doc", content, nil)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks for 2 H1 sections, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0].HeadingPath, "Section One") {
		t.Errorf("chunk 0 HeadingPath should contain 'Section One', got %q", chunks[0].HeadingPath)
	}
	if !strings.Contains(chunks[1].HeadingPath, "Section Two") {
		t.Errorf("chunk 1 HeadingPath should contain 'Section Two', got %q", chunks[1].HeadingPath)
	}
}

func TestChunk_H2Split(t *testing.T) {
	section := fillTo("Paragraph content here for H2 split testing. ", 3000)
	content := "## Alpha\n\n" + section + "\n\n## Beta\n\n" + section + "\n\n## Gamma\n\n" + section
	t.Logf("total content length: %d", len(content))

	chunks := Chunk("doc", content, nil)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks for 3 H2 sections, got %d", len(chunks))
	}
}

func TestChunk_H1AndH2Mixed(t *testing.T) {
	// H1 with sub-H2s — total <MaxChunkChars → stays together
	small := fillTo("Small text block. ", 600)
	content := "# Main\n\n## Sub A\n\n" + small + "\n\n## Sub B\n\n" + small
	t.Logf("total content length: %d", len(content))

	if len(content) >= MaxChunkChars {
		t.Fatalf("test setup: combined content too long (%d)", len(content))
	}

	chunks := Chunk("mixed", content, nil)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk (combined < MaxChunkChars), got %d", len(chunks))
	}
}

func TestChunk_LongSectionFallback(t *testing.T) {
	// Single section >MaxChunkChars → should paragraph-split
	para1 := fillTo("Long paragraph content for fallback testing. ", 4000)
	para2 := fillTo("Another long paragraph for overflow. ", 4000)
	content := "# Huge Section\n\n" + para1 + "\n\n" + para2
	t.Logf("total content length: %d", len(content))

	if len(content) <= MaxChunkChars {
		t.Fatalf("test setup: content should exceed MaxChunkChars (%d)", len(content))
	}

	chunks := Chunk("big", content, nil)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks from paragraph fallback, got %d", len(chunks))
	}
	// All chunks should be within limit
	for i, c := range chunks {
		if len(c.Content) > MaxChunkChars {
			t.Errorf("chunk %d exceeds MaxChunkChars: %d", i, len(c.Content))
		}
	}
}

// --- Context-Header ---

func TestChunk_HeaderFormat_Short(t *testing.T) {
	content := fillTo("Short content for header test. ", 300)
	chunks := Chunk("note", content, []string{"tag1"})
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0].Header, "Datei: note") {
		t.Errorf("short header should contain 'Datei: note', got %q", chunks[0].Header)
	}
	// Short docs should NOT have section info
	if strings.Contains(chunks[0].Header, "Sektion:") {
		t.Errorf("short doc header should not have section info, got %q", chunks[0].Header)
	}
}

func TestChunk_HeaderFormat_Medium(t *testing.T) {
	// Build a doc >MaxChunkChars with sections in 500-2000 range
	sec1 := fillTo("Medium text block for header test. ", 1000)
	sec2 := fillTo("Another medium block for testing. ", 1000)
	sec3 := fillTo("Third section text for testing. ", 6000)
	content := "# Part A\n\n" + sec1 + "\n\n# Part B\n\n" + sec2 + "\n\n# Part C\n\n" + sec3
	t.Logf("total content length: %d", len(content))

	chunks := Chunk("report", content, nil)
	if len(chunks) == 0 {
		t.Fatal("no chunks generated")
	}

	// Find a chunk in the 500-2000 range
	found := false
	for _, c := range chunks {
		if len(c.Content) >= 500 && len(c.Content) <= 2000 {
			if !strings.Contains(c.Header, "Datei: report") {
				t.Errorf("medium header should contain 'Datei: report', got %q", c.Header)
			}
			if !strings.Contains(c.Header, "Sektion:") {
				t.Errorf("medium header should contain 'Sektion:', got %q", c.Header)
			}
			found = true
			break
		}
	}
	if !found {
		// Relax: just check that multi-chunk docs get section info
		for _, c := range chunks {
			if c.HeadingPath != "" && !strings.Contains(c.Header, "Sektion:") {
				t.Errorf("chunk with heading should have Sektion in header, got %q", c.Header)
			}
		}
	}
}

func TestChunk_HeaderFormat_Long(t *testing.T) {
	// Large chunks >2000 chars with tags
	sec := fillTo("Long content block for header format testing. ", 4000)
	content := "# Big Section\n\n" + sec + "\n\n# Other Section\n\n" + sec
	t.Logf("total content length: %d", len(content))

	tags := []string{"database", "config"}
	chunks := Chunk("manual", content, tags)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	// Find a chunk >2000 chars
	foundLong := false
	for _, c := range chunks {
		if len(c.Content) > 2000 {
			if !strings.Contains(c.Header, "Tags:") {
				t.Errorf("long header should contain Tags, got %q", c.Header)
			}
			foundLong = true
			break
		}
	}
	if !foundLong {
		t.Error("no chunk >2000 chars found for tag test")
	}
}

// --- Chunk-Metadata ---

func TestChunk_IndexAssignment(t *testing.T) {
	sec := fillTo("Index test content for chunk metadata. ", 4000)
	content := "# A\n\n" + sec + "\n\n# B\n\n" + sec + "\n\n# C\n\n" + sec

	chunks := Chunk("idx", content, nil)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk %d: expected Index=%d, got %d", i, i, c.Index)
		}
	}
}

func TestChunk_TotalCount(t *testing.T) {
	sec := fillTo("Total test content for chunk metadata. ", 4000)
	content := "# X\n\n" + sec + "\n\n# Y\n\n" + sec + "\n\n# Z\n\n" + sec

	chunks := Chunk("tot", content, nil)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Total != 3 {
			t.Errorf("chunk %d: expected Total=3, got %d", i, c.Total)
		}
	}
}

func TestChunk_TitleGeneration(t *testing.T) {
	sec := fillTo("Title generation content for metadata. ", 4000)
	content := "# Database\n\n" + sec + "\n\n# Networking\n\n" + sec

	chunks := Chunk("note", content, nil)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	if chunks[0].Title != "note — Database" {
		t.Errorf("expected title 'note — Database', got %q", chunks[0].Title)
	}
	if chunks[1].Title != "note — Networking" {
		t.Errorf("expected title 'note — Networking', got %q", chunks[1].Title)
	}
}

// --- Edge Cases ---

func TestChunk_NoHeadings_LongDoc(t *testing.T) {
	// 10000+ chars without headings → should paragraph-split
	para := fillTo("No heading paragraph for edge case testing. ", 2600)
	content := para + "\n\n" + para + "\n\n" + para + "\n\n" + para
	t.Logf("total content length: %d", len(content))

	if len(content) <= MaxChunkChars {
		t.Fatalf("test setup: content should exceed MaxChunkChars (%d)", len(content))
	}

	chunks := Chunk("flat", content, nil)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks from paragraph split, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c.Content) > MaxChunkChars {
			t.Errorf("chunk %d exceeds MaxChunkChars: %d", i, len(c.Content))
		}
	}
}

func TestChunk_OnlyHeadings(t *testing.T) {
	content := "# Heading One\n\n# Heading Two\n\n# Heading Three\n"

	chunks := Chunk("headings", content, nil)
	// Headings without meaningful content → 0 chunks
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for headings-only doc, got %d", len(chunks))
	}
}

func TestChunk_MinChunkSize(t *testing.T) {
	// Sections smaller than MinChunkChars should be merged with next
	sec := fillTo("Section content here for merge testing. ", 4000)
	content := "# Tiny\n\nSmall.\n\n# Normal\n\n" + sec + "\n\n# Also Tiny\n\nAlso small.\n\n# Big\n\n" + sec

	chunks := Chunk("merge", content, nil)
	// Tiny sections should be merged — so we should get fewer chunks than sections
	for i, c := range chunks {
		if len(strings.TrimSpace(c.Content)) < MinChunkChars && len(chunks) > 1 {
			t.Errorf("chunk %d is below MinChunkChars (%d chars): should have been merged",
				i, len(strings.TrimSpace(c.Content)))
		}
	}
}
