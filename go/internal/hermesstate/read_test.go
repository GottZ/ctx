// read_test.go — the ist-fixture gates: the numbers of inventory §1.3/§1.4,
// the watermark derivation of design §3.2, and the content sentinel of §0.4.
//
// Source: https://github.com/GottZ/ctx
package hermesstate_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/hermesstate"
)

func openIst(t *testing.T, withSessionIDIndex bool) (*hermesstate.Source, istFixture) {
	t.Helper()
	fx := buildIst(t, t.TempDir(), withSessionIDIndex)
	src, err := hermesstate.Open(fx.Path, "ist", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src, fx
}

func TestIstFixtureShape(t *testing.T) {
	fx := buildIst(t, t.TempDir(), false)
	if got := countRows(t, fx.Path, "compacted = 1"); got != 910 {
		t.Errorf("archived rows = %d, want 910", got)
	}
	if got := countRows(t, fx.Path, "active = 1"); got != 399 {
		t.Errorf("live rows = %d, want 399", got)
	}
	if got := countRows(t, fx.Path, "compacted = 1 AND id BETWEEN 3 AND 930"); got != 910 {
		t.Errorf("archived rows inside 3..930 = %d, want 910", got)
	}
	if got := countRows(t, fx.Path, "active = 1 AND id BETWEEN 931 AND 1329"); got != 399 {
		t.Errorf("live rows inside 931..1329 = %d, want 399", got)
	}
	if got := countRows(t, fx.Path, "compacted = 1 AND role = 'tool'"); got != 418 {
		t.Errorf("archived tool rows = %d, want 418", got)
	}
}

func TestMaxCompactedIDAndHasNewArchived(t *testing.T) {
	src, _ := openIst(t, false)
	ctx := t.Context()

	maxID, err := src.MaxCompactedID(ctx, istSessionID)
	if err != nil {
		t.Fatalf("MaxCompactedID: %v", err)
	}
	if maxID != 930 {
		t.Errorf("MaxCompactedID = %d, want 930", maxID)
	}

	global, err := src.GlobalMaxID(ctx)
	if err != nil {
		t.Fatalf("GlobalMaxID: %v", err)
	}
	if global != 1329 {
		t.Errorf("GlobalMaxID = %d, want 1329", global)
	}

	for _, tc := range []struct {
		after int64
		want  bool
	}{{0, true}, {929, true}, {930, false}, {1329, false}} {
		got, err := src.HasNewArchived(ctx, istSessionID, tc.after)
		if err != nil {
			t.Fatalf("HasNewArchived(%d): %v", tc.after, err)
		}
		if got != tc.want {
			t.Errorf("HasNewArchived(%d) = %v, want %v", tc.after, got, tc.want)
		}
	}

	// An unknown session is empty, not an error: a source may legitimately
	// carry sessions this caller has never seen.
	got, err := src.HasNewArchived(ctx, "no-such-session", 0)
	if err != nil || got {
		t.Errorf("HasNewArchived(unknown) = %v, %v; want false, nil", got, err)
	}
}

func TestToolRowsBatch(t *testing.T) {
	src, fx := openIst(t, false)
	ctx := t.Context()

	rows, dropped, err := src.ToolRows(ctx, istSessionID, 0, 400)
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(rows) > 400 {
		t.Fatalf("len(rows) = %d, want <= 400", len(rows))
	}
	if len(rows) != 400 {
		t.Errorf("len(rows) = %d, want 400 (the fixture holds 418 archived tool rows)", len(rows))
	}

	var prev int64
	for _, r := range rows {
		if r.ID > 930 {
			t.Fatalf("row id %d is above the archived watermark 930", r.ID)
		}
		if !fx.ToolIDs[r.ID] {
			t.Fatalf("row id %d is not an archived tool row", r.ID)
		}
		if r.ID <= prev {
			t.Fatalf("rows are not ascending: %d after %d", r.ID, prev)
		}
		prev = r.ID
		if r.ToolName == "" {
			t.Errorf("row %d has no tool name", r.ID)
		}
		if strings.Contains(r.Content, "\x00") {
			t.Errorf("row %d content carries a NUL byte", r.ID)
		}
	}

	// The rest of the batch series: the remaining 18 rows, then nothing.
	rest, _, err := src.ToolRows(ctx, istSessionID, prev, 400)
	if err != nil {
		t.Fatalf("ToolRows rest: %v", err)
	}
	if len(rest) != 18 {
		t.Errorf("second batch = %d rows, want 18", len(rest))
	}
	tail, _, err := src.ToolRows(ctx, istSessionID, 930, 400)
	if err != nil {
		t.Fatalf("ToolRows tail: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("batch above the archived head = %d rows, want 0", len(tail))
	}

	if none, _, err := src.ToolRows(ctx, istSessionID, 0, 0); err != nil || none != nil {
		t.Errorf("ToolRows(limit=0) = %v, %v; want nil, nil", none, err)
	}
}

func TestQuietFor(t *testing.T) {
	src, fx := openIst(t, false)
	ctx := t.Context()

	now := fx.NewestTS.Add(17 * time.Minute)
	d, err := src.QuietFor(ctx, istSessionID, now)
	if err != nil {
		t.Fatalf("QuietFor: %v", err)
	}
	if d < 17*time.Minute-time.Second || d > 17*time.Minute+time.Second {
		t.Errorf("QuietFor = %v, want ~17m", d)
	}

	// A clock that runs behind the store (hermes orders by id rather than by
	// timestamp for exactly this reason) must not yield a negative idle time.
	if d, err := src.QuietFor(ctx, istSessionID, fx.NewestTS.Add(-time.Hour)); err != nil || d != 0 {
		t.Errorf("QuietFor with a lagging clock = %v, %v; want 0, nil", d, err)
	}

	if _, err := src.QuietFor(ctx, "no-such-session", now); !errors.Is(err, hermesstate.ErrNoActiveRows) {
		t.Errorf("QuietFor(unknown) error = %v, want ErrNoActiveRows", err)
	}
}

func TestSessionsRootChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db := newStore(t, path, false)
	base := istBaseTS
	insertSession(t, db, "root", "", base)
	insertSession(t, db, "child", "root", base.Add(time.Hour))
	insertSession(t, db, "grand", "child", base.Add(2*time.Hour))
	insertSession(t, db, "loop-a", "loop-b", base)
	insertSession(t, db, "loop-b", "loop-a", base)
	ts := float64(base.Add(3*time.Hour).UnixNano()) / 1e9
	mustExec(t, db, `INSERT INTO messages(id, session_id, role, content, timestamp, active, compacted)
		VALUES(1,'grand','assistant','live',?,1,0)`, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	src, err := hermesstate.Open(path, "chain", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()

	got, err := src.Sessions(t.Context())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	roots := map[string]string{}
	active := map[string]time.Time{}
	for _, s := range got {
		roots[s.ID] = s.RootID
		active[s.ID] = s.NewestActive
	}
	for id, want := range map[string]string{"root": "root", "child": "root", "grand": "root"} {
		if roots[id] != want {
			t.Errorf("root of %s = %q, want %q", id, roots[id], want)
		}
	}
	// A parent cycle in foreign data must terminate, not hang. Which member of
	// the cycle is reported is arbitrary; that it is one of them is not.
	if r := roots["loop-a"]; r != "loop-a" && r != "loop-b" {
		t.Errorf("root of loop-a = %q, want a member of the cycle", r)
	}
	if active["grand"].IsZero() {
		t.Error("grand has a live row but no newest-active time")
	}
	if !active["root"].IsZero() {
		t.Errorf("root has no live rows but reports %v", active["root"])
	}
}

// TestWatermarkFromCountsRowsNotIDs is the §3.2 negative probe. Two interleaved
// sessions share one global id space; the naive max_compacted_id − N formula
// lands somewhere else entirely, and this test states both numbers so the
// difference is visible rather than asserted.
func TestWatermarkFromCountsRowsNotIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db := newStore(t, path, false)
	base := istBaseTS
	insertSession(t, db, "target", "", base)
	insertSession(t, db, "other", "", base)

	// Strict interleave: odd ids belong to the target session, even ids to the
	// other one. This is the live shape (307 sessions sharing one AUTOINCREMENT).
	var targetIDs []int64
	for id := int64(1); id <= 40; id++ {
		sess := "other"
		if id%2 == 1 {
			sess = "target"
			targetIDs = append(targetIDs, id)
		}
		ts := float64(base.Add(time.Duration(id)*time.Second).UnixNano()) / 1e9
		mustExec(t, db, `INSERT INTO messages(id, session_id, role, content, tool_name, timestamp, active, compacted)
			VALUES(?,?,'tool',?, 'probe', ?, 0, 1)`, id, sess, "row", ts)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	src, err := hermesstate.Open(path, "backfill", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()
	ctx := t.Context()

	maxID, err := src.MaxCompactedID(ctx, "target")
	if err != nil {
		t.Fatalf("MaxCompactedID: %v", err)
	}
	if maxID != 39 {
		t.Fatalf("MaxCompactedID = %d, want 39", maxID)
	}

	const backfill = 5
	wm, err := src.WatermarkFrom(ctx, "target", backfill)
	if err != nil {
		t.Fatalf("WatermarkFrom: %v", err)
	}

	// The contract, stated as the thing the caller actually wants: exactly
	// `backfill` rows OF THE TARGET SESSION lie above the watermark.
	above, _, err := src.ToolRows(ctx, "target", wm, 100)
	if err != nil {
		t.Fatalf("ToolRows above watermark: %v", err)
	}
	if len(above) != backfill {
		t.Errorf("WatermarkFrom(%d) leaves %d target rows above it, want %d (watermark id %d)",
			backfill, len(above), backfill, wm)
	}
	if want := targetIDs[len(targetIDs)-backfill-1]; wm != want {
		t.Errorf("WatermarkFrom(%d) = %d, want %d", backfill, wm, want)
	}

	// The naive formula, run here so the gate shows the delta instead of
	// claiming it: on an interleaved store it leaves half the rows it promised.
	naive := maxID - backfill
	naiveAbove, _, err := src.ToolRows(ctx, "target", naive, 100)
	if err != nil {
		t.Fatalf("ToolRows above naive watermark: %v", err)
	}
	if len(naiveAbove) == backfill {
		t.Errorf("max_compacted_id - N yielded %d rows too — the fixture is not interleaved enough to prove the difference", backfill)
	}
	t.Logf("interleaved store: WatermarkFrom(%d) = %d leaves %d rows; max_compacted_id-%d = %d leaves %d rows",
		backfill, wm, len(above), backfill, naive, len(naiveAbove))

	// N = 0 means "start at the head": nothing is reprocessed.
	head, err := src.WatermarkFrom(ctx, "target", 0)
	if err != nil {
		t.Fatalf("WatermarkFrom(0): %v", err)
	}
	if head != maxID {
		t.Errorf("WatermarkFrom(0) = %d, want the archived head %d", head, maxID)
	}

	// Asking for more history than exists starts from the beginning.
	all, err := src.WatermarkFrom(ctx, "target", 10_000)
	if err != nil {
		t.Fatalf("WatermarkFrom(10000): %v", err)
	}
	if all != 0 {
		t.Errorf("WatermarkFrom(10000) = %d, want 0", all)
	}
}

// TestContentSentinel is the §0.4 gate: multimodal payloads are folded to their
// text, a broken payload costs its row, and no NUL byte leaves the package.
func TestContentSentinel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db := newStore(t, path, false)
	insertSession(t, db, "s", "", istBaseTS)

	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	rows := []struct {
		id      int64
		content string
	}{
		{1, "plain tool output"},
		{2, "\x00json:" + `[{"type":"text","text":"first part"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}},{"type":"text","text":"second part"}]`},
		{3, "\x00json:[{\"type\":\"text\",\"text\":\"truncated"},
		{4, "\x00json:" + `{"type":"text","text":"single part"}`},
		{5, "\x00json:" + `[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]`},
		{6, "carriage \x00 sneaked in"},
	}
	for _, r := range rows {
		ts := float64(istBaseTS.Add(time.Duration(r.id)*time.Second).UnixNano()) / 1e9
		mustExec(t, db, `INSERT INTO messages(id, session_id, role, content, tool_name, timestamp, active, compacted)
			VALUES(?,'s','tool',?, 'probe', ?, 0, 1)`, r.id, r.content, ts)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	src, err := hermesstate.Open(path, "sentinel", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()

	got, dropped, err := src.ToolRows(t.Context(), "s", 0, 50)
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if dropped != 1 {
		t.Errorf("rows_dropped_enc = %d, want 1", dropped)
	}
	if len(got) != 5 {
		t.Fatalf("len(rows) = %d, want 5 (6 stored, 1 dropped)", len(got))
	}
	byID := map[int64]string{}
	for _, r := range got {
		if strings.Contains(r.Content, "\x00") {
			t.Errorf("row %d carries a NUL byte: %q", r.ID, r.Content)
		}
		if strings.Contains(r.Content, b64) {
			t.Errorf("row %d carries a base64 blob", r.ID)
		}
		if strings.Contains(r.Content, "image_url") {
			t.Errorf("row %d carries a non-text part: %q", r.ID, r.Content)
		}
		byID[r.ID] = r.Content
	}
	if _, present := byID[3]; present {
		t.Error("row 3 has an unparseable payload and must have been dropped")
	}
	if want := "first part\nsecond part"; byID[2] != want {
		t.Errorf("row 2 content = %q, want %q", byID[2], want)
	}
	if want := "single part"; byID[4] != want {
		t.Errorf("row 4 content = %q, want %q", byID[4], want)
	}
	if byID[5] != "" {
		t.Errorf("row 5 is image-only, content = %q, want empty", byID[5])
	}
	if want := "carriage  sneaked in"; byID[6] != want {
		t.Errorf("row 6 content = %q, want %q", byID[6], want)
	}
	if byID[1] != "plain tool output" {
		t.Errorf("row 1 content = %q", byID[1])
	}
}

// TestAutocommitDiscipline proves invariant 4 the only way a test can: with a
// single connection in the pool, a read that left a transaction open would
// deadlock the next one. Two hundred batch reads in sequence against a bounded
// context are the probe — a leaked read mark shows up as a timeout, not as a
// wrong number.
func TestAutocommitDiscipline(t *testing.T) {
	src, _ := openIst(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for i := range 200 {
		if _, _, err := src.ToolRows(ctx, istSessionID, int64(i), 5); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		if _, err := src.HasNewArchived(ctx, istSessionID, int64(i)); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
}
