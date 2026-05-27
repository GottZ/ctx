// Package util provides small, dependency-free helpers shared across packages.
package util

import "unicode/utf8"

// TruncateRunes returns s truncated to at most n runes, appending "..." if
// truncation occurred. Rune-aware — never produces invalid UTF-8.
//
// A byte slice of a Go string can split a multi-byte rune (e.g. em-dash —,
// ellipsis …, CJK, emoji), leaving a dangling lead byte and an invalid UTF-8
// sequence. PostgreSQL (with server_encoding=UTF8) rejects such strings with
// SQLSTATE 22021 on write, so anything destined for persistence must use a
// rune-aware truncation.
//
// Edge cases:
//   - n <= 0: returns an empty string.
//   - n <= 3 with truncation needed: returns "..." truncated to n runes.
//   - utf8.RuneCountInString(s) <= n: returns s unchanged.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 3 {
		return "..."[:n]
	}
	return string([]rune(s)[:n-3]) + "..."
}

// TruncateRunesSuffix is like TruncateRunes but lets the caller choose the
// truncation suffix (e.g. "[... truncated]"). The returned string has at most
// n runes; if truncation is needed the suffix is appended and the prefix is
// shortened so the total rune count stays within n.
//
// If n is too small to fit the suffix, returns the suffix sliced to n runes.
func TruncateRunesSuffix(s, suffix string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	suffixRunes := utf8.RuneCountInString(suffix)
	if n <= suffixRunes {
		return string([]rune(suffix)[:n])
	}
	return string([]rune(s)[:n-suffixRunes]) + suffix
}
