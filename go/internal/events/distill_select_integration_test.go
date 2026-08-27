//go:build integration

// Gate A02-6 (design/02 §7.2, "A02-6 — Auswahl"): the ledger, the cross-run
// dedup, the credential drop proved AT THE DUMP, the chunk-level dedup key, the
// watermark under a cancellation, and the BA13 dump target.
//
// The ARM-WIDE red is recorded in the wave report and reproducible against the
// A02-5 tree: with the same material and the same open gates the run row closes
// as partial with rows_seen = rows_selected = chars_selected = 0 and
// watermark_to = 0. Every probe below that has a red of its own beyond that
// state names it in its comment.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestDistillSelection -count=1 -v
package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/distillsource/ctxcheckpoint"
	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/testdb"
)

// a6Ledger reads the ledger columns of the newest row of a source key.
type a6Row struct {
	seen, selected, cred, dup int
	chars                     int64
	outcome, errClass         string
	from, to                  int64
}

func a6Ledger(t *testing.T, pool *pgxpool.Pool, key string) a6Row {
	t.Helper()
	var r a6Row
	err := pool.QueryRow(context.Background(), `
		SELECT rows_seen, rows_selected, rows_dropped_cred, rows_dropped_dup,
		       chars_selected, outcome, COALESCE(error, ''), watermark_from, watermark_to
		  FROM distill_run
		 WHERE source_key = $1
		 ORDER BY started_at DESC LIMIT 1`, key).
		Scan(&r.seen, &r.selected, &r.cred, &r.dup, &r.chars, &r.outcome, &r.errClass, &r.from, &r.to)
	if err != nil {
		t.Fatalf("read ledger for %q: %v", key, err)
	}
	return r
}

// a6Config is dfConfig plus the three values this wave introduces.
func a6Config(dumpDir string, maxRunes, minRunes int) *config.Config {
	c := dfConfig()
	c.Distill.MaxRowRunes = maxRunes
	c.Distill.MinRowRunes = minRunes
	c.Distill.DryRunDir = dumpDir
	return c
}

// a6Text is filler with substance, n runes long.
func a6Text(n int) string {
	const unit = "belegbare prosa "
	return strings.Repeat(unit, n/len(unit)+1)
}

// a6Para is filler that is DISTINGUISHABLE from other filler. The dedup key is
// the normalized chunk text, so two paragraphs of the same repeated unit are
// one chunk as far as the ledger is concerned — which is correct behaviour and
// a useless fixture.
func a6Para(tag string, n int) string {
	return tag + " " + a6Text(n)
}

// a6Titles numbers the seeded blocks: (category, title, scope) is unique, and
// two parts of one manifest would otherwise collide on the second insert.
var a6Titles int

// a6SeedPart writes one part block with the given transcript body.
func a6SeedPart(t *testing.T, pool *pgxpool.Pool, root, body string, at time.Time) string {
	t.Helper()
	content := "# Compaction checkpoint " + root + "\n\n" +
		"## Compaction source evidence\n\n- Source blocks: 1\n\n" +
		"## Direct transcript\n\n" + body
	a6Titles++
	var id string
	if err := pool.QueryRow(context.Background(), `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, $2, $3, $4, 'checkpoint',
        jsonb_build_object('root_session_id', $5::text, 'part', '1'), $6)
RETURNING id::text`, dfCategory, fmt.Sprintf("%s-part-%d", root, a6Titles), content, dfScope, root, at).Scan(&id); err != nil {
		t.Fatalf("insert part: %v", err)
	}
	return id
}

// a6SeedManifest lists the given parts in one manifest and returns its
// microsecond watermark.
func a6SeedManifest(t *testing.T, pool *pgxpool.Pool, root string, at time.Time, partIDs []string) int64 {
	t.Helper()
	a6Titles++
	if _, err := pool.Exec(context.Background(), `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, $2, 'manifest', $3, 'checkpoint',
        jsonb_build_object('root_session_id', $4::text,
                           'source_block_ids', to_jsonb($5::text[])), $6)`,
		dfCategory, fmt.Sprintf("%s-manifest-%d", root, a6Titles), dfScope, root, partIDs, at); err != nil {
		t.Fatalf("insert manifest: %v", err)
	}
	return at.UnixMicro()
}

