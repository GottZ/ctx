//go:build integration

// Issue #491 point 7: ListIssuePushCandidates (and the comment listing) do
// not filter on type_name, so a block re-typed away from issue would still be
// pushed under its issue mapping. No write can reach that state: since
// migration 174, trg_block_retype refuses any UPDATE of type_name that moves
// an issue or a comment block (23514, ck_context_blocks_retype_bound), and the
// store's by-id update answers that refusal with store.ErrRetypeBound. This
// pins the refusal on the store's write and on a plain UPDATE, and the push
// that follows them.
//
//	go test -tags=integration ./internal/forge/ -run TestPushedEntityKeepsItsType -count=1 -v
package forge

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestPushedEntityKeepsItsType(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := enablePush(seedIFProject(t, pool, "i491retype"))
	issueID, commentID := i452Pulled(t, pool, proj, true)

	for _, id := range []string{issueID, commentID} {
		_, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{
			Type: str("knowledge"), Content: str("edited while re-typed"),
		}, []string{proj.Scope})
		if !errors.Is(err, store.ErrRetypeBound) {
			t.Fatalf("re-type of a pulled %s block through the store: err = %v, want ErrRetypeBound", getBlock(t, pool, proj.Scope, id).TypeName, err)
		}
		// Past the store: the row's own trigger.
		_, err = pool.Exec(ctx, `UPDATE context_blocks SET type_name = 'knowledge', content = 'edited while re-typed' WHERE id = $1::uuid`, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "ck_context_blocks_retype_bound" {
			t.Fatalf("re-type of a pulled block past the store: err = %v, want 23514 ck_context_blocks_retype_bound", err)
		}
		if b := getBlock(t, pool, proj.Scope, id); b.Content == "edited while re-typed" {
			t.Fatalf("a refused update wrote the content")
		}
	}
	spy := newPushSpy()
	if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || spy.wireCalls() != 0 {
		t.Fatalf("push after the refused re-types: %+v %v, %d wire writes — want none", res, err, spy.wireCalls())
	}
}
