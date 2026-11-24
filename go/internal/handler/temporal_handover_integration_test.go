//go:build integration

// Issue #434: a content write computes its block's content_times in its own
// statement (issue #421) and hands those dates to its temporal afterwork,
// which writes the dimension rows from them instead of extracting the same
// content a second time. The tests go through every door that writes content
// and runs the afterwork (POST /api/store with and without a supersedes,
// POST /api/ingest in block and chunk mode, POST /api/store/batch, manage
// update with a content and with edits, the MCP store, update and store_batch
// tools, and the confirmed store, update and batch of a confirm_writes key),
// and count every extraction of the written content through the extraction
// every temporal writer uses (store.SetTemporalExtractionForTest): the exact
// measure, independent of the clock and of other tests' writes, because it
// counts by content. An extraction is the afterwork's when the stack holds the
// afterwork (store.Refresh…), the write's otherwise.
//
// Besides the count, each door's rows are compared with what the extraction
// of the content the block holds yields (the behaviour before the handover),
// and the "i434-rows" lines of a -v run print them normalized, so a run on the
// base and one on the head can be diffed line by line.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestTemporalHandover' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/store"
)

// i434Extractions counts the extractions of each content, split into the
// write's and the afterwork's.
type i434Extractions struct {
	mu        sync.Mutex
	write     map[string]int
	afterwork map[string]int
}

// i434Count stands a counting extraction in for the one every temporal writer
// uses, for the rest of the test.
func i434Count(t *testing.T) *i434Extractions {
	t.Helper()
	c := &i434Extractions{write: map[string]int{}, afterwork: map[string]int{}}
	restore := store.SetTemporalExtractionForTest(func(content string) []time.Time {
		c.note(content)
		return store.ExtractDates(content)
	})
	t.Cleanup(restore)
	return c
}

// i434AfterworkPrefix is the prefix of the store's afterwork functions
// (RefreshTemporal and what calls it): an extraction with one of them on its
// stack is the afterwork's.
const i434AfterworkPrefix = "github.com/GottZ/ctx/internal/store.Refresh"

