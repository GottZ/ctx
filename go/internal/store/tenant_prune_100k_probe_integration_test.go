//go:build integration

// Issue #555 (a measurement): a tenant-delete of 100 000 blocks over two
// scopes, without writers beside it — the same file runs on the base and on
// this branch, so the owner test the prune's statements carry since #555 is
// the difference. Opt-in:
//
//	CTX_P555_PRUNE_PLAIN=1 go test -tags=integration -p 1 ./internal/store/ -run TestPrune100kPlain -v -timeout 30m
package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestPrune100kPlain(t *testing.T) {
	if os.Getenv("CTX_P555_PRUNE_PLAIN") == "" {
		t.Skip("measurement: set CTX_P555_PRUNE_PLAIN=1")
	}
	f := newP547Fixture(t)
	ctx := context.Background()
	const blocks = 100_000
	tenantID, scopes, _ := pruneWritersTenant(t, f, "plain100k", 2)
	for n := 0; n < blocks; n += 1000 {
		items := make([]store.BatchUpsert, 1000)
		for i := range items {
			items[i] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("plain %d", n+i), Content: fmt.Sprintf("block %d", n+i),
				Scope: scopes[(n/1000)%2], ScopeExplicit: true}
		}
		if _, err := store.UpsertBlocks(ctx, f.pool, items, "", f.set, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertBlob(ctx, f.pool, "files", fmt.Sprintf("plain blob %d", n), "f.txt", "text/plain", scopes[(n/1000)%2], []byte("x"), nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `ANALYZE context_blocks; ANALYZE context_blobs`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	t.Logf("prune of %d blocks and 100 blobs over 2 scopes, no writer beside it: %s", blocks, time.Since(start).Round(time.Millisecond))
}
