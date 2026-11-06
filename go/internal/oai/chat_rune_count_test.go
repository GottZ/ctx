package oai

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRuneCountIsRuneCountWithoutACopy (hardening 6): runeCount gives
// utf8.RuneCount's number for every byte sequence, valid or not, and
// allocates nothing — RuneCount copied the bytes after the first non-ASCII
// one (go1.27), 8 MiB for a message array of 8 MiB.
func TestRuneCountIsRuneCountWithoutACopy(t *testing.T) {
	for _, s := range []string{
		"", "a", "é", "aé", "\xff", "\xffa", "a\xff\xff", "\xe2\x82", "\xe2\x82\xac", "\x80", "\xc0\xaf", "\xed\xa0\x80",
		"\xf0\x9f\x98\x80x", "\xf0\x9f\x98", "\xef\xbf\xbd", strings.Repeat("aé\xff€\xf0\x9f\x98\x80", 1000),
	} {
		if got, want := runeCount([]byte(s)), utf8.RuneCount([]byte(s)); got != want {
			t.Errorf("runeCount(%q) = %d, utf8.RuneCount %d", s, got, want)
		}
	}
	big := []byte(`[{"role":"developer","content":"` + strings.Repeat("é", 512<<10) + `"}]`)
	if got := allocatedBy(func() { _ = runeCount(big) }); got > 0 {
		t.Errorf("runeCount of %d bytes allocates %d bytes, want 0", len(big), got)
	}
	if got := runeCount(big); got != utf8.RuneCount(big) {
		t.Errorf("runeCount = %d, utf8.RuneCount %d", got, utf8.RuneCount(big))
	}
}