func (c *i434Extractions) note(content string) {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	afterwork := false
	for {
		fr, more := frames.Next()
		if strings.HasPrefix(fr.Function, i434AfterworkPrefix) {
			afterwork = true
			break
		}
		if !more {
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if afterwork {
		c.afterwork[content]++
	} else {
		c.write[content]++
	}
}

// of returns the extractions of content so far.
func (c *i434Extractions) of(content string) (write, afterwork int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.write[content], c.afterwork[content]
}

// reset forgets the extractions of content (a staged write's checks), after
// it logged them for the inventory of the -v run.
func (c *i434Extractions) reset(t *testing.T, content string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Logf("i434-stage %q write=%d afterwork=%d", content[:min(len(content), 30)], c.write[content], c.afterwork[content])
	delete(c.write, content)
	delete(c.afterwork, content)
}

// i434Written is one block a door wrote and the content it wrote.
type i434Written struct {
	title, content string
}

// i434Content is a content with a date of its own (2031, month and day from
// n) and two more forms the extraction reads.
func i434Content(label string, n int) string {
	return fmt.Sprintf("i434 %s: the review is on 2031-%02d-%02d, the follow-up on 14.11.2032 and the retrospective in März 2033.",
		label, 1+n%12, 1+n%28)
}

// i434ID reads the id of the live block titled title in category i434.
func i434ID(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_blocks WHERE category = 'i434' AND title = $1 AND NOT is_archived`, title).Scan(&id); err != nil {
		t.Fatalf("id of %q: %v", title, err)
	}
	return id
}

// i434AwaitAfterwork waits until the block's rows hold every date of content:
// the afterwork of the write of content has committed (its extraction, if it
// makes one, came before its rows). It does not order anything; it bounds the
// wait for an afterwork that runs on a goroutine of its own.
func i434AwaitAfterwork(t *testing.T, pool *pgxpool.Pool, id, content string) {
	t.Helper()
	dates := store.ExtractDates(content)
	deadline := time.Now().Add(i414LockWait)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(DISTINCT source_time) FROM context_temporal WHERE block_id = $1 AND source_time = ANY($2::timestamptz[])`,
			id, dates).Scan(&n); err != nil {
			t.Fatalf("read rows of %s: %v", id, err)
		}
		if n == len(dates) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the afterwork of %s did not write the dates of %q within %s (%d of %d)", id, content, i414LockWait, n, len(dates))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// i434State is a block's temporal state, normalized for a comparison across
// runs: the content_times column, whether a review version row stands, and
// every dimension row as dimension=value@source, the creation time written as
// "created".
type i434State struct {
	contentTimes string
	reviewed     bool
	rows         string
}

func (s i434State) String() string {
	return fmt.Sprintf("content_times=%s reviewed=%v rows=%s", s.contentTimes, s.reviewed, s.rows)
}

// i434Rows reads the block's i434State.
func i434Rows(t *testing.T, pool *pgxpool.Pool, id string) i434State {
	t.Helper()
	ctx := context.Background()
	var created time.Time
	var times []time.Time
	var reviewed bool
	if err := pool.QueryRow(ctx,
		`SELECT created_at, COALESCE(content_times, '{}'),
		        EXISTS (SELECT 1 FROM context_temporal_reviews r WHERE r.block_id = b.id)
		   FROM context_blocks b WHERE id = $1`, id).Scan(&created, &times, &reviewed); err != nil {
		t.Fatalf("read block %s: %v", id, err)
	}
	src := func(s time.Time) string {
		if s.Equal(created) {
			return "created"
		}
		return s.UTC().Format(time.RFC3339)
	}
	rows, err := pool.Query(ctx, `SELECT dimension, value, source_time FROM context_temporal WHERE block_id = $1`, id)
	if err != nil {
		t.Fatalf("read rows of %s: %v", id, err)
	}
	var dims []string
	for rows.Next() {
		var d, v string
		var s time.Time
		if err := rows.Scan(&d, &v, &s); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if d == "_none" {
			dims = append(dims, "_none")
			continue
		}
		dims = append(dims, d+"="+v+"@"+src(s))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows of %s: %v", id, err)
	}
	slices.Sort(dims)
	ct := make([]string, len(times))
	for i, x := range times {
		ct[i] = src(x)
	}
	return i434State{contentTimes: fmt.Sprint(ct), reviewed: reviewed, rows: fmt.Sprint(dims)}
}

// i434Expected is the i434State of a block whose last content write stored
// timesOf and whose rows are the extraction of rowsOf (and its creation time),
// as the afterwork wrote them before the handover: content_times the dates of
// timesOf in their order, the rows of the creation time and the dates of
// rowsOf, extra the dates a review added (and then its version row).
func i434Expected(timesOf, rowsOf string, created time.Time, extra ...time.Time) i434State {
	dates := store.ExtractDates(rowsOf)
	src := func(s time.Time) string {
		if s.Equal(created) {
			return "created"
		}
		return s.UTC().Format(time.RFC3339)
	}
	var dims []string
	seen := map[string]bool{}
	for _, at := range append(append([]time.Time{created}, dates...), extra...) {
		for _, d := range store.ExpandDimensions(at) {
			k := d.Dimension + "=" + d.Value + "@" + src(at)
			if !seen[k] {
				seen[k] = true
				dims = append(dims, k)
			}
		}
	}
	slices.Sort(dims)
	stored := store.ExtractDates(timesOf)
	ct := make([]string, len(stored))
	for i, x := range stored {
		ct[i] = src(x)
	}
	return i434State{contentTimes: fmt.Sprint(ct), reviewed: len(extra) > 0, rows: fmt.Sprint(dims)}
}

// i434AssertRows fails unless the block's temporal state is what the
// extraction of content yields, and prints it for the base/head diff.
func i434AssertRows(t *testing.T, pool *pgxpool.Pool, label, id, content string, extra ...time.Time) {
	t.Helper()
	i434AssertState(t, pool, label, id, content, content, extra...)
}

// i434AssertState is i434AssertRows for a block whose content_times stem from
// timesOf (its last content write) and whose rows from rowsOf.
func i434AssertState(t *testing.T, pool *pgxpool.Pool, label, id, timesOf, rowsOf string, extra ...time.Time) {
	t.Helper()
	got := i434Rows(t, pool, id)
	t.Logf("i434-rows %s %s", label, got)
	var created time.Time
	if err := pool.QueryRow(context.Background(), `SELECT created_at FROM context_blocks WHERE id = $1`, id).Scan(&created); err != nil {
		t.Fatalf("created_at of %s: %v", id, err)
	}
	if want := i434Expected(timesOf, rowsOf, created, extra...); got != want {
		t.Errorf("%s: temporal state\n got %s\nwant %s", label, got, want)
	}
}

// i434Post serves one JSON request to a REST handler as the fixture's direct
// key and decodes the answer.
func i434Post(t *testing.T, ctx context.Context, h http.HandlerFunc, path string, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw))).WithContext(ctx))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d %s (%v)", path, rec.Code, rec.Body.String(), err)
	}
	return out
}

