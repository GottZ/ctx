package store

import (
	"testing"
	"time"
)

func TestExpandDimensions(t *testing.T) {
	// 2026-03-29 is a Sunday, ISO week 13, Q1
	d := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	dims := ExpandDimensions(d)

	if len(dims) != 5 {
		t.Fatalf("expected 5 dimensions, got %d", len(dims))
	}

	expected := map[string]string{
		"year":    "2026",
		"month":   "3",
		"week":    "13",
		"weekday": "7", // Sunday = ISO 7
		"quarter": "1",
	}

	for _, dim := range dims {
		want, ok := expected[dim.Dimension]
		if !ok {
			t.Errorf("unexpected dimension %q", dim.Dimension)
			continue
		}
		if dim.Value != want {
			t.Errorf("dimension %q: got %q, want %q", dim.Dimension, dim.Value, want)
		}
		if !dim.SourceDate.Equal(d) {
			t.Errorf("dimension %q: source date mismatch", dim.Dimension)
		}
		delete(expected, dim.Dimension)
	}

	for dim := range expected {
		t.Errorf("missing dimension %q", dim)
	}
}

func TestExpandDimensions_Weekday(t *testing.T) {
	// Verify ISO weekday mapping for all 7 days
	tests := []struct {
		date    time.Time
		isoDay  string
		dayName string
	}{
		{time.Date(2026, 3, 23, 0, 0, 0, 0, time.UTC), "1", "Monday"},
		{time.Date(2026, 3, 24, 0, 0, 0, 0, time.UTC), "2", "Tuesday"},
		{time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC), "3", "Wednesday"},
		{time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC), "4", "Thursday"},
		{time.Date(2026, 3, 27, 0, 0, 0, 0, time.UTC), "5", "Friday"},
		{time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC), "6", "Saturday"},
		{time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC), "7", "Sunday"},
	}

	for _, tt := range tests {
		dims := ExpandDimensions(tt.date)
		var weekdayDim *TemporalDimension
		for i := range dims {
			if dims[i].Dimension == "weekday" {
				weekdayDim = &dims[i]
				break
			}
		}
		if weekdayDim == nil {
			t.Errorf("%s: weekday dimension not found", tt.dayName)
			continue
		}
		if weekdayDim.Value != tt.isoDay {
			t.Errorf("%s: got ISO weekday %s, want %s", tt.dayName, weekdayDim.Value, tt.isoDay)
		}
	}
}

func TestExpandDimensions_Quarter(t *testing.T) {
	tests := []struct {
		month   time.Month
		quarter string
	}{
		{time.January, "1"},
		{time.March, "1"},
		{time.April, "2"},
		{time.June, "2"},
		{time.July, "3"},
		{time.September, "3"},
		{time.October, "4"},
		{time.December, "4"},
	}

	for _, tt := range tests {
		d := time.Date(2026, tt.month, 15, 0, 0, 0, 0, time.UTC)
		dims := ExpandDimensions(d)
		var quarterDim *TemporalDimension
		for i := range dims {
			if dims[i].Dimension == "quarter" {
				quarterDim = &dims[i]
				break
			}
		}
		if quarterDim == nil {
			t.Fatalf("month %d: quarter dimension not found", tt.month)
		}
		if quarterDim.Value != tt.quarter {
			t.Errorf("month %d: got quarter %s, want %s", tt.month, quarterDim.Value, tt.quarter)
		}
	}
}

func TestBuildTemporalBatch_EmptyDates(t *testing.T) {
	batch, count := BuildTemporalBatch("block-123", nil)

	if count != 2 {
		t.Fatalf("sentinel path: expected query count 2, got %d", count)
	}
	if batch.Len() != 2 {
		t.Fatalf("sentinel path: expected batch len 2, got %d", batch.Len())
	}
}

func TestBuildTemporalBatch_WithDates(t *testing.T) {
	tests := []struct {
		name      string
		dates     []time.Time
		wantCount int
	}{
		{
			name:      "1 date = 1 DELETE + 5 INSERTs",
			dates:     []time.Time{time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)},
			wantCount: 6,
		},
		{
			name: "3 dates = 1 DELETE + 15 INSERTs",
			dates: []time.Time{
				time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
			},
			wantCount: 16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batch, count := BuildTemporalBatch("block-456", tt.dates)

			if count != tt.wantCount {
				t.Errorf("query count: got %d, want %d", count, tt.wantCount)
			}
			if batch.Len() != tt.wantCount {
				t.Errorf("batch len: got %d, want %d", batch.Len(), tt.wantCount)
			}
		})
	}
}

func TestPopulateTemporal_SentinelPath(t *testing.T) {
	// Verify that the sentinel path produces exactly the right batch structure.
	// We can't call PopulateTemporal without a DB, but BuildTemporalBatch
	// captures the exact same logic.

	// Empty slice (not nil) should also trigger sentinel
	batch, count := BuildTemporalBatch("block-sentinel", []time.Time{})
	if count != 2 {
		t.Fatalf("empty slice: expected query count 2, got %d", count)
	}
	if batch.Len() != 2 {
		t.Fatalf("empty slice: expected batch len 2, got %d", batch.Len())
	}

	// nil slice should also trigger sentinel
	batch2, count2 := BuildTemporalBatch("block-sentinel-nil", nil)
	if count2 != 2 {
		t.Fatalf("nil slice: expected query count 2, got %d", count2)
	}
	if batch2.Len() != 2 {
		t.Fatalf("nil slice: expected batch len 2, got %d", batch2.Len())
	}
}

func TestExpandDimensions_ISOWeekEdge(t *testing.T) {
	// Jan 1, 2026 is a Thursday → ISO week 1
	d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dims := ExpandDimensions(d)

	var weekDim *TemporalDimension
	for i := range dims {
		if dims[i].Dimension == "week" {
			weekDim = &dims[i]
			break
		}
	}
	if weekDim == nil {
		t.Fatal("week dimension not found")
	}
	if weekDim.Value != "1" {
		t.Errorf("Jan 1 2026 ISO week: got %s, want 1", weekDim.Value)
	}

	// Dec 31, 2026 is a Thursday → ISO week 53
	d2 := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	dims2 := ExpandDimensions(d2)

	var weekDim2 *TemporalDimension
	for i := range dims2 {
		if dims2[i].Dimension == "week" {
			weekDim2 = &dims2[i]
			break
		}
	}
	if weekDim2 == nil {
		t.Fatal("week dimension not found for Dec 31")
	}
	// Dec 31, 2026 is Thursday → ISO week 53 of 2026
	if weekDim2.Value != "53" {
		t.Errorf("Dec 31 2026 ISO week: got %s, want 53", weekDim2.Value)
	}
}
