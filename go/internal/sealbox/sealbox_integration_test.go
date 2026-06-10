//go:build integration

// Integration test for the break-glass extraction path (F2-W3 / G04)
// against a real PG18 testcontainer — the REAL encode(bytea,'base64') MIME
// behavior, not a Go-built stdin string (encoding/base64 never wraps, and
// short values stay single-line: both would mask the bug by construction).
//
// Covers the G04 gate probe:
//   - >41-byte plaintext (realistic provider-key length, 73 chars) sealed
//     via the real store write path (PutSecret into context_secrets)
//   - naked encode(…,'base64') emits MIME line breaks (RFC 2045, every 76
//     chars) — asserted, this is the failure source
//   - RED: a line-based reader on the naked record gets only the first
//     fragment and fails to decrypt
//   - GREEN: the exact break-glass.sh SQL (replace(encode(…),E'\n',”)) +
//     full-input DecodeLine + Box.Open round-trips the plaintext
//   - the EOF-reading decoder also survives the naked multi-line record
//     (CR/LF strip — robustness against consumers without replace())
//
// Run with:
//
//	go test -tags=integration ./internal/sealbox/ -count=1 -v
package sealbox_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBreakGlassPsqlMIME_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	box, err := sealbox.New(hex.EncodeToString(raw), "")
	if err != nil {
		t.Fatalf("box: %v", err)
	}

	// 73 chars — over the 41-byte threshold below which the ct's base64
	// stays ≤76 chars and PG never wraps (the green-while-broken trap).
	plaintext := "sk-or-v1-" + strings.Repeat("0123456789abcdef", 4)[:62] // assembled: no key-shaped literal for secret scanners
	name, scope := "openrouter-main", store.GlobalScope

	nonce, ct, err := box.Seal(name, scope, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Real write path: PutSecret into context_secrets (051 schema).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if _, err := store.PutSecret(ctx, tx, name, scope, nonce, ct, 1, nil); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// --- RED side: naked encode() is MIME and breaks the record apart ---
	var nakedRecord string
	err = pool.QueryRow(ctx,
		`SELECT encode(nonce,'base64') ||':'|| encode(ciphertext,'base64')
		        ||':'|| name ||':'|| scope
		   FROM context_secrets WHERE name=$1 AND scope=$2`, name, scope,
	).Scan(&nakedRecord)
	if err != nil {
		t.Fatalf("naked select: %v", err)
	}
	if !strings.Contains(nakedRecord, "\n") {
		t.Fatalf("naked encode() emitted NO line break — MIME probe is not exercising the failure mode (ct b64 len %d)", len(nakedRecord))
	}

	// A line-based reader (the naive decrypt-mode implementation) sees only
	// the first fragment — decode/decrypt MUST fail. Red proof that the
	// EOF-read + CR/LF-strip in DecodeLine is load-bearing.
	scanner := bufio.NewScanner(strings.NewReader(nakedRecord))
	if !scanner.Scan() {
		t.Fatal("scanner: no first line")
	}
	firstFragment := scanner.Text()
	if n, c, fn, fs, decErr := sealbox.DecodeLine([]byte(firstFragment)); decErr == nil {
		if _, _, openErr := box.Open(fn, fs, n, c); openErr == nil {
			t.Fatal("RED-proof failed: line-based fragment decrypted — probe does not demonstrate the MIME failure")
		}
	}

	// --- GREEN side: the exact break-glass.sh SQL round-trips ---
	var record string
	err = pool.QueryRow(ctx,
		`SELECT replace(encode(nonce,'base64'), E'\n', '')
		        ||':'|| replace(encode(ciphertext,'base64'), E'\n', '')
		        ||':'|| name ||':'|| scope
		   FROM context_secrets WHERE name=$1 AND scope=$2`, name, scope,
	).Scan(&record)
	if err != nil {
		t.Fatalf("break-glass select: %v", err)
	}
	if strings.Contains(record, "\n") {
		t.Fatalf("replace()-SQL still contains line breaks: %q", record)
	}

	gotNonce, gotCt, gotName, gotScope, err := sealbox.DecodeLine([]byte(record + "\n"))
	if err != nil {
		t.Fatalf("decode break-glass record: %v", err)
	}
	got, usedPrev, err := box.Open(gotName, gotScope, gotNonce, gotCt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if usedPrev {
		t.Error("usedPrev = true, want false")
	}
	if string(got) != plaintext {
		t.Errorf("plaintext mismatch: got %d bytes, want %d", len(got), len(plaintext))
	}

	// --- Robustness: the EOF-reading decoder also survives the NAKED
	// multi-line record (psql output piped without replace()) ---
	n2, c2, fn2, fs2, err := sealbox.DecodeLine([]byte(nakedRecord))
	if err != nil {
		t.Fatalf("decode naked multi-line record: %v", err)
	}
	got2, _, err := box.Open(fn2, fs2, n2, c2)
	if err != nil {
		t.Fatalf("open naked record: %v", err)
	}
	if string(got2) != plaintext {
		t.Error("naked-record plaintext mismatch")
	}
}