// i434Staged takes the answer of a staged write (a tool result that says the
// write waits for its confirm, an error result by design).
func i434Staged(t *testing.T) func(*mcp.CallToolResult, any, error) string {
	return func(r *mcp.CallToolResult, _ any, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		return resultText(t, r)
	}
}

// i434Confirm confirms the write a confirm_writes key staged.
func i434Confirm(t *testing.T, f *i406Fixture, staged string) {
	t.Helper()
	m := hashRe.FindStringSubmatch(staged)
	if m == nil {
		t.Fatalf("the write was not staged: %s", staged)
	}
	r, _, err := mcpConfirmHandler(f.mcpCfg)(f.flaggedCtx, nil, confirmInput{PayloadHash: m[1]})
	if err != nil || r.IsError {
		t.Fatalf("confirm: %v %s", err, resultText(t, r))
	}
}

// i434Tool takes a tool call's answer and fails on an error.
func i434Tool(t *testing.T) func(*mcp.CallToolResult, any, error) string {
	return func(r *mcp.CallToolResult, _ any, err error) string {
		t.Helper()
		if err != nil || r == nil || r.IsError {
			t.Fatalf("tool: %v %v", err, r)
		}
		return resultText(t, r)
	}
}

// i434Door is one door that writes content and runs the afterwork: write
// writes and returns what it wrote; the counter c is reset by doors whose
// write is staged first, after the stage.
type i434Door struct {
	name  string
	write func(t *testing.T, f *i406Fixture, c *i434Extractions, n int) []i434Written
}

