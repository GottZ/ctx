//go:build integration

// WF T8 digest.include gate against real PG (design/01 §7-T8 / §4.4 #13).
// RED state proven against the pre-T8 tree on 2026-07-02 with a scratch
// probe (deleted in this wave): a block of a registered type with
// digest.include=false WAS in the topic-map source ("digest.include=false
// block is in the topic-map source"), because fetchBlockMeta had no type
// sieve at all. This test pins the GREEN contract on the same fixture.
package digest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestRunDigest_IncludeFalseType_AbsentFromTopicMap(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	homeScope := "private"

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, config)
		 VALUES ('wf-nodigest', '_global', '{"v":1,"digest":{"include":false}}'::jsonb)`); err != nil {
		t.Fatalf("register type: %v", err)
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	seedDigestBlock(t, pool, homeScope, "plain-knowledge-block")
	seedDigestBlock(t, pool, homeScope, "hidden-nodigest-block")
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET type_name = 'wf-nodigest' WHERE title = 'hidden-nodigest-block'`); err != nil {
		t.Fatalf("set type: %v", err)
	}

	if err := digest.RunDigest(ctx, pool, reg, homeScope, []string{homeScope}); err != nil {
		t.Fatalf("RunDigest: %v", err)
	}

	var content string
	if err := pool.QueryRow(ctx,
		`SELECT content FROM context_blocks WHERE category = 'index' AND title = $1`,
		"topic-map-"+homeScope).Scan(&content); err != nil {
		t.Fatalf("read topic map: %v", err)
	}
	if !strings.Contains(content, "plain-knowledge-block") {
		t.Errorf("topic map lost the knowledge block")
	}
	if strings.Contains(content, "hidden-nodigest-block") {
		t.Errorf("digest.include=false block is in the topic-map source")
	}
}

// TestRunDigest_NilRegistry_FailsLoud pins the WF T8 wiring contract: since
// the source sieve needs the type allowlist, a nil registry is a loud error
// — never a silent unfiltered digest.
func TestRunDigest_NilRegistry_FailsLoud(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	if err := digest.RunDigest(context.Background(), pool, nil, "private", []string{"private"}); err == nil {
		t.Fatal("RunDigest with nil registry must fail loudly, got nil error")
	}
}
