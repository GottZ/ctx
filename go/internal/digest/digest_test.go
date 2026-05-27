package digest

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateTitle_EmDashAtBoundary is the direct regression test for
// Issue #4: a title with an em-dash at byte 66 (lead byte of a 3-byte rune)
// must NOT yield invalid UTF-8 after truncation. The pre-fix code did
// `title[:67] + "..."` which left a dangling 0xe2 lead byte followed by 0x2e,
// breaking utf8.ValidString and causing the topic-map UPSERT to fail with
// SQLSTATE 22021 (invalid byte sequence for encoding "UTF8").
func TestTruncateTitle_EmDashAtBoundary(t *testing.T) {
	// 66 ASCII bytes, then em-dash (E2 80 94), then tail to exceed the 70-rune guard.
	title := strings.Repeat("a", 66) + "—long tail well past seventy bytes"

	out := truncateTitle(title)

	if !utf8.ValidString(out) {
		t.Fatalf("truncateTitle produced invalid UTF-8: % x", []byte(out))
	}
	if utf8.RuneCountInString(out) > 70 {
		t.Errorf("truncateTitle output has %d runes, want <= 70",
			utf8.RuneCountInString(out))
	}
	want := strings.Repeat("a", 66) + "—..."
	if out != want {
		t.Errorf("truncateTitle = %q, want %q", out, want)
	}
}

// TestTruncateTitle_EllipsisAtBoundary covers the other common 3-byte rune
// from Damien's report: U+2026 HORIZONTAL ELLIPSIS (E2 80 A6).
func TestTruncateTitle_EllipsisAtBoundary(t *testing.T) {
	title := strings.Repeat("a", 66) + "… trailing content past the cap"
	out := truncateTitle(title)
	if !utf8.ValidString(out) {
		t.Fatalf("truncateTitle produced invalid UTF-8: % x", []byte(out))
	}
}

// TestTruncateTitle_NoTruncationNeeded — short titles unchanged.
func TestTruncateTitle_NoTruncationNeeded(t *testing.T) {
	title := "short ASCII title"
	if got := truncateTitle(title); got != title {
		t.Errorf("truncateTitle(%q) = %q, want unchanged", title, got)
	}
	// Non-ASCII but under limit.
	title2 := "中文标题 mit ein paar emojis 🎉"
	if got := truncateTitle(title2); got != title2 {
		t.Errorf("truncateTitle(%q) = %q, want unchanged", title2, got)
	}
}

// TestTruncateTitle_ExactBoundary70 — a title of exactly 70 runes must NOT
// be truncated.
func TestTruncateTitle_ExactBoundary70(t *testing.T) {
	title := strings.Repeat("z", 70)
	if got := truncateTitle(title); got != title {
		t.Errorf("truncateTitle at exactly 70 runes: got %q, want unchanged", got)
	}
}

// TestTruncateTitle_FuzzMultiByteBoundary walks an em-dash through 100 byte
// offsets and asserts the output is always valid UTF-8 and within the rune
// cap. Guards against future regressions if the cap or suffix is changed.
func TestTruncateTitle_FuzzMultiByteBoundary(t *testing.T) {
	for offset := 0; offset < 100; offset++ {
		prefix := strings.Repeat("a", offset)
		title := prefix + "—" + strings.Repeat("z", 100)
		out := truncateTitle(title)
		if !utf8.ValidString(out) {
			t.Errorf("offset=%d: invalid UTF-8 in %q", offset, out)
		}
		if utf8.RuneCountInString(out) > 70 {
			t.Errorf("offset=%d: %d runes (> 70)", offset, utf8.RuneCountInString(out))
		}
	}
}

// TestTruncateTitle_ChineseAndEmoji — multi-byte ASCII-adjacent characters
// across various rune widths (3-byte Chinese, 4-byte emoji).
func TestTruncateTitle_ChineseAndEmoji(t *testing.T) {
	cases := []string{
		strings.Repeat("中", 100),
		strings.Repeat("🎉", 100),
		"mixed 中文 emoji 🎉 " + strings.Repeat("a", 200),
	}
	for i, c := range cases {
		out := truncateTitle(c)
		if !utf8.ValidString(out) {
			t.Errorf("case %d: invalid UTF-8 in %q", i, out)
		}
		if utf8.RuneCountInString(out) > 70 {
			t.Errorf("case %d: %d runes (> 70)", i, utf8.RuneCountInString(out))
		}
	}
}