func i434Doors() []i434Door {
	rest := func(f *i406Fixture) *StoreHandler { return NewStoreHandler(f.pool, f.mcpCfg.Cfg, f.mcpCfg.Blocktypes) }
	storeBody := func(title, content string) map[string]any {
		return map[string]any{"category": "i434", "title": title, "content": content}
	}
	// seeded writes a block through the MCP store tool (its afterwork is
	// inline) for a door that updates it.
	seeded := func(t *testing.T, f *i406Fixture, title string) string {
		t.Helper()
		i434Tool(t)(mcpStoreHandler(f.mcpCfg)(f.directCtx, nil, storeInput{Category: "i434", Title: title, Content: "i434 seed of " + title}))
		return i434ID(t, f.pool, title)
	}
	return []i434Door{
		{"rest_store_new", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			w := i434Written{"rest store new", i434Content("rest store new", n)}
			i434Post(t, f.directCtx, rest(f).HandleStore, "/api/store", storeBody(w.title, w.content))
			return []i434Written{w}
		}},
		{"rest_store_changed", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			title := "rest store changed"
			seeded(t, f, title)
			w := i434Written{title, i434Content(title, n)}
			i434Post(t, f.directCtx, rest(f).HandleStore, "/api/store", storeBody(w.title, w.content))
			return []i434Written{w}
		}},
		{"rest_store_supersedes", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			target := seeded(t, f, "rest store superseded")
			w := i434Written{"rest store superseding", i434Content("rest store superseding", n)}
			body := storeBody(w.title, w.content)
			body["supersedes"] = target
			i434Post(t, f.directCtx, rest(f).HandleStore, "/api/store", body)
			return []i434Written{w}
		}},
		{"rest_ingest_block", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			w := i434Written{"rest ingest block", i434Content("rest ingest block", n)}
			h := NewIngestHandler(f.pool, f.mcpCfg.Cfg, f.mcpCfg.Blocktypes)
			i434Post(t, f.directCtx, h.HandleIngest, "/api/ingest", map[string]any{"source": "i434",
				"chunks": []map[string]any{storeBody(w.title, w.content)}})
			return []i434Written{w}
		}},
		{"rest_ingest_chunk", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			w := i434Written{"rest ingest chunk", i434Content("rest ingest chunk", n)}
			chunk := storeBody(w.title, w.content)
			chunk["source_id"], chunk["chunk_index"] = seedSource(t, f.pool, "/i434/chunk.md", "private"), 0
			h := NewIngestHandler(f.pool, f.mcpCfg.Cfg, f.mcpCfg.Blocktypes)
			i434Post(t, f.directCtx, h.HandleIngest, "/api/ingest", map[string]any{"source": "i434",
				"chunks": []map[string]any{chunk}})
			return []i434Written{w}
		}},
		{"rest_store_batch", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			a := i434Written{"rest batch a", i434Content("rest batch a", n)}
			b := i434Written{"rest batch b", i434Content("rest batch b", n+40)}
			i434Post(t, f.directCtx, rest(f).HandleStoreBatch, "/api/store/batch", map[string]any{
				"items": []map[string]any{storeBody(a.title, a.content), storeBody(b.title, b.content)}})
			return []i434Written{a, b}
		}},
		{"rest_manage_update_content", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			title := "rest manage update content"
			id := seeded(t, f, title)
			w := i434Written{title, i434Content(title, n)}
			raw, _ := json.Marshal(map[string]any{"content": w.content})
			if status, body := f.restUpdate(t, id, string(raw)); status != http.StatusOK || body["success"] != true {
				t.Fatalf("manage update: %d %v", status, body)
			}
			return []i434Written{w}
		}},
		{"rest_manage_update_edits", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			title := "rest manage update edits"
			id := seeded(t, f, title)
			w := i434Written{title, "i434 seed of " + title + " — " + i434Content("edited", n)}
			raw, _ := json.Marshal(map[string]any{"edits": []map[string]any{{"old": title, "new": title + " — " + i434Content("edited", n)}}})
			if status, body := f.restUpdate(t, id, string(raw)); status != http.StatusOK || body["success"] != true {
				t.Fatalf("manage update edits: %d %v", status, body)
			}
			return []i434Written{w}
		}},
		{"mcp_store", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			w := i434Written{"mcp store", i434Content("mcp store", n)}
			i434Tool(t)(mcpStoreHandler(f.mcpCfg)(f.directCtx, nil, storeInput{Category: "i434", Title: w.title, Content: w.content}))
			return []i434Written{w}
		}},
		{"mcp_update", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			title := "mcp update"
			id := seeded(t, f, title)
			w := i434Written{title, i434Content(title, n)}
			i434Tool(t)(mcpUpdateHandler(f.mcpCfg)(f.directCtx, nil, updateInput{ID: id, Content: &w.content}))
			return []i434Written{w}
		}},
		{"mcp_store_batch", func(t *testing.T, f *i406Fixture, _ *i434Extractions, n int) []i434Written {
			a := i434Written{"mcp batch a", i434Content("mcp batch a", n)}
			b := i434Written{"mcp batch b", i434Content("mcp batch b", n+40)}
			i434Tool(t)(mcpStoreBatchHandler(f.mcpCfg)(f.directCtx, nil, storeBatchInput{Items: []storeInput{
				{Category: "i434", Title: a.title, Content: a.content}, {Category: "i434", Title: b.title, Content: b.content}}}))
			return []i434Written{a, b}
		}},
		{"confirmed_store", func(t *testing.T, f *i406Fixture, c *i434Extractions, n int) []i434Written {
			w := i434Written{"confirmed store", i434Content("confirmed store", n)}
			staged := i434Staged(t)(mcpStoreHandler(f.mcpCfg)(f.flaggedCtx, nil, storeInput{Category: "i434", Title: w.title, Content: w.content}))
			c.reset(t, w.content)
			i434Confirm(t, f, staged)
			return []i434Written{w}
		}},
		{"confirmed_update", func(t *testing.T, f *i406Fixture, c *i434Extractions, n int) []i434Written {
			title := "confirmed update"
			id := seeded(t, f, title)
			w := i434Written{title, i434Content(title, n)}
			staged := i434Staged(t)(mcpUpdateHandler(f.mcpCfg)(f.flaggedCtx, nil, updateInput{ID: id, Content: &w.content}))
			c.reset(t, w.content)
			i434Confirm(t, f, staged)
			return []i434Written{w}
		}},
		{"confirmed_store_batch", func(t *testing.T, f *i406Fixture, c *i434Extractions, n int) []i434Written {
			a := i434Written{"confirmed batch a", i434Content("confirmed batch a", n)}
			b := i434Written{"confirmed batch b", i434Content("confirmed batch b", n+40)}
			staged := i434Staged(t)(mcpStoreBatchHandler(f.mcpCfg)(f.flaggedCtx, nil, storeBatchInput{Items: []storeInput{
				{Category: "i434", Title: a.title, Content: a.content}, {Category: "i434", Title: b.title, Content: b.content}}}))
			c.reset(t, a.content)
			c.reset(t, b.content)
			i434Confirm(t, f, staged)
			return []i434Written{a, b}
		}},
	}
}

