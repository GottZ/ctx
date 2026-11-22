//go:build integration

// Issue #491 (second review, P1): a fetch older than the forge observation the
// mapping records writes nothing (olderOrUnordered) — also without a moved
// base, which closes the window between a pull's fetch and its read of the
// mapping. When the forge's updated_at went back behind the recorded stamp (a
// clock set back, a restore of the forge from a backup, a manual change),
// every pull skips the entity although the forge carries something else, and
// a later local edit is pushed over it. The skip is now visible: a WARN once
// per entity and run, and pull_skipped in the run state and the run row's
// stats. The ordinary older fetch — the same version as the base, at an
// earlier stamp — says nothing. The way out for an operator is the mapping's
// forge_updated_at set to NULL: the next pull decides on content again.
//
//	go test -tags=integration ./internal/forge/ -run 'TestPullReportsAFetchOlderThanItsMappingRecords|TestPullOfAnOlderFetchOfTheBaseVersionSaysNothing' -count=1 -v
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i491LogBuffer captures the process log for the test (restored at its end).
type i491LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *i491LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// count answers how many log lines carry s.
func (b *i491LogBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

func i491CaptureLog(t *testing.T) *i491LogBuffer {
	t.Helper()
	b := &i491LogBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

const i491StaleWarn = "forge: a pull skipped a fetch older than the forge version its mapping records"

// i491Run runs one sync of the project through the SyncManager (the pull
// legs on the fake forge, the production Applier) and returns its state and
// its run row's stats.
func (e i491Env) i491Run(t *testing.T) (SyncStatus, map[string]int) {
	t.Helper()
	ctx := context.Background()
	// The project points at the fake forge (loopback, which validateForge
	// refuses as an api_base; the row is set directly), never at GitHub.
	proj := e.proj
	proj.Forge = json.RawMessage(`{"kind":"github","owner":"o","repo":"r","api_base":"` + e.gh.srv.URL + `"}`)
	if _, err := e.pool.Exec(ctx, `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`, proj.ID, string(proj.Forge)); err != nil {
		t.Fatal(err)
	}
	m := mgr(e.pool, okTenant, okPolicy, e.gh.client(), e.applier(e.pool).ApplyIssues)
	if _, err := m.StartSync(ctx, proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, e.proj.ID)
	var run *store.SyncRunRow
	var err error
	for i := 0; i < 400; i++ {
		if run, err = store.LatestSyncRun(ctx, e.pool, e.proj.ID); err != nil || (run != nil && run.Status != "running") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || run == nil || run.Status != "done" {
		t.Fatalf("run row: %+v %v, want a done run (state %+v)", run, err, st)
	}
	var stats map[string]int
	if err := json.Unmarshal(run.Stats, &stats); err != nil {
		t.Fatal(err)
	}
	return st, stats
}

func TestPullReportsAFetchOlderThanItsMappingRecords(t *testing.T) {
	logs := i491CaptureLog(t)
	e := i491Setup(t, "i491staleskip", "", fakeIssue{Title: "Crane", Body: "forge body"})
	ctx := context.Background()
	recorded := e.gh.issue(t, 5).Updated
	// The forge's clock went back: a remote edit carries a stamp before the
	// one the mapping recorded at the pull.
	e.gh.editAt(t, 5, recorded.Add(-time.Hour), func(it *fakeIssue) { it.Body = "remote edit after the forge's clock went back" })

	for run := 1; run <= 3; run++ {
		st, stats := e.i491Run(t)
		if st.Applied != 0 || st.Conflicts != 0 || st.PullSkipped != 1 || stats["pull_skipped"] != 1 {
			t.Errorf("run %d: applied %d, conflicts %d, pull_skipped %d (run row %d) — want the skip counted once", run, st.Applied, st.Conflicts, st.PullSkipped, stats["pull_skipped"])
		}
		if n := logs.count(i491StaleWarn); n != run {
			t.Errorf("after run %d: %d WARN line(s), want one per run", run, n)
		}
	}
	if !strings.Contains(e.body(t), "forge body") {
		t.Fatalf("precondition: the block holds %q, want the old body (the skip)", e.body(t))
	}

	// One run lists the entity twice (two pages): one report.
	before := logs.count(i491StaleWarn)
	runCtx := withStaleSeen(ctx)
	page := e.fetch(t)
	var skipped int
	for i := 0; i < 2; i++ {
		r, err := e.applier(e.pool).ApplyIssues(runCtx, e.proj, page)
		if err != nil {
			t.Fatal(err)
		}
		skipped += r.Skipped
	}
	if n := logs.count(i491StaleWarn) - before; n != 1 || skipped != 1 {
		t.Errorf("one run applying the entity twice: %d WARN line(s), %d counted — want one each", n, skipped)
	}

	// The way out: the mapping's forge_updated_at set to NULL; the next pull
	// decides on content (block at the base, forge ahead) and writes.
	if _, err := e.pool.Exec(ctx, `UPDATE context_project_sync_map SET forge_updated_at = NULL WHERE block_id = $1::uuid`, e.id(t)); err != nil {
		t.Fatal(err)
	}
	before = logs.count(i491StaleWarn)
	if r := e.pullOn(t, e.pool); r.Applied != 1 || r.Skipped != 0 {
		t.Errorf("the pull after the reset: %+v, want the remote edit applied", r)
	}
	if got := e.body(t); got != "remote edit after the forge's clock went back" || logs.count(i491StaleWarn) != before {
		t.Errorf("after the reset the block holds %q (%d new WARN line(s)), want the remote edit and none", got, logs.count(i491StaleWarn)-before)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
}

// TestPullOfAnOlderFetchOfTheBaseVersionSaysNothing: the ordinary older
// fetch. This pull fetched a version; the forge bumps its updated_at without
// changing what the projection carries (a comment, a reaction), and another
// instance pulls the bumped one before this pull writes. The fetch is older
// than the row's stamp and is the base's version: nothing to write, nothing
// to report.
func TestPullOfAnOlderFetchOfTheBaseVersionSaysNothing(t *testing.T) {
	logs := i491CaptureLog(t)
	e := i491Setup(t, "i491stalesame", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge" })
	res := e.pullHeldOnPage(t, e.fetch(t), func() {
		e.gh.edit(t, 5, func(*fakeIssue) {}) // a newer stamp, the same content
		if r := e.pullOn(t, e.pool); r.Applied != 1 {
			t.Fatalf("other instance's pull of the bumped version: %+v", r)
		}
	})
	if res.Applied != 0 || res.Conflicts != 0 || res.Skipped != 0 {
		t.Errorf("the pull of the older fetch of the base's version: %+v, want nothing written, nothing counted", res)
	}
	if n := logs.count(i491StaleWarn); n != 0 {
		t.Errorf("%d WARN line(s) for the ordinary older fetch, want none", n)
	}
}