// a6Clean removes a root's blocks and its journal/ledger rows at subtest end.
func a6Clean(t *testing.T, pool *pgxpool.Pool, root, key string) {
	t.Helper()
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM context_blocks WHERE metadata->>'root_session_id' = $1`, root)
		_, _ = pool.Exec(bg, `DELETE FROM distill_seen WHERE source_key = $1`, key)
	})
}

// a6Reset is the "same range again" state of gate 2: the journal series and the
// dedup ledger of one source, gone.
func a6Reset(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	bg := context.Background()
	if _, err := pool.Exec(bg, `DELETE FROM distill_run WHERE source_key = $1`, key); err != nil {
		t.Fatalf("reset journal: %v", err)
	}
	if _, err := pool.Exec(bg, `DELETE FROM distill_seen WHERE source_key = $1`, key); err != nil {
		t.Fatalf("reset dedup ledger: %v", err)
	}
}

type a6Rec struct {
	Block string `json:"block"`
	Chunk int    `json:"chunk"`
	Hash  string `json:"hash"`
	Runes int    `json:"runes"`
	Text  string `json:"text"`
}

// a6Dump reads every dump file of a directory.
func a6Dump(t *testing.T, dir string) []a6Rec {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil {
		t.Fatalf("glob dump: %v", err)
	}
	var out []a6Rec
	for _, n := range names {
		f, err := os.Open(n)
		if err != nil {
			t.Fatalf("open dump %s: %v", n, err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var r a6Rec
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("decode dump line: %v", err)
			}
			out = append(out, r)
		}
		f.Close()
	}
	return out
}

func TestDistillSelection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	// GATE 1 — the ledger carries the numbers of a real selection, and the
	// watermark moves with them. RED (A02-5): all four counters and
	// chars_selected stay 0, outcome partial, watermark_to 0.
	t.Run("LedgerCarriesTheNumbers", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_100000_a026led"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		dir := a6DumpRoot(t)

		at := time.Now().Add(-2 * time.Hour)
		p1 := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Text(600), at)
		p2 := a6SeedPart(t, pool, root, "### Message 2 — assistant\n"+a6Text(700), at)
		wm := a6SeedManifest(t, pool, root, at, []string{p1, p2})

		dfScheduler(pool, a6Config(dir, 4000, 200), nil).distillOnce(ctx, dfNoDemand)

		r := a6Ledger(t, pool, key)
		if r.seen != 2 || r.selected != 2 || r.cred != 0 || r.dup != 0 {
			t.Fatalf("ledger = seen %d / selected %d / cred %d / dup %d, want 2/2/0/0", r.seen, r.selected, r.cred, r.dup)
		}
		if r.chars < 1200 {
			t.Fatalf("chars_selected = %d, want the summed rune length of both chunks (>= 1200)", r.chars)
		}
		if r.outcome != "ok" || r.errClass != "" {
			t.Fatalf("outcome/error = %q/%q, want ok//", r.outcome, r.errClass)
		}
		if r.from != 0 || r.to != wm {
			t.Fatalf("watermark %d..%d, want 0..%d — the run covered the manifest", r.from, r.to, wm)
		}
		recs := a6Dump(t, dir)
		if len(recs) != 2 {
			t.Fatalf("dump holds %d chunks, want 2", len(recs))
		}
		// §4.2.5 rule 1 as an ASSERTION rather than a second strip: the reader
		// cuts the boilerplate, so no selected chunk may still carry it.
		for _, rec := range recs {
			if strings.Contains(rec.Text, "Compaction source evidence") || strings.Contains(rec.Text, "# Compaction checkpoint") {
				t.Fatalf("a selected chunk still carries the boilerplate head: %.80q", rec.Text)
			}
		}
	})

	// GATE 2 — reproducibility. Same range, watermark reset and dedup ledger
	// emptied ⇒ identical ledger numbers.
	t.Run("TwoRunsOverTheSameRangeAreIdentical", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_101000_a026rep"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		at := time.Now().Add(-3 * time.Hour)
		p1 := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Text(900), at)
		p2 := a6SeedPart(t, pool, root, a6Text(50), at) // below the floor
		a6SeedManifest(t, pool, root, at, []string{p1, p2})

		first := a6DumpRoot(t)
		dfScheduler(pool, a6Config(first, 4000, 200), nil).distillOnce(ctx, dfNoDemand)
		a := a6Ledger(t, pool, key)

		a6Reset(t, pool, key)
		second := a6DumpRoot(t)
		dfScheduler(pool, a6Config(second, 4000, 200), nil).distillOnce(ctx, dfNoDemand)
		b := a6Ledger(t, pool, key)

		if a != b {
			t.Fatalf("two runs over the same range differ:\n first %+v\nsecond %+v", a, b)
		}
		if a.seen != 2 || a.selected != 1 {
			t.Fatalf("ledger = seen %d / selected %d, want 2/1 (the short part fails the floor)", a.seen, a.selected)
		}
		if x, y := a6Dump(t, first), a6Dump(t, second); len(x) != len(y) || len(x) != 1 || x[0].Hash != y[0].Hash {
			t.Fatalf("the two dumps differ: %d vs %d records", len(x), len(y))
		}
	})

	// GATE 3 — BA6. The proof is the DUMP, not the counter: a counter can be
	// raised by a run that still put the secret in front of the model.
	t.Run("CredentialDropIsProvedAtTheDump", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_102000_a026cred"
		const secret = "AKIAIOSFODNN7EXAMPLE"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		dir := a6DumpRoot(t)

		at := time.Now().Add(-4 * time.Hour)
		clean := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Text(600), at)
		dirty := a6SeedPart(t, pool, root, "### Message 2 — assistant\n"+a6Text(600)+"\nexport AWS_ACCESS_KEY_ID="+secret+"\n", at)
		a6SeedManifest(t, pool, root, at, []string{clean, dirty})

		dfScheduler(pool, a6Config(dir, 4000, 200), nil).distillOnce(ctx, dfNoDemand)

		r := a6Ledger(t, pool, key)
		if r.cred != 1 || r.selected != 1 || r.seen != 2 {
			t.Fatalf("ledger = seen %d / selected %d / cred %d, want 2/1/1", r.seen, r.selected, r.cred)
		}
		recs := a6Dump(t, dir)
		if len(recs) != 1 || recs[0].Block != clean {
			t.Fatalf("dump holds %d records, want exactly the clean part %s", len(recs), clean)
		}
		for _, rec := range recs {
			if strings.Contains(rec.Text, secret) || strings.Contains(rec.Text, "AKIA") {
				t.Fatal("the secret reached the dry-run dump")
			}
		}
		// The file NAMES carry no foreign text either, and the journal row
		// carries no secret in any column.
		names, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, n := range names {
			if strings.Contains(filepath.Base(n), root) || strings.Contains(filepath.Base(n), "AKIA") {
				t.Fatalf("dump file name carries foreign text: %s", filepath.Base(n))
			}
		}
		var dumpRow string
		if err := pool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM distill_run d WHERE source_key = $1`, key).Scan(&dumpRow); err != nil {
			t.Fatalf("dump journal row: %v", err)
		}
		if strings.Contains(dumpRow, "AKIA") {
			t.Fatalf("the journal row carries the secret: %s", dumpRow)
		}
	})

	// GATE 4 — the cross-run half, built on the live shape: one part listed by
	// two manifests of the same root (live 019f5b5f-e51c-7a94-a374-91c104491dd2).
	// The second manifest arrives in a SECOND RUN, so only the durable ledger
	// can catch it. RED without distill_seen: the second run selects it again.
	t.Run("CrossRunDedupOnTheDoublyListedPart", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_103000_a026dup"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		at := time.Now().Add(-5 * time.Hour)
		part := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Text(800), at)
		a6SeedManifest(t, pool, root, at, []string{part})

		first := a6DumpRoot(t)
		dfScheduler(pool, a6Config(first, 4000, 200), nil).distillOnce(ctx, dfNoDemand)
		if r := a6Ledger(t, pool, key); r.selected != 1 || r.dup != 0 {
			t.Fatalf("first run = selected %d / dup %d, want 1/0", r.selected, r.dup)
		}

		// The same part, listed again by a later manifest.
		a6SeedManifest(t, pool, root, at.Add(time.Minute), []string{part})
		second := a6DumpRoot(t)
		dfScheduler(pool, a6Config(second, 4000, 200), nil).distillOnce(ctx, dfNoDemand)

		r := a6Ledger(t, pool, key)
		if r.dup < 1 || r.selected != 0 {
			t.Fatalf("second run = selected %d / dup %d, want 0/>=1", r.selected, r.dup)
		}
		if recs := a6Dump(t, second); len(recs) != 0 {
			t.Fatalf("the second run dumped %d records although the material was seen", len(recs))
		}
		var last, seen int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM distill_seen WHERE source_key = $1`, key).Scan(&seen); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		if seen != 1 {
			t.Fatalf("distill_seen holds %d rows for one chunk, want 1 (the repeat is an UPDATE)", seen)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM distill_seen
			 WHERE source_key = $1 AND last_seen > now() - interval '1 minute'`, key).Scan(&last); err != nil {
			t.Fatalf("ledger age: %v", err)
		}
		if last != 1 {
			t.Fatal("last_seen was not slid forward on the repeat — a cyclic output would be paid for again")
		}
	})

	// GATE 5 — the dedup key is the CHUNK. A part whose first chunk is already
	// in the ledger still delivers the rest. RED with a part hash: the whole
	// part falls.
	t.Run("ChunkHashNotPartHash", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_104000_a026chunk"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		at := time.Now().Add(-6 * time.Hour)
		body := "### Message 1 — user\n" + a6Para("alpha", 300) + "\n\n" + a6Para("beta", 300) + "\n\n" + a6Para("gamma", 300)
		a6SeedManifest(t, pool, root, at, []string{a6SeedPart(t, pool, root, body, at)})

		first := a6DumpRoot(t)
		dfScheduler(pool, a6Config(first, 400, 100), nil).distillOnce(ctx, dfNoDemand)
		recs := a6Dump(t, first)
		if len(recs) < 3 {
			t.Fatalf("the part produced %d chunks, want >= 3 for this probe", len(recs))
		}

		// Only the FIRST chunk stays known; the journal series is reset so the
		// same range is read again.
		if _, err := pool.Exec(ctx, `DELETE FROM distill_run WHERE source_key = $1`, key); err != nil {
			t.Fatalf("reset journal: %v", err)
		}
		var firstChunk string
		for _, rec := range recs {
			if rec.Chunk == 1 {
				firstChunk = rec.Hash
			}
		}
		if firstChunk == "" {
			t.Fatal("the dump names no chunk 1")
		}
		if _, err := pool.Exec(ctx, `
			DELETE FROM distill_seen WHERE source_key = $1 AND row_hash <> decode($2, 'hex')`,
			key, firstChunk); err != nil {
			t.Fatalf("trim ledger: %v", err)
		}

		second := a6DumpRoot(t)
		dfScheduler(pool, a6Config(second, 400, 100), nil).distillOnce(ctx, dfNoDemand)
		r := a6Ledger(t, pool, key)
		if r.dup != 1 || r.selected != len(recs)-1 {
			t.Fatalf("second run = selected %d / dup %d, want %d/1 — a part hash would drop all %d chunks",
				r.selected, r.dup, len(recs)-1, len(recs))
		}
		out := a6Dump(t, second)
		if len(out) != len(recs)-1 {
			t.Fatalf("dump holds %d chunks, want %d", len(out), len(recs)-1)
		}
		for _, rec := range out {
			if rec.Chunk == 1 {
				t.Fatal("the already seen first chunk was dumped again")
			}
		}
	})

	// GATE 6 — the watermark stands on the last DURABLE batch. The test form of
	// SIGTERM is a context cancelled inside the second Read. RED against a
	// version that writes the watermark before the batch is durable: the row
	// then carries the second batch's mark and its material is never read again.
	t.Run("WatermarkStopsAtTheLastDurableBatch", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_105000_a026wm"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		const firstWM, secondWM = int64(1000), int64(2000)

		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		reads := 0
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: secondWM},
			hasNew:   map[string]bool{root: true},
		}
		src.readFn = func(after int64) (distillsource.Batch, error) {
			reads++
			switch reads {
			case 1:
				return a6Batch(firstWM, "a", a6Text(400)), nil
			case 2:
				stop() // the SIGTERM moment: cancelled DURING the second read
				return a6Batch(secondWM, "b", a6Text(400)), nil
			default:
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
		}
		dir := a6DumpRoot(t)
		dfScheduler(pool, a6Config(dir, 4000, 200), src).distillOnce(runCtx, dfNoDemand)

		var outcome string
		var to int64
		if err := pool.QueryRow(ctx, `
			SELECT outcome, watermark_to FROM distill_run
			 WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&outcome, &to); err != nil {
			t.Fatalf("read run row: %v", err)
		}
		if outcome != "running" || to != firstWM {
			t.Fatalf("interrupted row = %s at %d, want running at %d", outcome, to, firstWM)
		}
		if got := dfDerive(t, pool, key); got != 0 {
			t.Fatalf("derivation = %d before the sweep, want 0 (a running row is invisible to it)", got)
		}

		// Sweep + restart: the value survives and the next run resumes there.
		next := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: secondWM},
			hasNew:   map[string]bool{root: true},
		}
		s := dfScheduler(pool, a6Config(a6DumpRoot(t), 4000, 200), next)
		s.distillStartupSweep(ctx)
		if got := dfDerive(t, pool, key); got != firstWM {
			t.Fatalf("derivation after the sweep = %d, want %d", got, firstWM)
		}
		s.distillOnce(ctx, dfNoDemand)
		if next.lastAfter != firstWM {
			t.Fatalf("the restarted run read from %d, want %d", next.lastAfter, firstWM)
		}
	})

	// GATE 6, second half — THE WRITE ORDER ITSELF. The cancellation probe above
	// cannot see it: with a dead context every statement of the batch fails, so
	// a version that writes the watermark first fails to write it too, and both
	// versions look alike. This probe therefore breaks the DURABLE step instead
	// of the context — the dedup ledger disappears between the batches — and
	// then asks what the row says.
	//
	// RED, measured against the version that calls distillAdvance before
	// distillBatch: the row carries the SECOND batch's watermark although that
	// batch never became durable, and after the sweep its material is never read
	// again.
	t.Run("AFailedBatchDoesNotMoveTheWatermark", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_105500_a026ord"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		const firstWM, secondWM = int64(3000), int64(4000)

		restored := false
		restore := func() {
			if restored {
				return
			}
			restored = true
			if _, err := pool.Exec(ctx, `ALTER TABLE distill_seen_a6 RENAME TO distill_seen`); err != nil {
				t.Fatalf("restore distill_seen: %v", err)
			}
		}
		t.Cleanup(restore)

		reads := 0
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: secondWM},
			hasNew:   map[string]bool{root: true},
		}
		src.readFn = func(after int64) (distillsource.Batch, error) {
			reads++
			switch reads {
			case 1:
				return a6Batch(firstWM, "a", a6Text(400)), nil
			case 2:
				// The durable half of the NEXT batch is now impossible.
				if _, err := pool.Exec(ctx, `ALTER TABLE distill_seen RENAME TO distill_seen_a6`); err != nil {
					t.Fatalf("break the ledger: %v", err)
				}
				return a6Batch(secondWM, "b", a6Text(400)), nil
			default:
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
		}
		dfScheduler(pool, a6Config(a6DumpRoot(t), 4000, 200), src).distillOnce(ctx, dfNoDemand)
		restore()

		r := a6Ledger(t, pool, key)
		if r.to != firstWM {
			t.Fatalf("watermark_to = %d after a failed second batch, want %d — the mark moved without a durable batch", r.to, firstWM)
		}
		if r.outcome != "failed" || r.errClass != "query_failed" {
			t.Fatalf("row = %s/%s, want failed/query_failed", r.outcome, r.errClass)
		}
		if r.seen != 1 || r.selected != 1 {
			t.Fatalf("ledger = seen %d / selected %d, want 1/1 — only the first batch counted", r.seen, r.selected)
		}
	})

	// REVIEW #3 — an EMPTY batch that reports Complete=false must not close as
	// `ok`. The reader of this source cannot produce that shape today, but the
	// hermes adapter does (hermesadapter.go:149, a window whose every row was
	// undecodable), and both readers do it for a non-positive cap. Closing it as
	// `ok` would journal a covered range for a batch that covered nothing — the
	// silent null operation D-02 §4.2.1(b) wants to see red.
	//
	// RED against b8976774: `outcome="ok" watermark=0..0 seen=0`.
	t.Run("AnIncompleteEmptyBatchIsNotOk", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_110000_a026inc"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: 5000},
			hasNew:   map[string]bool{root: true},
			readFn: func(after int64) (distillsource.Batch, error) {
				// Exactly hermesadapter.go:149 for a fully dropped window.
				return distillsource.Batch{Watermark: after, Complete: false}, nil
			},
		}
		dfScheduler(pool, a6Config(a6DumpRoot(t), 4000, 200), src).distillOnce(ctx, dfNoDemand)

		r := a6Ledger(t, pool, key)
		if r.outcome == "ok" {
			t.Fatalf("an incomplete batch that delivered nothing closed as ok: %+v", r)
		}
		if r.outcome != "partial" {
			t.Fatalf("outcome = %q, want partial (covered material, did not finish it)", r.outcome)
		}
		if r.seen != 0 || r.to != 0 {
			t.Fatalf("ledger = seen %d / watermark_to %d, want 0/0 — nothing was covered", r.seen, r.to)
		}
	})

	// REVIEW #4 — the rune cap is the VALIDATOR's authority. The clamp that used
	// to absorb a non-positive value is gone, so the arm now does what the
	// reader's contract says for that input: it reads nothing, covers nothing
	// and journals a run that did not finish. The unit half of this probe
	// (TestDistillSizingIsTheValidatorsAuthority) pins the validator rule that
	// makes this state unreachable in a daemon at all.
	//
	// RED against b8976774: the clamp silently substituted 4000 and the run
	// closed `ok` with the material processed — a configuration the validator
	// refuses would have run as if it were valid.
	t.Run("ANonPositiveRuneCapProcessesNothing", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_111000_a026cap"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		dir := a6DumpRoot(t)

		at := time.Now().Add(-2 * time.Hour)
		p := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Text(900), at)
		a6SeedManifest(t, pool, root, at, []string{p})

		dfScheduler(pool, a6Config(dir, 0, 200), nil).distillOnce(ctx, dfNoDemand)

		r := a6Ledger(t, pool, key)
		if r.seen != 0 || r.selected != 0 || r.to != 0 {
			t.Fatalf("ledger = seen %d / selected %d / watermark_to %d, want 0/0/0 — a refused cap must not be substituted",
				r.seen, r.selected, r.to)
		}
		if r.outcome == "ok" {
			t.Fatalf("a run that read nothing closed as ok: %+v", r)
		}
		if recs := a6Dump(t, dir); len(recs) != 0 {
			t.Fatalf("the dump holds %d records although the cap was non-positive", len(recs))
		}
	})

	// REVIEW #6 — the CONTRACT the missing seam scan (§4.2.5 stage b) rests on,
	// bound in THIS package instead of borrowed from the reader's tests: all
	// chunks of a part arrive in ONE batch, consecutively, and their
	// concatenation is the whole stripped body. If a later reader change splits
	// a part across batches or reorders items, stage (a) silently degrades to
	// stage (c) and the seam case comes back — this probe goes red first.
	//
	// The second half runs the seam secret over the PRODUCTION path (the review's
	// own S1): a 64-hex run 30/34 across the real 4000-rune chunk boundary.
	t.Run("ThePartArrivesWholeSoTheSeamScanIsCovered", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20260827_112000_a026seam"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		// No newline anywhere, so cutPoint falls through to the hard rune cap
		// and the boundary lands inside the secret. The spaces around the run
		// are not decoration: reHexBlob is \b-anchored (sensitivity.go:69), so a
		// hex run embedded in word characters is not a secret to the detector at
		// all and the probe would prove nothing.
		secret := strings.Repeat("a1b2c3d4", 8) // 64 hex characters
		head := strings.Repeat("x", 3969) + " " // boundary lands 30 runes into the secret
		tail := " " + strings.Repeat("y", 2000) // ... and 34 runes before its end
		body := head + secret + tail            // 6035 runes
		at := time.Now().Add(-2 * time.Hour)
		partID := a6SeedPart(t, pool, root, body, at)
		a6SeedManifest(t, pool, root, at, []string{partID})

		// (1) THE CONTRACT, read straight from the production reader.
		reader, err := ctxcheckpoint.New(pool, ctxcheckpoint.Options{
			Label: dfLabel, Scope: dfScope, Category: dfCategory,
			MaxSessions: 4, MaxManifests: 400,
		})
		if err != nil {
			t.Fatalf("build reader: %v", err)
		}
		b, err := reader.Read(ctx, root, 0, 400, 4000)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(b.Items) < 2 {
			t.Fatalf("the part yielded %d chunks, want >= 2 for a seam to exist", len(b.Items))
		}
		parts := distillParts(b.Items)
		if len(parts) != 1 || len(parts[0]) != len(b.Items) {
			t.Fatalf("the part arrived as %d groups (%d items) — stage (a) can only scan a whole part",
				len(parts), len(b.Items))
		}
		if got := distillPartBody(parts[0]); got != body {
			t.Fatalf("the reassembled body differs from the stripped part body (%d vs %d runes) — "+
				"stage (a) would scan something other than what the model gets",
				len([]rune(got)), len([]rune(body)))
		}
		// The red state this contract protects against, asserted rather than
		// assumed: neither chunk alone carries a detectable secret.
		for i, it := range b.Items {
			if _, hit := sensitivity.Scan(it.Text); hit {
				t.Fatalf("chunk %d already flags on its own — the probe would prove nothing", i+1)
			}
		}

		// (2) The production path end to end.
		dir := a6DumpRoot(t)
		dfScheduler(pool, a6Config(dir, 4000, 200), nil).distillOnce(ctx, dfNoDemand)
		r := a6Ledger(t, pool, key)
		if r.cred != len(b.Items) || r.selected != 0 {
			t.Fatalf("ledger = seen %d / selected %d / cred %d, want %d dropped on the seam secret",
				r.seen, r.selected, r.cred, len(b.Items))
		}
		for _, rec := range a6Dump(t, dir) {
			if strings.Contains(rec.Text, secret[:30]) || strings.Contains(rec.Text, secret[30:]) {
				t.Fatal("half of the seam secret reached the dry-run dump")
			}
		}
	})

	// GATE 7 — BA13 at the arm. A dump target inside a git working copy stops
	// the tick BEFORE the corpus is read at all, and says so in the journal.
	t.Run("ADumpTargetInsideAGitWorkingCopyStopsTheTick", func(t *testing.T) {
		dfTruncate(t, pool)
		work := a6DumpRoot(t)
		if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(work, "distill-dryrun")

		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: "20260827_106000_a026git"}},
			head:     map[string]int64{"20260827_106000_a026git": 4000},
			hasNew:   map[string]bool{"20260827_106000_a026git": true},
		}
		if dfScheduler(pool, a6Config(target, 4000, 200), src).distillOnce(ctx, dfNoDemand) {
			t.Fatal("the arm reached its per-session work with a refused dump target")
		}
		if src.reads != 0 {
			t.Fatalf("the corpus was read %d times although the dump target was refused", src.reads)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("the refused target was created anyway: %v", err)
		}
		rows := dfRows(t, pool)
		tickKey := distillSourceKey(dfLabel, dfScope, "")
		if len(rows) != 1 || rows[0].sourceKey != tickKey ||
			rows[0].outcome != "failed" || rows[0].errClass != "block_write_failed" {
			t.Fatalf("journal = %+v, want one failed/block_write_failed row under the tick key", rows)
		}
		if rows[0].from != 0 || rows[0].to != 0 {
			t.Fatalf("tick row is %d..%d, want 0..0", rows[0].from, rows[0].to)
		}
	})
}

// a6Batch is one steered batch of a single part.
func a6Batch(wm int64, block, text string) distillsource.Batch {
	return distillsource.Batch{
		Items: []distillsource.Item{{
			Text:   text,
			Origin: distillsource.Origin{BlockID: block, ChunkIndex: 1},
		}},
		Watermark: wm,
		Complete:  true,
	}
}