// TestTemporalHandoverOneExtractionPerWrite: through every door, a write of
// a content extracts its dates once — in the write, for content_times — and
// its afterwork writes the rows from those dates without extracting again.
// The rows are the ones the extraction of the content yields. Before the
// handover the afterwork extracted the content a second time: write=1,
// afterwork=1 on every door.
func TestTemporalHandoverOneExtractionPerWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	c := i434Count(t)
	for n, door := range i434Doors() {
		t.Run(door.name, func(t *testing.T) {
			for _, w := range door.write(t, f, c, n) {
				id := i434ID(t, f.pool, w.title)
				i434AwaitAfterwork(t, f.pool, id, w.content)
				write, afterwork := c.of(w.content)
				t.Logf("i434-count %s %q write=%d afterwork=%d", door.name, w.title, write, afterwork)
				if write != 1 || afterwork != 0 {
					t.Errorf("%s %q: extractions write=%d afterwork=%d, want write=1 afterwork=0", door.name, w.title, write, afterwork)
				}
				i434AssertRows(t, f.pool, door.name, id, w.content)
			}
		})
	}
}

// i434Upsert writes through the store's upsert, the write of the store doors.
func i434Upsert(t *testing.T, pool *pgxpool.Pool, title, content string, tags []string, metadata map[string]any) *store.Block {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "i434", title, content, tags, metadata, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("upsert %q: %v", title, err)
	}
	return b
}

