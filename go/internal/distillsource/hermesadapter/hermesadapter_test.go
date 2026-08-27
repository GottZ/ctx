// hermesadapter_test.go — the A02-2 gate. Every assertion here compares the
// adapter against the DIRECT hermesstate call on the SAME store, so a
// translation error shows up as a difference rather than as a plausible number.
//
// The fixture is built from internal/hermesstate/testdata/hermes_schema.sql —
// the verbatim upstream schema that package already pins. Read-only, over the
// module-relative path: a second copy of that DDL would drift away from the one
// the reader is actually proven against.
//
// Source: https://github.com/GottZ/ctx
package hermesadapter_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/distillsource/hermesadapter"
	"github.com/GottZ/ctx/internal/hermesstate"
	_ "modernc.org/sqlite"
)

// schemaPath is the pinned upstream schema of the package under adaptation.
const schemaPath = "../../hermesstate/testdata/hermes_schema.sql"

// baseTS is the fixture epoch.
var baseTS = time.Date(2026, 8, 24, 8, 58, 52, 0, time.UTC)

// multibyteBody is 60 runes over 180 bytes, all three-byte characters. The
// width is chosen so a byte cap at 10 both returns the wrong AMOUNT of text and
// lands INSIDE a character: a body of two-byte runes would hit a boundary at 10
// bytes and hide half the bug.
var multibyteBody = strings.Repeat("€", 60)

// row is one fixture message.
type row struct {
	id        int64
	session   string
	role      string
	content   string
	active    int
	compacted int
}

// fixtureRows is the whole store, written out rather than generated: every
// assertion below names an id, and a reader has to be able to see what that id
// is without running the builder in their head.
//
//	main   — the session under test. Archived tool rows carry every shape the
//	         decoder can produce: plain text, a folded multimodal payload, an
//	         unparseable payload (dropped), an image-only payload (decodes to
//	         nothing), and an oversized multibyte body.
//	older  — a second live session, for the Sessions ordering.
//	broken — every archived row unparseable: the window the adapter cannot name
//	         a watermark for.
//	silent — archived only: QuietFor has nothing to measure.
var fixtureRows = []row{
	{1, "main", "tool", "alpha tool output", 0, 1},
	{2, "main", "assistant", "prose, never read by this source", 0, 1},
	{3, "main", "tool", "bravo tool output", 0, 1},
	{4, "main", "tool", "\x00json:" + `[{"type":"text","text":"first part"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},{"type":"text","text":"second part"}]`, 0, 1},
	{5, "main", "tool", "\x00json:[{\"type\":\"text\",\"text\":\"unterminated", 0, 1},
	{6, "main", "tool", "\x00json:" + `[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]`, 0, 1},
	{7, "main", "tool", multibyteBody, 0, 1},
	{8, "main", "tool", "delta tool output", 0, 1},
	{9, "main", "tool", "live tool row, never archived", 1, 0},
	{10, "main", "assistant", "live prose", 1, 0},

	{11, "older", "tool", "older archived tool output", 0, 1},
	{12, "older", "assistant", "older live prose", 1, 0},

	{13, "broken", "tool", "\x00json:{ unterminated", 0, 1},
	{14, "broken", "tool", "\x00json:[ also unterminated", 0, 1},

	{15, "silent", "tool", "archived, nothing live left", 0, 1},
}

// liveNewest maps a session to the id of its newest active row, so the
// ordering assertion does not have to re-derive it.
var liveNewest = map[string]int64{"main": 10, "older": 12}

// sessionSkew moves a session's whole timeline. Without it the fixture's
// timestamps follow the id, and "older" — which necessarily carries HIGHER ids
// than "main", because ids are one global sequence — would come out as the
// newer session. That is the very confusion between id order and time order
// this store forces on every reader.
var sessionSkew = map[string]time.Duration{"older": -2 * time.Hour}

// rowTS is the wall clock of one fixture row.
func rowTS(r row) time.Time {
	return baseTS.Add(time.Duration(r.id)*time.Second + sessionSkew[r.session])
}

