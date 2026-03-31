package cli

import (
	"testing"
)

func TestParseMarkersSingle(t *testing.T) {
	text := `Some analysis here.

[PERSIST:learnings:RRF Threshold]
Bei k=60 liegt der Sweet Spot bei 0.008.
`
	blocks := parseMarkers(text)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Category != "learnings" {
		t.Errorf("category = %q", blocks[0].Category)
	}
	if blocks[0].Title != "RRF Threshold" {
		t.Errorf("title = %q", blocks[0].Title)
	}
	if blocks[0].Content != "Bei k=60 liegt der Sweet Spot bei 0.008." {
		t.Errorf("content = %q", blocks[0].Content)
	}
}

func TestParseMarkersMultiple(t *testing.T) {
	text := `Intro text.

[PERSIST:learnings:Finding A]
First finding content.
Multi-line.

[PERSIST:decisions:Finding B]
Second finding content.
`
	blocks := parseMarkers(text)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Category != "learnings" || blocks[0].Title != "Finding A" {
		t.Errorf("block 0: %q / %q", blocks[0].Category, blocks[0].Title)
	}
	if blocks[1].Category != "decisions" || blocks[1].Title != "Finding B" {
		t.Errorf("block 1: %q / %q", blocks[1].Category, blocks[1].Title)
	}
	if blocks[0].Content != "First finding content.\nMulti-line." {
		t.Errorf("block 0 content = %q", blocks[0].Content)
	}
	if blocks[1].Content != "Second finding content." {
		t.Errorf("block 1 content = %q", blocks[1].Content)
	}
}

func TestParseMarkersNone(t *testing.T) {
	blocks := parseMarkers("Just regular text without markers.")
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestParseMarkersEmpty(t *testing.T) {
	blocks := parseMarkers("")
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestParseMarkersEmptyContent(t *testing.T) {
	text := `[PERSIST:learnings:Empty]

[PERSIST:decisions:Has Content]
Real content here.
`
	blocks := parseMarkers(text)
	// First marker has no content → skipped.
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (empty content skipped), got %d", len(blocks))
	}
	if blocks[0].Title != "Has Content" {
		t.Errorf("expected 'Has Content', got %q", blocks[0].Title)
	}
}

func TestParseMarkersSpecialChars(t *testing.T) {
	text := `[PERSIST:infrastructure:PG18 + pgvector Tuning]
shared_buffers=8GB, work_mem=64MB, effective_cache_size=48GB.
`
	blocks := parseMarkers(text)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Title != "PG18 + pgvector Tuning" {
		t.Errorf("title = %q", blocks[0].Title)
	}
}

func TestBuildMetadata(t *testing.T) {
	input := &persistHookInput{
		CWD:       "/compose/n8n",
		SessionID: "sess-123",
		AgentType: "Explore",
		AgentID:   "agent-abc",
	}
	meta := buildMetadata(input)
	if meta.SessionID != "sess-123" {
		t.Errorf("session = %q", meta.SessionID)
	}
	if meta.AgentType != "Explore" {
		t.Errorf("agent_type = %q", meta.AgentType)
	}
	if meta.Hostname == "" {
		t.Error("hostname should not be empty")
	}
	if meta.Project != "/compose/n8n" {
		t.Errorf("project = %q", meta.Project)
	}
}
