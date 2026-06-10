package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/sealbox"
)

func sealForTest(t *testing.T, name, scope string, plaintext []byte) (keyHex, line string) {
	t.Helper()
	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	keyHex = hex.EncodeToString(raw)
	box, err := sealbox.New(keyHex, "")
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	nonce, ct, err := box.Seal(name, scope, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	line = base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(ct) + ":" + name + ":" + scope
	return keyHex, line
}

// mimeWrap reproduces PG's encode(bytea,'base64') line discipline: a newline
// every 76 chars (RFC 2045). Applied to the b64 FIELDS, exactly like a naked
// `SELECT encode(…) || ':' || …` would emit through psql -At.
func mimeWrap(b64 string) string {
	var sb strings.Builder
	for len(b64) > 76 {
		sb.WriteString(b64[:76])
		sb.WriteByte('\n')
		b64 = b64[76:]
	}
	sb.WriteString(b64)
	return sb.String()
}

func TestRunSecretDecrypt_RoundTrip(t *testing.T) {
	// 73 chars — realistic provider-key length (>41-byte gate threshold).
	plaintext := "sk-or-v1-" + strings.Repeat("0123456789abcdef", 4)[:62] // assembled: no key-shaped literal for secret scanners
	keyHex, line := sealForTest(t, "openrouter-main", "_global", []byte(plaintext))
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")

	var stdout, stderr bytes.Buffer
	code := runSecretDecrypt(strings.NewReader(line+"\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if stdout.String() != plaintext+"\n" {
		t.Errorf("stdout = %q, want plaintext + newline", stdout.String())
	}
}

// The break-glass emergency shape: MIME-wrapped base64 spanning multiple
// lines (any realistic ciphertext does). The full-stdin reader + CR/LF strip
// must handle it; a line-based reader demonstrably does NOT (red probe
// below) — that asymmetry is why runSecretDecrypt reads to EOF.
func TestRunSecretDecrypt_MIMEWrappedInput(t *testing.T) {
	plaintext := "sk-or-v1-" + strings.Repeat("0123456789abcdef", 4)[:62] // assembled: no key-shaped literal for secret scanners
	keyHex, line := sealForTest(t, "openrouter-main", "_global", []byte(plaintext))
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")

	parts := strings.SplitN(line, ":", 4)
	wrapped := parts[0] + ":" + mimeWrap(parts[1]) + ":" + parts[2] + ":" + parts[3] + "\n"
	if !strings.Contains(wrapped, "\n"+"") || strings.Count(wrapped, "\n") < 2 {
		t.Fatalf("test input did not wrap — ct too short? %q", wrapped)
	}

	var stdout, stderr bytes.Buffer
	code := runSecretDecrypt(strings.NewReader(wrapped), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d on MIME-wrapped input, stderr: %s", code, stderr.String())
	}
	if stdout.String() != plaintext+"\n" {
		t.Errorf("stdout = %q, want plaintext + newline", stdout.String())
	}

	// RED probe: the naive line-based implementation (bufio.Scanner, first
	// line only) gets just the first fragment and MUST fail on the same
	// input — while a short test value (≤41 bytes plaintext, single-line
	// b64) would mask exactly this bug. Negative proof that reading to EOF
	// is load-bearing, not style.
	scanner := bufio.NewScanner(strings.NewReader(wrapped))
	if !scanner.Scan() {
		t.Fatal("scanner: no first line")
	}
	firstLine := scanner.Text()
	var redOut, redErr bytes.Buffer
	if code := runSecretDecrypt(strings.NewReader(firstLine), &redOut, &redErr); code == 0 {
		t.Fatal("line-based fragment decrypted successfully — MIME probe is not demonstrating the failure mode")
	}
}

func TestRunSecretDecrypt_PrevKeyNote(t *testing.T) {
	plaintext := []byte("rotate-me-0123456789-0123456789-0123456789-0123456789")
	oldHex, line := sealForTest(t, "rotated", "_global", plaintext)

	rawNew := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(rawNew); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv(sealbox.EnvKey, hex.EncodeToString(rawNew))
	t.Setenv(sealbox.EnvKeyPrev, oldHex)

	var stdout, stderr bytes.Buffer
	code := runSecretDecrypt(strings.NewReader(line), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if stdout.String() != string(plaintext)+"\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "re-encrypt sweep pending") {
		t.Errorf("stderr should note the prev-key open: %s", stderr.String())
	}
}

func TestRunSecretDecrypt_Failures(t *testing.T) {
	keyHex, line := sealForTest(t, "k", "s", []byte("value-0123456789"))

	t.Run("missing master key", func(t *testing.T) {
		t.Setenv(sealbox.EnvKey, "")
		t.Setenv(sealbox.EnvKeyPrev, "")
		var stdout, stderr bytes.Buffer
		if code := runSecretDecrypt(strings.NewReader(line), &stdout, &stderr); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if stdout.Len() != 0 {
			t.Error("stdout must stay empty on failure")
		}
	})

	t.Run("wrong master key", func(t *testing.T) {
		raw := make([]byte, sealbox.KeySize)
		if _, err := rand.Read(raw); err != nil {
			t.Fatalf("rand: %v", err)
		}
		t.Setenv(sealbox.EnvKey, hex.EncodeToString(raw))
		t.Setenv(sealbox.EnvKeyPrev, "")
		var stdout, stderr bytes.Buffer
		if code := runSecretDecrypt(strings.NewReader(line), &stdout, &stderr); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if stdout.Len() != 0 {
			t.Error("stdout must stay empty on failure")
		}
	})

	t.Run("malformed stdin", func(t *testing.T) {
		t.Setenv(sealbox.EnvKey, keyHex)
		t.Setenv(sealbox.EnvKeyPrev, "")
		var stdout, stderr bytes.Buffer
		if code := runSecretDecrypt(strings.NewReader("not-a-record\n"), &stdout, &stderr); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
	})
}
