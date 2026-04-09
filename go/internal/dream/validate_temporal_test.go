package dream

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildTemporalReviewPrompt(t *testing.T) {
	block := &BlockInfo{
		ID:        "test-id",
		Title:     "Meeting Notes Q1",
		Content:   "We discussed the deployment on 2026-03-15. After the Go refactor, performance improved.",
		CreatedAt: time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC),
	}
	prompt := buildTemporalReviewPrompt(block)

	if !stringContains(prompt, "2026-03-20") {
		t.Error("prompt should contain created_at date")
	}
	if !stringContains(prompt, "Meeting Notes Q1") {
		t.Error("prompt should contain title")
	}
	if !stringContains(prompt, "2026-03-15") {
		t.Error("prompt should contain content dates")
	}
}

func TestBuildTemporalReviewPrompt_LongContent(t *testing.T) {
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	block := &BlockInfo{
		ID:        "test-id",
		Title:     "Long",
		Content:   string(long),
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	prompt := buildTemporalReviewPrompt(block)
	// Title + "Block created:" header + 3000 content ≈ 3050 chars.
	if len(prompt) > 3200 {
		t.Errorf("prompt should truncate content to ~3000 chars, got %d", len(prompt))
	}
}

func TestContainsDate(t *testing.T) {
	times := []time.Time{
		time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC),
	}

	if !containsDate(times, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)) {
		t.Error("should find date by day, ignoring time")
	}
	if containsDate(times, time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)) {
		t.Error("should not find non-existent date")
	}
	if !containsDate(times, time.Date(2026, 3, 20, 8, 0, 0, 0, time.UTC)) {
		t.Error("should find date with different time")
	}
}

func TestTemporalReviewParse(t *testing.T) {
	raw := `{"dates":[{"date":"2026-03-15","source":"explicit"}],"directions":[{"direction":"past","note":"after the Go refactor"}],"false_positives":["v2026.03 is a version number"]}`

	var review TemporalReview
	if err := json.Unmarshal([]byte(raw), &review); err != nil {
		t.Fatalf("failed to parse review: %v", err)
	}
	if len(review.Dates) != 1 {
		t.Errorf("expected 1 date, got %d", len(review.Dates))
	}
	if review.Dates[0].Source != "explicit" {
		t.Errorf("date source = %q, want 'explicit'", review.Dates[0].Source)
	}
	if len(review.Directions) != 1 {
		t.Errorf("expected 1 direction, got %d", len(review.Directions))
	}
	if review.Directions[0].Direction != "past" {
		t.Errorf("direction = %q, want 'past'", review.Directions[0].Direction)
	}
	if len(review.FalsePositives) != 1 {
		t.Errorf("expected 1 false positive, got %d", len(review.FalsePositives))
	}
}

func TestTemporalReviewParse_Empty(t *testing.T) {
	raw := `{"dates":[]}`
	var review TemporalReview
	if err := json.Unmarshal([]byte(raw), &review); err != nil {
		t.Fatalf("failed to parse empty review: %v", err)
	}
	if len(review.Dates) != 0 {
		t.Errorf("expected 0 dates, got %d", len(review.Dates))
	}
}

func TestContainsDate_Empty(t *testing.T) {
	if containsDate(nil, time.Now()) {
		t.Error("empty slice should never contain a date")
	}
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
