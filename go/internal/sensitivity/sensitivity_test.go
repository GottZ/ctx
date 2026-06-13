package sensitivity

import (
	"math"
	"strings"
	"testing"
)

// Secret-shaped fixtures are ASSEMBLED AT RUNTIME from fragments — never a
// contiguous secret-shaped literal in source. GitHub Push Protection matches
// the SHAPE even on synthetic values (Phase-0 lesson); a literal token in a
// test file would block the push.
func awsKey() string   { return "AKIA" + strings.Repeat("Z", 16) }
func ghToken() string  { return "ghp_" + strings.Repeat("a", 36) }
func skToken() string  { return "sk-" + strings.Repeat("b", 28) }
func jwt() string      { return "eyJ" + strings.Repeat("a", 12) + "." + "eyJ" + strings.Repeat("b", 14) + "." + strings.Repeat("c", 20) }
func hex64() string    { return strings.Repeat("0123456789abcdef", 4) } // 64 hex
func sha1Hex() string  { return strings.Repeat("a1b2", 10) }            // 40 hex (git SHA-1)
func base64Key() string {
	// 40 chars, mixed alnum — high entropy (~5.2 bits/char), no provider shape.
	return "aB3xK9mP2qR7vT5wZ1nL8jH4gF6dS0cVeY2uI4oP"
}

func TestScanPositives(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind string
	}{
		{"aws key", "deploy uses " + awsKey() + " for upload", "aws-key"},
		{"pem private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIE...", "pem-private-key"},
		{"pem openssh", "-----BEGIN OPENSSH PRIVATE KEY-----", "pem-private-key"},
		{"github token", "export TOKEN=" + ghToken(), "token-prefix"},
		{"openai token", "client(api_key='" + skToken() + "')", "token-prefix"},
		{"jwt", "Authorization: Bearer " + jwt(), "jwt"},
		{"secret assignment", "DB_PASSWORD=" + base64Key(), "secret-assignment"},
		{"base64 blob", "key blob " + base64Key() + " stored", "base64-blob"},
		{"hex 64", "raw key " + hex64(), "hex-blob"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, hit := Scan(c.in)
			if !hit {
				t.Fatalf("expected hit, got none")
			}
			if m.Kind != c.kind {
				t.Errorf("kind = %q, want %q", m.Kind, c.kind)
			}
			if strings.Contains(m.Reason, awsKey()) || strings.Contains(m.Reason, ghToken()) ||
				strings.Contains(m.Reason, skToken()) || strings.Contains(m.Reason, base64Key()) {
				t.Errorf("Reason must never echo the matched secret: %q", m.Reason)
			}
		})
	}
}

// TestScanNegatives covers the corpus's false-positive sources: a knowledge
// store ABOUT ctx development is full of git SHAs, prose about secrets, and
// templated config examples. None may flag.
func TestScanNegatives(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"plain prose", "The query path resolves sensitivity from the settings default."},
		{"prose about secrets", "The secret is stored via sealbox; the api_key_ref names an F2 secret, never the value."},
		{"git short sha", "Commit de9207c feat(llmlog): time-based body retention"},
		{"git sha-1 full", "rev " + sha1Hex() + " is the merge base"},
		{"placeholder password", "set password = changeme in the example config"},
		{"placeholder token", "api_key: <your-token>"},
		{"env var placeholder", "export API_KEY=${CTX_ADMIN_KEY}"},
		{"your-prefix placeholder", "token=your-token-goes-here"},
		{"pem certificate public", "-----BEGIN CERTIFICATE-----\nMIID..."},
		{"short hex uuid-like", "id 550e8400e29b41d4a716446655440000 row"},
		{"markdown table", "| password | the user secret | required |"},
		{"settings key name", "pool.default_block_sensitivity guard: sensitivity-downgrade"},
		{"low-entropy assignment", "password=aaaaaaaa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if m, hit := Scan(c.in); hit {
				t.Errorf("expected NO hit, got %s (%s)", m.Kind, m.Reason)
			}
		})
	}
}

func TestShannonEntropy(t *testing.T) {
	if e := shannonEntropy(""); e != 0 {
		t.Errorf("empty entropy = %v, want 0", e)
	}
	if e := shannonEntropy("aaaaaaaa"); e != 0 {
		t.Errorf("uniform char entropy = %v, want 0", e)
	}
	// "ab" alternating: two symbols, equal freq → exactly 1 bit/char.
	if e := shannonEntropy("abab"); math.Abs(e-1.0) > 1e-9 {
		t.Errorf("two-symbol entropy = %v, want 1.0", e)
	}
	// A high-entropy base64 key must clear the 4.5 gate.
	if e := shannonEntropy(base64Key()); e < base64MinEntropy {
		t.Errorf("base64 key entropy = %v, want >= %v", e, base64MinEntropy)
	}
	// Hex (16 symbols) caps near 4.0 — below the base64 gate by construction.
	if e := shannonEntropy(hex64()); e >= base64MinEntropy {
		t.Errorf("hex entropy = %v, must stay below base64 gate %v", e, base64MinEntropy)
	}
}

func TestPlaceholderValue(t *testing.T) {
	for _, p := range []string{"changeme", "<your-token>", "${VAR}", "xxxxxxxx", "your-secret", "TODO"} {
		if !placeholderValue(p) {
			t.Errorf("%q should be a placeholder", p)
		}
	}
	for _, real := range []string{base64Key(), "p7Kx9mQ2vR8w"} {
		if placeholderValue(real) {
			t.Errorf("%q should NOT be a placeholder", real)
		}
	}
}
