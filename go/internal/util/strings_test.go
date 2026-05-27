package util

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 10, ""},
		{"ascii_under_limit", "hello", 10, "hello"},
		{"ascii_exact_limit", "helloworld", 10, "helloworld"},
		{"ascii_over_limit", "helloworldfoo", 10, "hellowo..."},
		{"n_zero", "anything", 0, ""},
		{"n_negative", "anything", -5, ""},
		{"n_one_truncates", "anything", 1, "."},
		{"n_three_truncates", "anything", 3, "..."},
		{"n_four_truncates", "anything", 4, "a..."},
		{"emdash_at_boundary_byte67",
			strings.Repeat("a", 66) + "—long tail well past seventy bytes",
			70,
			strings.Repeat("a", 66) + "—...",
		},
		{"ellipsis_at_boundary",
			strings.Repeat("b", 50) + "… more content that exceeds the limit",
			60,
			strings.Repeat("b", 50) + "… more ...",
		},
		{"chinese_chars",
			"中文测试" + strings.Repeat("字", 100),
			10,
			"中文测试字字字...",
		},
		{"emoji",
			"hello 🎉 world " + strings.Repeat("x", 100),
			15,
			"hello 🎉 worl...",
		},
		{"multi_byte_under_limit", "中文测试", 10, "中文测试"},
		{"multi_byte_exact_limit", "中文测试", 4, "中文测试"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateRunes(tt.in, tt.n)
			if got != tt.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunes(%q, %d) = %q: invalid UTF-8", tt.in, tt.n, got)
			}
			if utf8.RuneCountInString(got) > tt.n && tt.n > 0 {
				t.Errorf("TruncateRunes(%q, %d) = %q has %d runes (> %d)",
					tt.in, tt.n, got, utf8.RuneCountInString(got), tt.n)
			}
		})
	}
}

// TestTruncateRunes_NeverInvalidUTF8 fuzzes multi-byte boundary positions to
// guarantee the helper never returns invalid UTF-8, regardless of where the
// rune boundary falls relative to n.
func TestTruncateRunes_NeverInvalidUTF8(t *testing.T) {
	// Place an em-dash (3 bytes) at every byte offset from 0..99 with tail.
	for offset := 0; offset < 100; offset++ {
		prefix := strings.Repeat("a", offset)
		s := prefix + "—" + strings.Repeat("z", 100)
		for n := 1; n <= 110; n++ {
			got := TruncateRunes(s, n)
			if !utf8.ValidString(got) {
				t.Errorf("offset=%d n=%d: invalid UTF-8 in %q", offset, n, got)
			}
		}
	}
}

func TestTruncateRunesWithSuffix(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		suffix string
		n      int
		want   string
	}{
		{"empty", "", "...", 10, ""},
		{"under_limit_ascii", "hello", "...", 10, "hello"},
		{"exact_limit_ascii", "helloworld", "...", 10, "helloworld"},
		{"one_over_limit_ascii", "helloworld!", "...", 10, "helloworld..."},
		{"emdash_at_boundary",
			strings.Repeat("a", 66) + "—long tail well past seventy bytes",
			"...",
			70,
			strings.Repeat("a", 66) + "—lon...",
		},
		{"long_suffix",
			strings.Repeat("x", 1501),
			"[... truncated]",
			1500,
			strings.Repeat("x", 1500) + "[... truncated]",
		},
		{"multi_byte_under_limit", "中文测试", "...", 10, "中文测试"},
		{"negative_n", "hello", "...", -5, "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateRunesWithSuffix(tt.in, tt.suffix, tt.n)
			if got != tt.want {
				t.Errorf("TruncateRunesWithSuffix(%q, %q, %d) = %q, want %q",
					tt.in, tt.suffix, tt.n, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunesWithSuffix(%q, %q, %d) = %q: invalid UTF-8",
					tt.in, tt.suffix, tt.n, got)
			}
		})
	}
}

func TestTruncateRunesSuffix(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		suffix string
		n      int
		want   string
	}{
		{"empty", "", "[trunc]", 10, ""},
		{"under_limit", "hello", "[trunc]", 10, "hello"},
		{"over_limit_long_suffix",
			strings.Repeat("x", 30),
			"[... truncated]",
			20,
			strings.Repeat("x", 5) + "[... truncated]",
		},
		{"emdash_with_custom_suffix",
			strings.Repeat("a", 66) + "—long tail",
			"[... truncated]",
			70,
			strings.Repeat("a", 55) + "[... truncated]",
		},
		{"suffix_longer_than_n",
			"abcdefghij",
			"[... truncated]",
			5,
			"[... ",
		},
		{"n_zero", "anything", "[t]", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateRunesSuffix(tt.in, tt.suffix, tt.n)
			if got != tt.want {
				t.Errorf("TruncateRunesSuffix(%q, %q, %d) = %q, want %q",
					tt.in, tt.suffix, tt.n, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunesSuffix(%q, %q, %d) = %q: invalid UTF-8",
					tt.in, tt.suffix, tt.n, got)
			}
			if utf8.RuneCountInString(got) > tt.n && tt.n > 0 {
				t.Errorf("TruncateRunesSuffix(%q, %q, %d) = %q has %d runes (> %d)",
					tt.in, tt.suffix, tt.n, got, utf8.RuneCountInString(got), tt.n)
			}
		})
	}
}