// TestTemporalHandoverRowsAsTheExtraction runs the write and then the
// afterwork the doors run after it (finishStoredBlock after the store's
// upsert and the batch, finishBlockUpdate after an update) over blocks with
// dates in the title or the content, without any, with a changed and an
// unchanged content, with a change of the metadata only, of 50 KB, and with a
// review the same-content write must leave standing: the afterwork's rows are
// in each case those of the extraction of the content the block holds.
func TestTemporalHandoverRowsAsTheExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	ctx := f.ctx
	set := f.mcpCfg.Blocktypes.SnapshotForRequest(ctx)
	stored := func(t *testing.T, label string, b *store.Block, content string, extra ...time.Time) {
		t.Helper()
		finishStoredBlock(ctx, f.pool, set, b, "i434-"+label)
		i434AssertRows(t, f.pool, label, b.ID, content, extra...)
	}
	updated := func(t *testing.T, label, id string, data store.UpdateBlockData, content string) {
		t.Helper()
		b := i414Update(t, f, id, data)
		finishBlockUpdate(ctx, f.pool, f.mcpCfg.Blocktypes, data, b, "i434-"+label)
		i434AssertRows(t, f.pool, label, id, content)
	}
	const dated = "Kickoff 2031-02-03 09:30, Abnahme am 17.03.2031, Release im April 2031; the audit is due in September 2032."

	t.Run("dates in the content", func(t *testing.T) {
		stored(t, "dated", i434Upsert(t, f.pool, "dated", dated, nil, nil), dated)
	})
	t.Run("a date in the title only", func(t *testing.T) {
		const content = "No date in this content, only in the title."
		stored(t, "title-dated", i434Upsert(t, f.pool, "Planning 2031-05-06", content, nil, nil), content)
	})
	t.Run("no dates", func(t *testing.T) {
		const content = "Neither title nor content name a day."
		stored(t, "undated", i434Upsert(t, f.pool, "undated", content, nil, nil), content)
	})
	t.Run("changed content, upsert", func(t *testing.T) {
		stored(t, "changed-upsert-1", i434Upsert(t, f.pool, "changed upsert", dated, nil, nil), dated)
		const next = "Moved: the kickoff is now on 2031-06-07."
		stored(t, "changed-upsert-2", i434Upsert(t, f.pool, "changed upsert", next, nil, nil), next)
	})
	t.Run("changed content, update and edits", func(t *testing.T) {
		b := i434Upsert(t, f.pool, "changed update", dated, nil, nil)
		stored(t, "changed-update-0", b, dated)
		next := "Replaced on 2031-08-09."
		updated(t, "changed-update-1", b.ID, store.UpdateBlockData{Content: &next}, next)
		repl := "2031-10-11"
		updated(t, "changed-update-2", b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "2031-08-09", New: &repl}}}, "Replaced on 2031-10-11.")
	})
	t.Run("unchanged content, other tags and metadata", func(t *testing.T) {
		stored(t, "unchanged-1", i434Upsert(t, f.pool, "unchanged", dated, []string{"a"}, nil), dated)
		stored(t, "unchanged-2", i434Upsert(t, f.pool, "unchanged", dated, []string{"b"}, map[string]any{"k": "v"}), dated)
	})
	t.Run("metadata only, update", func(t *testing.T) {
		b := i434Upsert(t, f.pool, "metadata only", dated, nil, nil)
		stored(t, "metadata-only-0", b, dated)
		updated(t, "metadata-only-1", b.ID, store.UpdateBlockData{Metadata: map[string]any{"k": "v"}}, dated)
		updated(t, "metadata-only-2", b.ID, store.UpdateBlockData{MetadataMerge: map[string]any{"j": "w"}}, dated)
	})
	t.Run("50 KB", func(t *testing.T) {
		var sb strings.Builder
		for i := 0; sb.Len() < 50_000; i++ {
			fmt.Fprintf(&sb, "Paragraph %d of the long note, written on %02d.%02d.2031 about the plan for März 2032. ", i, 1+i%28, 1+i%12)
		}
		content := sb.String()
		stored(t, "50kb", i434Upsert(t, f.pool, "fifty kilobytes", content, nil, nil), content)
	})
	t.Run("a review the same-content write leaves standing", func(t *testing.T) {
		b := i434Upsert(t, f.pool, "reviewed", dated, nil, nil)
		stored(t, "reviewed-0", b, dated)
		found := time.Date(2031, 12, 24, 0, 0, 0, 0, time.UTC)
		review := append(store.ExtractDates(dated), found)
		if applied, err := store.PopulateTemporalReview(ctx, f.pool, b.ID, b.Title, b.Content, review, b.CreatedAt); err != nil || !applied {
			t.Fatalf("review: applied=%v err=%v", applied, err)
		}
		stored(t, "reviewed-1", i434Upsert(t, f.pool, "reviewed", dated, []string{"again"}, nil), dated, found)
	})
	t.Run("a batch", func(t *testing.T) {
		const other = "Batch item two, on 2031-11-12."
		res, err := store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
			{Category: "i434", Title: "batch one", Content: dated, Scope: "private"},
			{Category: "i434", Title: "batch two", Content: other, Scope: "private"},
		}, f.directAR.ApiKeyID, set, nil)
		if err != nil || len(res) != 2 {
			t.Fatalf("batch: %v %v", res, err)
		}
		stored(t, "batch-1", res[0].Block, dated)
		stored(t, "batch-2", res[1].Block, other)
	})
}

