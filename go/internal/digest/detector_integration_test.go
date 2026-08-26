//go:build integration

// Wissens-Ebenen V-W8 (design/05 §7 row V-W8): the asymmetry probe on a REAL
// in-process writer. digest.RunDigest goes straight to store.UpsertBlock
// (digest.go:146) and therefore never saw the handler-side G40 detector
// (handler/context_store.go:107, handler/stage_gates.go:81).
//
// The pattern reaches the topic-map through a block TITLE: RunDigest renders
// every block's title into the index content (digest.go:119, truncated to 70
// runes), so a seeded title carrying an AWS access key id shape puts a real
// credentials signal into a block that a background job writes.
//
// Run with:
//
//	go test -tags=integration ./internal/digest/ -run TestRunDigest_CredentialsDetector -count=1 -v
package digest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRunDigest_CredentialsDetectorRaisesTopicMap is the RED half of the V-W8
// asymmetry: before the wave the digest-written topic map carried the DDL
// defaults ('credentials'/'default', 113_baseline.sql:5474-5477) and no
// detector trace, while the identical content over POST /api/store was raised
// to credentials with sensitivity_source='pattern' and a metadata trace.
func TestRunDigest_CredentialsDetectorRaisesTopicMap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const homeScope = "private"
	readScopes := []string{homeScope}

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	// Synthetic AKIA shape (AKIA + 16 constant base32 chars) — sensitivity.Scan
	// rule "aws-key". 20 runes, well inside the 70-rune title truncation.
	seedDigestBlock(t, pool, homeScope, "leaked key AKIA"+strings.Repeat("Z", 16)+" in a title")

	if err := digest.RunDigest(ctx, pool, reg, digest.ModeFull, "", homeScope, homeScope, readScopes); err != nil {
		t.Fatalf("RunDigest: %v", err)
	}

	var (
		sens, source string
		md           map[string]any
		content      string
	)
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source, metadata, content FROM context_blocks
		 WHERE category = 'index' AND title = $1 AND NOT is_archived`,
		"topic-map-"+homeScope).Scan(&sens, &source, &md, &content); err != nil {
		t.Fatalf("read topic-map block: %v", err)
	}

	// Fixture self-check: without the pattern in the rendered content the
	// probe would prove nothing.
	if !strings.Contains(content, "AKIA"+strings.Repeat("Z", 16)) {
		t.Fatalf("topic-map content does not carry the seeded key shape — fixture broken:\n%s", content)
	}

	if sens != "credentials" {
		t.Errorf("sensitivity = %q, want credentials", sens)
	}
	if source != "pattern" {
		t.Errorf("sensitivity_source = %q, want pattern — the background writer must carry the detector too", source)
	}
	trace, ok := md["sensitivity_detector"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.sensitivity_detector missing, got metadata %v", md)
	}
	if trace["kind"] != "aws-key" || trace["reason"] != "AWS access key id pattern" {
		t.Errorf("trace = %v, want kind=aws-key reason=\"AWS access key id pattern\"", trace)
	}
	// The digest's own metadata must survive the detector.
	if md["source"] != "context-digest" || md["is_meta"] != true {
		t.Errorf("digest metadata lost: %v", md)
	}
}

// TestRunDigest_CleanCorpusUnchanged is the non-regression half: a corpus
// without any key shape leaves the topic map exactly where it was before V-W8
// — DDL defaults, no trace. Values read off the UNCHANGED binary.
func TestRunDigest_CleanCorpusUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const homeScope = "private"
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	seedDigestBlock(t, pool, homeScope, "an ordinary decision title")

	if err := digest.RunDigest(ctx, pool, reg, digest.ModeFull, "", homeScope, homeScope, []string{homeScope}); err != nil {
		t.Fatalf("RunDigest: %v", err)
	}

	var (
		sens, source string
		md           map[string]any
	)
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source, metadata FROM context_blocks
		 WHERE category = 'index' AND title = $1 AND NOT is_archived`,
		"topic-map-"+homeScope).Scan(&sens, &source, &md); err != nil {
		t.Fatalf("read topic-map block: %v", err)
	}
	if sens != "credentials" || source != "default" {
		t.Errorf("clean topic-map row = %s/%s, want credentials/default (the DDL defaults, unchanged)", sens, source)
	}
	if _, present := md["sensitivity_detector"]; present {
		t.Errorf("clean corpus grew a detector trace: %v", md)
	}
}
