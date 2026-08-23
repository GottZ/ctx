package dream

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
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

// TestParseTemporalReviewFences pins the fence tolerance of the Phase-2
// decode. The two fenced rows FAILED before parseTemporalReview existed —
// json.Unmarshal on the raw content reported "invalid character '`'" — and the
// failure is invisible in production: it is non-fatal, and
// dream_temporal_validated_at is stamped anyway, so the block is not re-dreamed
// for it. Reverting the stripCodeFence call turns these rows red.
func TestParseTemporalReviewFences(t *testing.T) {
	const payload = `{"dates":[{"date":"2026-03-15","source":"explicit"}]}`

	tests := []struct {
		name string
		raw  string
	}{
		{name: "bare", raw: payload},
		{name: "json fenced", raw: "```json\n" + payload + "\n```"},
		{name: "bare fenced", raw: "```\n" + payload + "\n```"},
		{name: "fenced with surrounding whitespace", raw: "\n  ```json\n" + payload + "\n```\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review, err := parseTemporalReview(tt.raw)
			if err != nil {
				t.Fatalf("parseTemporalReview(%q): %v", tt.raw, err)
			}
			if len(review.Dates) != 1 {
				t.Fatalf("dates = %d, want 1", len(review.Dates))
			}
			if review.Dates[0].Date != "2026-03-15" {
				t.Errorf("date = %q, want 2026-03-15", review.Dates[0].Date)
			}
		})
	}
}

// TestParseTemporalReviewProse keeps the failure that MUST stay a failure:
// commentary around the JSON is not extracted. The stage's answer contract is
// a single object, and a "grab the first {...}" scanner is the leniency class
// the link parser's zero-link contract exists to refuse.
func TestParseTemporalReviewProse(t *testing.T) {
	if _, err := parseTemporalReview("Here is the review: {\"dates\":[]}"); err == nil {
		t.Error("prose-wrapped answer parsed, want a parse error")
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

func TestTemporalTimeout(t *testing.T) {
	tests := []struct {
		name     string
		router   *Router
		expected time.Duration
	}{
		{
			name:     "nil router falls back to package default",
			router:   nil,
			expected: ValidateTimeout,
		},
		{
			name:     "router without timeout falls back to package default",
			router:   &Router{},
			expected: ValidateTimeout,
		},
		{
			name:     "router setting wins",
			router:   &Router{TemporalTimeout: 180 * time.Second},
			expected: 180 * time.Second,
		},
		{
			// A negative value never reaches a wired router — config V16
			// rejects it at boot and at the settings write. The fallback is
			// the second line of defence for hand-built routers.
			name:     "negative router value falls back to package default",
			router:   &Router{TemporalTimeout: -30 * time.Second},
			expected: ValidateTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := temporalTimeout(tt.router); got != tt.expected {
				t.Errorf("temporalTimeout() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// temporalWireRouter seeds a single dream backend whose per-role timeout map
// is under the test's control, so the row half of the precedence rule is
// reachable. Same shape as newTestRouter otherwise (full trust, so the
// zero-value fixture sensitivity still resolves a chain).
func temporalWireRouter(routerTimeout time.Duration, rowTimeouts map[string]int) *Router {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "test-backend-id", Name: "test-backend",
		Host: "h", APIKey: "k",
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Timeouts: rowTimeouts,
		Priority: 100, Enabled: true,
	}})
	return &Router{Pool: p, Admit: testAdmit(), TemporalTimeout: routerTimeout}
}

// TestTemporalReviewWireTimeout pins what the Phase-2 call actually puts on
// the wire — the contract TestTemporalTimeout above cannot see, because
// temporalTimeout only produces the chain walk's DEFAULT.
//
// Two things are pinned here. First, the call site consumes the router
// setting at all: reverting it to the bare ValidateTimeout constant leaves
// every other test in the package green, so without this the one mutation
// that undoes the whole feature is invisible. Second, the precedence rule the
// operations row documents: the Phase-2 call resolves under role dream, so a
// timeouts.dream entry on the serving context_backends row wins over the
// config key (Backend.TimeoutFor inside llm.ChatChainVia) — operators on such
// a row have to raise the row value instead.
func TestTemporalReviewWireTimeout(t *testing.T) {
	tests := []struct {
		name          string
		routerTimeout time.Duration
		rowTimeouts   map[string]int
		want          time.Duration
	}{
		{
			name:          "unwired router puts the package default on the wire",
			routerTimeout: 0,
			want:          ValidateTimeout,
		},
		{
			name:          "router setting reaches the wire",
			routerTimeout: 180 * time.Second,
			want:          180 * time.Second,
		},
		{
			name:          "row timeouts.dream beats the configured key",
			routerTimeout: 180 * time.Second,
			rowTimeouts:   map[string]int{backends.RoleDream: 90},
			want:          90 * time.Second,
		},
		{
			name:        "row timeouts.dream beats the package default too",
			rowTimeouts: map[string]int{backends.RoleDream: 240},
			want:        240 * time.Second,
		},
		{
			name:          "a foreign role entry does not apply",
			routerTimeout: 180 * time.Second,
			rowTimeouts:   map[string]int{backends.RoleDigest: 30},
			want:          180 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got time.Duration
			calls := 0
			mockChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
				calls++
				got = timeout
				return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: `{"dates":[]}`}}, nil
			})

			block := srcBlock(uuidA)
			r := temporalWireRouter(tt.routerTimeout, tt.rowTimeouts)
			if _, _, _, err := r.temporalReview(context.Background(), &block, "user prompt", llm.Options{}); err != nil {
				t.Fatalf("temporalReview: %v", err)
			}
			if calls != 1 {
				t.Fatalf("wire calls = %d, want 1", calls)
			}
			if got != tt.want {
				t.Errorf("wire timeout = %v, want %v", got, tt.want)
			}
		})
	}
}