// TestTemporalHandoverLateAfterworkOfAnEarlierContent: two writes A, B of one
// block, B's afterwork before A's; A's afterwork carries the dates of A's
// content and must write nothing over the rows of B, the content the block
// holds. Sequenced by the order of the calls, on the store's upsert (the
// store doors) and on an update.
func TestTemporalHandoverLateAfterworkOfAnEarlierContent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	ctx := f.ctx
	set := f.mcpCfg.Blocktypes.SnapshotForRequest(ctx)
	const a, b = "Content A, due on 2031-03-04.", "Content B, due on 2032-05-06."

	t.Run("upsert, A's afterwork last", func(t *testing.T) {
		blockA := i434Upsert(t, f.pool, "late upsert", a, nil, nil)
		blockB := i434Upsert(t, f.pool, "late upsert", b, nil, nil)
		finishStoredBlock(ctx, f.pool, set, blockB, "i434-b")
		finishStoredBlock(ctx, f.pool, set, blockA, "i434-a")
		i434AssertRows(t, f.pool, "late-upsert", blockB.ID, b)
	})
	t.Run("upsert, A's afterwork between B's write and B's afterwork", func(t *testing.T) {
		initial := "Initial, 2030-01-02."
		first := i434Upsert(t, f.pool, "between upsert", initial, nil, nil)
		finishStoredBlock(ctx, f.pool, set, first, "i434-0")
		blockA := i434Upsert(t, f.pool, "between upsert", a, nil, nil)
		blockB := i434Upsert(t, f.pool, "between upsert", b, nil, nil)
		finishStoredBlock(ctx, f.pool, set, blockA, "i434-a")
		i434AssertState(t, f.pool, "between-upsert-after-a", blockB.ID, b, initial)
		finishStoredBlock(ctx, f.pool, set, blockB, "i434-b")
		i434AssertRows(t, f.pool, "between-upsert-after-b", blockB.ID, b)
	})
	t.Run("update, A's afterwork last", func(t *testing.T) {
		id := i434Upsert(t, f.pool, "late update", "Seed.", nil, nil).ID
		dataA, dataB := i414Content(a), i414Content(b)
		blockA := i414Update(t, f, id, dataA)
		blockB := i414Update(t, f, id, dataB)
		finishBlockUpdate(ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i434-b")
		finishBlockUpdate(ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i434-a")
		i434AssertRows(t, f.pool, "late-update", id, b)
	})
	t.Run("A, B, A again: A's late afterwork finds its content back", func(t *testing.T) {
		blockA := i434Upsert(t, f.pool, "aba", a, nil, nil)
		blockB := i434Upsert(t, f.pool, "aba", b, nil, nil)
		blockA2 := i434Upsert(t, f.pool, "aba", a, nil, nil)
		finishStoredBlock(ctx, f.pool, set, blockB, "i434-b")
		finishStoredBlock(ctx, f.pool, set, blockA, "i434-a")
		i434AssertRows(t, f.pool, "aba-after-a", blockA2.ID, a)
		finishStoredBlock(ctx, f.pool, set, blockA2, "i434-a2")
		i434AssertRows(t, f.pool, "aba-after-a2", blockA2.ID, a)
	})
}

// i434StartGate holds the first transaction on its pool that sends the
// temporal writers' turn (their first statement) before it sends it, until
// release; done closes when that transaction has ended (commit or rollback).
// Like testdb.CommitHold it is a pgx tracer, so the statements are the
// production ones; it gates the start instead of the COMMIT.
type i434StartGate struct {
	mu      sync.Mutex
	conn    *pgx.Conn
	held    chan struct{}
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

func i434GatedPool(t *testing.T, dsn string) (*pgxpool.Pool, *i434StartGate) {
	t.Helper()
	g := &i434StartGate{held: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = g
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.once.Do(func() { close(g.release) })
		pool.Close()
	})
	return pool, g
}

func (g *i434StartGate) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, q := range data.Batch.QueuedQueries {
		if strings.Contains(q.SQL, "hashtext('ctx:temporal')") {
			g.mu.Lock()
			first := g.conn == nil
			if first {
				g.conn = conn
			}
			g.mu.Unlock()
			if first {
				close(g.held)
				select {
				case <-g.release:
				case <-time.After(i414LockWait * 3):
				}
			}
			break
		}
	}
	return ctx
}

