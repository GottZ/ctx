// Break-glass secret decrypt mode: `/ctx -secret-decrypt` (analogous to
// -health). openssl enc cannot do AES-GCM ("AEAD ciphers not supported",
// verified against OpenSSL 3.6.2) — host-side extraction therefore runs
// through the ctxd binary itself, driven by break-glass.sh.
//
// Reads ONLY CTX_SECRETS_KEY / CTX_SECRETS_KEY_PREV from env plus ONE record
// from stdin — no DB access, no config load. That keeps the mode usable via
// `docker run` even when the ctx container itself crash-loops (the
// break-glass scenario par excellence).
//
// File named sealbox.go: pre-commit Gate 1 blocks new *secret* basenames.

package main

import (
	"fmt"
	"io"

	"github.com/GottZ/ctx/internal/sealbox"
)

// runSecretDecrypt implements the -secret-decrypt mode. Input format (one
// record, read to EOF — NOT line-wise: PG's encode(bytea,'base64') is MIME
// and wraps every 76 chars, so realistic ciphertexts span multiple lines):
//
//	<nonce_b64>:<ct_b64>:<name>:<scope>
//
// name+scope ride along because the AAD binds them. Plaintext + '\n' goes to
// stdout; all diagnostics go to stderr. Returns the process exit code.
func runSecretDecrypt(stdin io.Reader, stdout, stderr io.Writer) int {
	box, err := sealbox.FromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "secret-decrypt: %v\n", err)
		return 1
	}

	input, err := io.ReadAll(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "secret-decrypt: read stdin: %v\n", err)
		return 1
	}
	nonce, ciphertext, name, scope, err := sealbox.DecodeLine(input)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "secret-decrypt: %v\n", err)
		return 1
	}

	plaintext, usedPrev, err := box.Open(name, scope, nonce, ciphertext)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "secret-decrypt: %v\n", err)
		return 1
	}
	if usedPrev {
		_, _ = fmt.Fprintf(stderr, "secret-decrypt: note: opened with %s — re-encrypt sweep pending\n", sealbox.EnvKeyPrev)
	}
	if _, err := stdout.Write(append(plaintext, '\n')); err != nil {
		_, _ = fmt.Fprintf(stderr, "secret-decrypt: write stdout: %v\n", err)
		return 1
	}
	return 0
}
