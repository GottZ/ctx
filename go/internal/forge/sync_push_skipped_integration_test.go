//go:build integration

// Issue #491 (review of the first round, F6): an entity the push leaves to
// the next run — another instance's push holds it, wrote it since the
// listing, or a pull flagged it since — is counted, and the count appears
// where the other counters of a sync run do: the in-memory run state that
// forge-sync-status serves (push_skipped) and the stats of the run row.
//
//	go test -tags=integration ./internal/forge/ -run TestSyncRunReportsTheEntitiesThePushLeft -count=1 -v
package forge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSyncRunReportsTheEntitiesThePushLeftToTheNextRun(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := enablePush(seedIFProject(t, pool, "i491skipcount"))

	// The push leg runs only with a resolved token: seal one for the project.
	box, err := sealbox.New(hex.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "forge_token"
	nonce, ct, err := box.Seal(secret, proj.Scope, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutSecret(ctx, tx, secret, proj.Scope, nonce, ct, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectToken(ctx, tx, proj.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	name := secret
	proj.TokenSecret = &name

	m := mgr(pool, okTenant, okPolicy, &fakeForge{}, nil)
	m.openBox = func() (*sealbox.Box, error) { return box, nil }
	m.throttle = NewThrottle()
	m.SetPush(func(context.Context, store.ProjectRow, Forge, RepoRef, func() bool) (PushResult, error) {
		return PushResult{Pushed: 1, Skipped: 2}, nil
	})
	if _, err := m.StartSync(ctx, proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, proj.ID)
	if st.Pushed != 1 || st.PushSkipped != 2 || st.LastError != "" {
		t.Errorf("run state: pushed %d, push_skipped %d, last_error %q — want 1, 2, none", st.Pushed, st.PushSkipped, st.LastError)
	}
	raw, err := json.Marshal(st)
	if err != nil || !strings.Contains(string(raw), `"push_skipped":2`) {
		t.Errorf("the run state as forge-sync-status serves it: %s %v, want push_skipped 2", raw, err)
	}
	// The run row is closed after the in-memory state (finish).
	var run *store.SyncRunRow
	for i := 0; i < 400; i++ {
		if run, err = store.LatestSyncRun(ctx, pool, proj.ID); err != nil || (run != nil && run.Status != "running") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || run == nil || run.Status != "done" {
		t.Fatalf("latest run: %+v %v, want a done run", run, err)
	}
	var stats map[string]int
	if err := json.Unmarshal(run.Stats, &stats); err != nil {
		t.Fatal(err)
	}
	if stats["push_skipped"] != 2 || stats["pushed"] != 1 {
		t.Errorf("run row stats %v, want push_skipped 2 and pushed 1", stats)
	}
}