func (g *i434StartGate) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (g *i434StartGate) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func (g *i434StartGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (g *i434StartGate) TraceQueryEnd(_ context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	g.mu.Lock()
	gated := g.conn == conn
	g.mu.Unlock()
	if !gated {
		return
	}
	switch strings.ToLower(strings.TrimSpace(data.CommandTag.String())) {
	case "commit", "rollback":
		select {
		case <-g.done:
		default:
			close(g.done)
		}
	}
}

// TestTemporalHandoverLateAfterworkAtTheStoreDoor is the same interleaving at
// POST /api/store, whose afterwork runs on a goroutine of its own: A's
// afterwork is held before its first statement (a gate on the pool of A's
// handler), B is posted through a second handler and its afterwork commits
// B's rows, then A's afterwork goes on. It writes nothing; the block keeps
// B's rows.
func TestTemporalHandoverLateAfterworkAtTheStoreDoor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	gated, gate := i434GatedPool(t, f.pool.Config().ConnString())
	const a, b = "Door content A, due on 2031-03-04.", "Door content B, due on 2032-05-06."
	body := func(content string) map[string]any {
		return map[string]any{"category": "i434", "title": "door", "content": content}
	}

	i434Post(t, f.directCtx, NewStoreHandler(gated, f.mcpCfg.Cfg, f.mcpCfg.Blocktypes).HandleStore, "/api/store", body(a))
	select {
	case <-gate.held:
	case <-time.After(i414LockWait):
		t.Fatalf("A's afterwork did not reach its first statement within %s", i414LockWait)
	}
	i434Post(t, f.directCtx, NewStoreHandler(f.pool, f.mcpCfg.Cfg, f.mcpCfg.Blocktypes).HandleStore, "/api/store", body(b))
	id := i434ID(t, f.pool, "door")
	i434AwaitAfterwork(t, f.pool, id, b)
	i434AssertRows(t, f.pool, "door-after-b", id, b)

	gate.once.Do(func() { close(gate.release) })
	select {
	case <-gate.done:
	case <-time.After(i414LockWait):
		t.Fatalf("A's afterwork did not end within %s of its release", i414LockWait)
	}
	i434AssertRows(t, f.pool, "door-after-a", id, b)
}
