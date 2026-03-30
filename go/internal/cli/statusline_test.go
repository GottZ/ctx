package cli

import (
	"regexp"
	"strings"
	"testing"
)

func TestSmoothBar(t *testing.T) {
	ansiRe := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	tests := []struct {
		name      string
		width     int
		pct       float64
		wantLabel string // embedded label
	}{
		{"empty", 16, 0, "0%"},
		{"full", 16, 100, "100%"},
		{"half", 16, 50, "50%"},
		{"quarter", 16, 25, "25%"},
		{"tiny", 16, 3, "3%"},
		{"high", 16, 85, "85%"},
		{"over 100 clamped", 16, 150, "100%"},
		{"negative clamped", 16, -10, "0%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bar := smoothBar(tt.width, tt.pct)
			clean := ansiRe.ReplaceAllString(bar, "")

			// Must have left border
			if !strings.HasPrefix(clean, "▕") {
				t.Errorf("bar should start with ▕, got %q", clean)
			}

			// Must have right border
			if !strings.HasSuffix(clean, "▏") {
				t.Errorf("bar should end with ▏, got %q", clean)
			}

			// Inner width must match (between ▕ and ▏)
			inner := []rune(clean)[1 : len([]rune(clean))-1]
			if len(inner) != tt.width {
				t.Errorf("inner width = %d, want %d", len(inner), tt.width)
			}

			// Label must be embedded
			if !strings.Contains(clean, tt.wantLabel) {
				t.Errorf("bar should contain label %q, got %q", tt.wantLabel, clean)
			}

			// Must contain ANSI background codes
			if !strings.Contains(bar, ansiBgDark) {
				t.Error("bar should contain dark background ANSI code")
			}
		})
	}
}

func TestSmoothBarLabelPlacement(t *testing.T) {
	ansiRe := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	// Low percentage: label should be in empty area (no reverse video)
	barLow := smoothBar(16, 20)
	if strings.Contains(barLow, ansiReverse) {
		t.Error("at 20%, label should be in empty area (no reverse video)")
	}

	// High percentage: label should be in filled area (reverse video)
	barHigh := smoothBar(16, 90)
	if !strings.Contains(barHigh, ansiReverse) {
		t.Error("at 90%, label should be in filled area (reverse video)")
	}

	// 100%: label must be in filled area with reverse
	bar100 := smoothBar(16, 100)
	clean100 := ansiRe.ReplaceAllString(bar100, "")
	if !strings.Contains(clean100, "100%") {
		t.Errorf("100%% bar should contain label, got %q", clean100)
	}
	if !strings.Contains(bar100, ansiReverse) {
		t.Error("at 100%, label must use reverse video")
	}
}

func TestSmoothBarResolution(t *testing.T) {
	// Verify that different percentages produce different bars
	// at 1/8th resolution (16 chars = 128 distinct states)
	seen := make(map[string]bool)
	dupes := 0
	for pct := 0; pct <= 100; pct++ {
		bar := smoothBar(16, float64(pct))
		if seen[bar] {
			dupes++
		}
		seen[bar] = true
	}
	// With 128 possible states and 101 percentage values,
	// we should have very few duplicates
	if dupes > 5 {
		t.Errorf("too many duplicate bars (%d), expected good resolution", dupes)
	}
}

func TestBarColor(t *testing.T) {
	tests := []struct {
		pct  float64
		want string
	}{
		{0, ansiGreen},
		{25, ansiGreen},
		{49, ansiGreen},
		{50, ansiYellow},
		{64, ansiYellow},
		{65, ansiOrange},
		{79, ansiOrange},
		{80, ansiRed},
		{100, ansiRed},
	}
	for _, tt := range tests {
		got := barColor(tt.pct)
		if got != tt.want {
			t.Errorf("barColor(%v) = %q, want %q", tt.pct, got, tt.want)
		}
	}
}

func TestRateColor(t *testing.T) {
	tests := []struct {
		pct  float64
		want string
	}{
		{0, ansiDim},
		{49, ansiDim},
		{50, ansiYellow},
		{74, ansiYellow},
		{75, ansiOrange},
		{89, ansiOrange},
		{90, ansiRed},
		{100, ansiRed},
	}
	for _, tt := range tests {
		got := rateColor(tt.pct)
		if got != tt.want {
			t.Errorf("rateColor(%v) = %q, want %q", tt.pct, got, tt.want)
		}
	}
}

func TestHealthIndicator(t *testing.T) {
	// ok = no indicator (block count implies healthy)
	ok := healthIndicator("ok")
	if ok != "" {
		t.Errorf("ok should be empty, got %q", ok)
	}

	// degraded = yellow warning
	degraded := healthIndicator("degraded")
	if !strings.Contains(degraded, "\uEA6C") {
		t.Errorf("degraded should contain nerdfont outline warning, got %q", degraded)
	}
	if !strings.Contains(degraded, ansiYellow) {
		t.Error("degraded should be yellow")
	}

	// unhealthy/error = red warning
	for _, status := range []string{"unhealthy", "error", "unknown"} {
		got := healthIndicator(status)
		if !strings.Contains(got, "\uF071") {
			t.Errorf("%s should contain nerdfont warning, got %q", status, got)
		}
		if !strings.Contains(got, ansiRed) {
			t.Errorf("%s should be red", status)
		}
	}
}