// buildStore writes the fixture and returns its path. It uses a write-capable
// handle; the package under test has no such path at all.
func buildStore(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", schemaPath, err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(wal)&_pragma=synchronous(off)")
	if err != nil {
		t.Fatalf("open writable: %v", err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	for _, s := range []string{"main", "older", "broken", "silent"} {
		if _, err := db.Exec(
			"INSERT INTO sessions(id, source, started_at) VALUES(?,?,?)",
			s, "cli", baseTS.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert session %s: %v", s, err)
		}
	}
	for _, r := range fixtureRows {
		ts := float64(rowTS(r).UnixNano()) / 1e9
		if _, err := db.Exec(`INSERT INTO messages
			(id, session_id, role, content, tool_name, timestamp, active, compacted)
			VALUES(?,?,?,?,?,?,?,?)`,
			r.id, r.session, r.role, r.content, "probe", ts, r.active, r.compacted); err != nil {
			t.Fatalf("insert message %d: %v", r.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}
	return path
}

// open returns the direct handle and the adapter over it. The handle is closed
// once, by the test — ownership is proven separately in TestCloseClosesTheHandle.
func open(t *testing.T) (*hermesstate.Source, distillsource.Source) {
	t.Helper()
	src, err := hermesstate.Open(buildStore(t), "hermes", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	a := hermesadapter.New(src)
	if a == nil {
		t.Fatal("New returned nil")
	}
	return src, a
}

// TestReadMatchesTheDirectCall is the gate: the adapter must surface exactly
// the rows the direct call surfaces, with exactly their text. The one
// admissible difference is stated and checked, not waved through — a row whose
// payload folds to nothing carries no prompt-ready text and produces no item,
// while still counting as covered.
func TestReadMatchesTheDirectCall(t *testing.T) {
	src, a := open(t)
	ctx := t.Context()

	direct, dropped, err := src.ToolRows(ctx, "main", 0, 50)
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1 (id 5 is unparseable)", dropped)
	}

	b, err := a.Read(ctx, "main", 0, 50, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var wantIDs []int64
	var emptyIDs []int64
	byID := map[int64]string{}
	for _, r := range direct {
		byID[r.ID] = r.Content
		if r.Content == "" {
			emptyIDs = append(emptyIDs, r.ID)
			continue
		}
		wantIDs = append(wantIDs, r.ID)
	}
	if len(emptyIDs) != 1 || emptyIDs[0] != 6 {
		t.Fatalf("rows folding to empty text = %v, want [6]", emptyIDs)
	}

	gotIDs := make([]int64, 0, len(b.Items))
	for _, it := range b.Items {
		gotIDs = append(gotIDs, it.Origin.RowID)
		if want := byID[it.Origin.RowID]; it.Text != want {
			t.Errorf("row %d text = %q, want the direct call's %q", it.Origin.RowID, it.Text, want)
		}
		if it.Truncated {
			t.Errorf("row %d marked truncated under a 10 000 rune cap", it.Origin.RowID)
		}
	}
	if !equalIDs(gotIDs, wantIDs) {
		t.Errorf("item rows = %v, want %v", gotIDs, wantIDs)
	}

	// The covered range includes the item-less row: the watermark comes from
	// the rows, not from the items.
	if b.Watermark != direct[len(direct)-1].ID {
		t.Errorf("watermark = %d, want the highest row read %d", b.Watermark, direct[len(direct)-1].ID)
	}
	if !b.Complete {
		t.Error("Complete = false; a hermes watermark group is a single row and cannot be cut")
	}
}

// TestItemShape checks the fields the arm consumes and that this wave decides
// rather than reads out of the store.
func TestItemShape(t *testing.T) {
	_, a := open(t)
	b, err := a.Read(t.Context(), "main", 0, 50, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) == 0 {
		t.Fatal("no items")
	}
	for _, it := range b.Items {
		if it.Text == "" {
			t.Error("empty item text reached the caller")
		}
		if !it.Untrusted {
			t.Errorf("row %d is not marked untrusted", it.Origin.RowID)
		}
		if it.Sensitivity != backends.SensCredentials {
			t.Errorf("row %d sensitivity = %q, want the fail-closed %q",
				it.Origin.RowID, it.Sensitivity, backends.SensCredentials)
		}
		if it.Origin.ChunkIndex != 1 {
			t.Errorf("row %d chunk index = %d, want 1", it.Origin.RowID, it.Origin.ChunkIndex)
		}
		if it.Origin.Role != "tool" {
			t.Errorf("row %d role = %q, want tool", it.Origin.RowID, it.Origin.Role)
		}
		if it.Origin.BlockID != "" {
			t.Errorf("row %d carries a block id %q", it.Origin.RowID, it.Origin.BlockID)
		}
		want := []string{"row=" + strconv.FormatInt(it.Origin.RowID, 10), "chunk=1"}
		got := make([]string, 0, len(it.Attrs))
		for _, at := range it.Attrs {
			got = append(got, at.Name+"="+at.Value)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("row %d attrs = %v, want %v", it.Origin.RowID, got, want)
		}
	}
}

// TestKeysetWalkCoversEveryRowOnce walks the session in windows of two and
// compares the union against the direct call. A watermark taken from the items
// instead of the rows re-reads the item-less row forever; this is the walk that
// would never terminate.
func TestKeysetWalkCoversEveryRowOnce(t *testing.T) {
	src, a := open(t)
	ctx := t.Context()

	direct, _, err := src.ToolRows(ctx, "main", 0, 50)
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	var want []int64
	for _, r := range direct {
		if r.Content != "" {
			want = append(want, r.ID)
		}
	}

	var (
		got   []int64
		after int64
	)
	for i := range 20 {
		b, err := a.Read(ctx, "main", after, 2, 10_000)
		if err != nil {
			t.Fatalf("Read at %d: %v", after, err)
		}
		if !b.Complete {
			t.Fatalf("window %d starting at %d is incomplete", i, after)
		}
		for _, it := range b.Items {
			got = append(got, it.Origin.RowID)
		}
		if b.Watermark == after {
			if len(b.Items) != 0 {
				t.Fatalf("window %d returned %d items without advancing", i, len(b.Items))
			}
			break
		}
		if b.Watermark < after {
			t.Fatalf("watermark went backwards: %d after %d", b.Watermark, after)
		}
		after = b.Watermark
	}
	if !equalIDs(got, want) {
		t.Errorf("walked rows = %v, want %v", got, want)
	}
	if after != want[len(want)-1] {
		t.Errorf("final watermark = %d, want %d", after, want[len(want)-1])
	}
}

// TestTruncationIsMarked is the "never truncate silently" property. The cap is
// over runes: the body is 120 runes over 240 bytes, so a byte cut would both
// return the wrong amount of text and split a character.
func TestTruncationIsMarked(t *testing.T) {
	_, a := open(t)
	b, err := a.Read(t.Context(), "main", 6, 1, 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(b.Items))
	}
	it := b.Items[0]
	if it.Origin.RowID != 7 {
		t.Fatalf("row = %d, want 7", it.Origin.RowID)
	}
	if !it.Truncated {
		t.Error("head-capped item is not marked truncated")
	}
	if n := utf8.RuneCountInString(it.Text); n != 10 {
		t.Errorf("item carries %d runes, want 10", n)
	}
	if !utf8.ValidString(it.Text) {
		t.Errorf("item text is not valid UTF-8: %q", it.Text)
	}
	if !strings.HasPrefix(multibyteBody, it.Text) {
		t.Errorf("item text %q is not a head of the row body", it.Text)
	}
	if b.Watermark != 7 || !b.Complete {
		t.Errorf("watermark = %d, complete = %v; want 7, true", b.Watermark, b.Complete)
	}
}

// TestUndecodableWindowDoesNotAdvance is the one case the adapter cannot name a
// covered watermark for. Standing still costs a repeat; guessing would skip the
// material for good.
func TestUndecodableWindowDoesNotAdvance(t *testing.T) {
	src, a := open(t)
	ctx := t.Context()

	direct, dropped, err := src.ToolRows(ctx, "broken", 0, 10)
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if len(direct) != 0 || dropped != 2 {
		t.Fatalf("direct call = %d rows / %d dropped, want 0 / 2", len(direct), dropped)
	}

	b, err := a.Read(ctx, "broken", 0, 10, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(b.Items))
	}
	if b.Watermark != 0 {
		t.Errorf("watermark = %d, want 0 (unchanged)", b.Watermark)
	}
	if b.Complete {
		t.Error("Complete = true although no row could be named")
	}
}

// TestEmptyWindowIsComplete separates "nothing new" from "nothing nameable":
// both stand still, only one of them blocks the caller.
func TestEmptyWindowIsComplete(t *testing.T) {
	_, a := open(t)
	b, err := a.Read(t.Context(), "main", 8, 10, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) != 0 || b.Watermark != 8 || !b.Complete {
		t.Errorf("batch = %d items / watermark %d / complete %v, want 0 / 8 / true",
			len(b.Items), b.Watermark, b.Complete)
	}
}

// TestNonPositiveCapsReadNothing proves the source does not substitute a cap of
// its own: with no admissible budget nothing is read, and nothing is covered.
func TestNonPositiveCapsReadNothing(t *testing.T) {
	_, a := open(t)
	for _, tc := range []struct{ items, runes int }{{0, 10}, {10, 0}, {-1, -1}} {
		b, err := a.Read(t.Context(), "main", 3, tc.items, tc.runes)
		if err != nil {
			t.Fatalf("Read(%d,%d): %v", tc.items, tc.runes, err)
		}
		if len(b.Items) != 0 || b.Watermark != 3 || b.Complete {
			t.Errorf("Read(%d,%d) = %d items / watermark %d / complete %v, want 0 / 3 / false",
				tc.items, tc.runes, len(b.Items), b.Watermark, b.Complete)
		}
	}
}

// TestProbesMatchTheDirectCalls covers the three cheap methods against their
// hermesstate counterparts.
func TestProbesMatchTheDirectCalls(t *testing.T) {
	src, a := open(t)
	ctx := t.Context()
	now := baseTS.Add(time.Hour)

	if got := a.Label(); got != src.Label() {
		t.Errorf("Label = %q, want %q", got, src.Label())
	}
	for _, after := range []int64{0, 4, 8, 99} {
		want, err := src.HasNewArchived(ctx, "main", after)
		if err != nil {
			t.Fatalf("HasNewArchived(%d): %v", after, err)
		}
		got, err := a.HasNew(ctx, "main", after)
		if err != nil {
			t.Fatalf("HasNew(%d): %v", after, err)
		}
		if got != want {
			t.Errorf("HasNew(%d) = %v, want %v", after, got, want)
		}
	}
	wantHead, err := src.MaxCompactedID(ctx, "main")
	if err != nil {
		t.Fatalf("MaxCompactedID: %v", err)
	}
	gotHead, err := a.Head(ctx, "main")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if gotHead != wantHead || gotHead != 8 {
		t.Errorf("Head = %d, want %d (direct) and 8 (fixture)", gotHead, wantHead)
	}
	wantQuiet, err := src.QuietFor(ctx, "main", now)
	if err != nil {
		t.Fatalf("QuietFor direct: %v", err)
	}
	gotQuiet, err := a.QuietFor(ctx, "main", now)
	if err != nil {
		t.Fatalf("QuietFor: %v", err)
	}
	if gotQuiet != wantQuiet {
		t.Errorf("QuietFor = %v, want %v", gotQuiet, wantQuiet)
	}
	if want := now.Sub(rowTS(row{id: liveNewest["main"], session: "main"})); gotQuiet != want {
		t.Errorf("QuietFor = %v, want %v against the fixture's newest live row", gotQuiet, want)
	}
}

// TestErrorClassesAreLifted proves the adapter hands back the ABSTRACTION's
// classes. A caller that had to match hermesstate's sentinels would carry the
// SQLite package in its own import graph, which is the coupling this adapter
// exists to remove.
func TestErrorClassesAreLifted(t *testing.T) {
	_, a := open(t)
	_, err := a.QuietFor(t.Context(), "silent", baseTS.Add(time.Hour))
	if err == nil {
		t.Fatal("QuietFor on a fully archived session returned no error")
	}
	if !errors.Is(err, distillsource.ErrNoActiveRows) {
		t.Errorf("error %v is not distillsource.ErrNoActiveRows", err)
	}
	for _, other := range []error{
		distillsource.ErrQueryFailed,
		distillsource.ErrSourceUnavailable,
		distillsource.ErrSchemaUntrusted,
	} {
		if errors.Is(err, other) {
			t.Errorf("error %v also matches %v", err, other)
		}
	}
}

// TestCloseClosesTheHandle proves the ownership claim in New: after Close the
// wrapped source is unusable, and the failure arrives as a lifted class.
func TestCloseClosesTheHandle(t *testing.T) {
	src, err := hermesstate.Open(buildStore(t), "hermes", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	a := hermesadapter.New(src)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := src.MaxCompactedID(t.Context(), "main"); err == nil {
		t.Fatal("the wrapped handle still answers after Close")
	}
	if _, err := a.Head(t.Context(), "main"); !errors.Is(err, distillsource.ErrQueryFailed) {
		t.Errorf("Head after Close = %v, want a lifted query_failed", err)
	}
}

// TestSessionsNewestFirst checks the ordering contract and that a session
// without live rows is listed rather than hidden.
func TestSessionsNewestFirst(t *testing.T) {
	src, a := open(t)
	ctx := t.Context()

	direct, err := src.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions direct: %v", err)
	}
	refs, err := a.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(refs) != len(direct) {
		t.Fatalf("len(refs) = %d, want %d", len(refs), len(direct))
	}
	if refs[0].Session != "main" || refs[1].Session != "older" {
		t.Errorf("head of the list = %q, %q; want main, older", refs[0].Session, refs[1].Session)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if r.Watermark != 0 {
			t.Errorf("session %q carries watermark %d; Sessions does not pay for a head",
				r.Session, r.Watermark)
		}
		seen[r.Session] = true
	}
	for _, want := range []string{"main", "older", "broken", "silent"} {
		if !seen[want] {
			t.Errorf("session %q is missing from the candidate list", want)
		}
	}
}

// TestReadIgnoresLiveAndNonToolRows restates the source's own filter through
// the adapter: nothing that is not an archived tool result may reach an item.
func TestReadIgnoresLiveAndNonToolRows(t *testing.T) {
	_, a := open(t)
	b, err := a.Read(t.Context(), "main", 0, 50, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, it := range b.Items {
		if it.Origin.RowID >= 9 {
			t.Errorf("row %d is live and must not be read", it.Origin.RowID)
		}
		if strings.Contains(it.Text, "prose") {
			t.Errorf("row %d carries non-tool prose: %q", it.Origin.RowID, it.Text)
		}
	}
}

// equalIDs compares two id slices elementwise, nil and empty alike.
func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
