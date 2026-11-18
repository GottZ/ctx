//go:build integration

// Issue #343, part 1: an update that leaves title and content alone keeps a
// block's full-text vectors (ts_de, ts_en) as they are, and every write that
// changes the text still leaves the vectors of the generated expression.
//
// Until migration 178 both columns were stored generated columns. With a
// BEFORE ROW UPDATE trigger on the table (trg_block_retype, 174, and
// trg_block_embed_excluded, 175) PostgreSQL recomputes every stored generated
// column on every update, so each archive, metadata, tag or embedding write
// computed both vectors again and, for a wide block, wrote two new TOAST
// values. The fixtures go through the production write paths.
package store_test

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ftsVectorIDs is the TOAST value id of each vector of one block (0 = not
// toasted).
type ftsVectorIDs struct{ de, en int64 }

func readFTSVectorIDs(t *testing.T, pool *pgxpool.Pool, id string) ftsVectorIDs {
	t.Helper()
	var v ftsVectorIDs
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(pg_column_toast_chunk_id(ts_de)::bigint, 0),
		       COALESCE(pg_column_toast_chunk_id(ts_en)::bigint, 0)
		  FROM context_blocks WHERE id = $1`, id).Scan(&v.de, &v.en); err != nil {
		t.Fatalf("toast ids of %s: %v", id, err)
	}
	return v
}

// assertFTSVectorsOfText fails unless both vectors of the block are the
// generated expression of migration 044 over its current title and content.
func assertFTSVectorsOfText(t *testing.T, pool *pgxpool.Pool, id, when string) {
	t.Helper()
	var de, en bool
	if err := pool.QueryRow(context.Background(), `
		SELECT ts_de IS NOT DISTINCT FROM to_tsvector('german', coalesce(title::text, '') || ' ' || content),
		       ts_en IS NOT DISTINCT FROM to_tsvector('english', coalesce(title::text, '') || ' ' || content)
		  FROM context_blocks WHERE id = $1`, id).Scan(&de, &en); err != nil {
		t.Fatalf("%s: read vectors: %v", when, err)
	}
	if !de || !en {
		t.Errorf("%s: vectors are not those of the block's text (ts_de %v, ts_en %v)", when, de, en)
	}
}

// ftsWideText is a block body of transcript width: both vectors are stored
// out of line.
func ftsWideText(seed uint64) string { return unrankedText(seed, 35000, "keptvectormarker", 1) }

func TestFullTextVectorsKeptWithoutTextChange(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	scopes := []string{"private"}

	b, err := store.UpsertBlock(ctx, pool, "notes", "wide block of issue 343", ftsWideText(1), []string{"one"}, nil,
		"private", true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	before := readFTSVectorIDs(t, pool, b.ID)
	if before.de == 0 || before.en == 0 {
		t.Fatalf("fixture: the vectors of a 35 000-character block are not toasted (%+v)", before)
	}
	title, content := b.Title, ftsWideText(1)

	vec := make([]float32, 1024)
	vec[0] = 1
	for _, w := range []struct {
		name string
		run  func() error
	}{
		{"metadata update", func() error {
			_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Metadata: map[string]any{"probe": 1}}, scopes)
			return err
		}},
		{"tags update", func() error {
			_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Tags: []string{"one", "two"}}, scopes)
			return err
		}},
		{"title and content sent unchanged", func() error {
			_, changed, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Title: &title, Content: &content}, scopes)
			if err == nil && changed {
				t.Errorf("UpdateBlock reported a text change for the unchanged text")
			}
			return err
		}},
		{"upsert of the identical block", func() error {
			_, err := store.UpsertBlock(ctx, pool, "notes", title, content, []string{"one", "two"}, nil,
				"private", true, store.SensitivityWrite{}, "knowledge")
			return err
		}},
		{"embedding write", func() error { return store.StoreEmbedding(ctx, pool, b.ID, "test-embed", vec) }},
		{"archive", func() error { _, err := store.DeleteBlock(ctx, pool, b.ID, scopes); return err }},
	} {
		if err := w.run(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if got := readFTSVectorIDs(t, pool, b.ID); got != before {
			t.Errorf("%s rewrote the full-text vectors: TOAST ids %+v -> %+v", w.name, before, got)
		}
		assertFTSVectorsOfText(t, pool, b.ID, w.name)
	}
}

func TestFullTextVectorsFollowText(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	scopes := []string{"private"}
	find := func(term string) []string {
		t.Helper()
		rows, err := store.SearchBlocks(ctx, pool, nil, term, scopes, "", nil, 10, true, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		var titles []string
		for _, r := range rows {
			titles = append(titles, r.Title)
		}
		return titles
	}

	b, err := store.UpsertBlock(ctx, pool, "notes", "Lagerbestand alpha", "Die Quarzuhr tickt im Lager.", nil, nil,
		"private", true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "insert")
	if got := find("Quarzuhr"); len(got) != 1 {
		t.Errorf("after insert: search Quarzuhr = %v, want the block", got)
	}

	if _, err := store.UpsertBlock(ctx, pool, "notes", "Lagerbestand alpha", "Der Kompass liegt im Regal.", nil, nil,
		"private", true, store.SensitivityWrite{}, "knowledge"); err != nil {
		t.Fatalf("upsert with new content: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "upsert with new content")
	if got := find("Quarzuhr"); len(got) != 0 {
		t.Errorf("after the content change: search Quarzuhr = %v, want nothing", got)
	}
	if got := find("Kompass"); len(got) != 1 {
		t.Errorf("after the content change: search Kompass = %v, want the block", got)
	}

	newTitle := "Lagerbestand Sextant"
	if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Title: &newTitle}, scopes); err != nil {
		t.Fatalf("title update: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "title update")
	if got := find("Sextant"); len(got) != 1 {
		t.Errorf("after the title change: search Sextant = %v, want the block", got)
	}

	newContent := ftsWideText(2) + " Astrolabium"
	if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &newContent}, scopes); err != nil {
		t.Fatalf("content update: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "content update to a wide text")
	if got := find("Astrolabium"); len(got) != 1 {
		t.Errorf("after the wide content change: search Astrolabium = %v, want the block", got)
	}

}

// TestFullTextVectorsDirectWriteReplaced: a statement that names a vector
// column gets the vectors of the block's text, not its own value. The
// generated column refused such a write (428C9); the columns are ordinary
// since migration 178, so the trigger keeps them equal to the text.
func TestFullTextVectorsDirectWriteReplaced(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	b, err := store.UpsertBlock(ctx, pool, "notes", "Kartentisch", "Der Kompass liegt im Regal.", nil, nil,
		"private", true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET ts_de = to_tsvector('simple', 'fremd'), ts_en = NULL WHERE id = $1`, b.ID); err != nil {
		t.Fatalf("direct vector write: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "direct vector write")
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET ts_en = to_tsvector('simple', 'fremd') WHERE id = $1`, b.ID); err != nil {
		t.Fatalf("direct write of ts_en alone: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "direct write of ts_en alone")
}

// TestMigration178KeepsTheVectorsOfAFilledStore runs migration 178 over a
// store written at 177, the path a live database takes: both columns stop
// being generated, every block keeps the vectors (and the TOAST values) it
// held, and the triggers take over — an archive keeps them, a content change
// computes them for the new text.
func TestMigration178KeepsTheVectorsOfAFilledStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 177)
	scopes := []string{"private"}
	var ids []string
	for i, w := range []struct{ typeName, content string }{
		{"knowledge", ftsWideText(11)},
		{"knowledge", "Der Kompass liegt im Regal."},
		{"checkpoint", ftsWideText(12)},
	} {
		b, err := store.UpsertBlock(ctx, pool, "upgrade", fmt.Sprintf("upgrade block %d", i), w.content, nil, nil,
			"private", true, store.SensitivityWrite{}, w.typeName)
		if err != nil {
			t.Fatalf("upsert at 177: %v", err)
		}
		ids = append(ids, b.ID)
	}
	type snapshot struct {
		de, en string
		toast  ftsVectorIDs
	}
	read := func(id string) snapshot {
		t.Helper()
		var s snapshot
		if err := pool.QueryRow(ctx, `SELECT ts_de::text, ts_en::text FROM context_blocks WHERE id = $1`, id).Scan(&s.de, &s.en); err != nil {
			t.Fatalf("read vectors: %v", err)
		}
		s.toast = readFTSVectorIDs(t, pool, id)
		return s
	}
	before := map[string]snapshot{}
	for _, id := range ids {
		before[id] = read(id)
	}

	if err := store.RunMigrationsUpTo(ctx, pool, 178); err != nil {
		t.Fatalf("migrate 177 -> 178: %v", err)
	}
	var generated, triggers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_attribute WHERE attrelid = 'context_blocks'::regclass
		AND attname IN ('ts_de', 'ts_en') AND attgenerated <> ''`).Scan(&generated); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'context_blocks'::regclass
		AND tgname IN ('trg_block_fulltext', 'trg_block_fulltext_text') AND tgenabled = 'O'`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if generated != 0 || triggers != 2 {
		t.Fatalf("after 178: %d vector columns still generated (want 0), %d of the 2 triggers enabled", generated, triggers)
	}
	for _, id := range ids {
		if got := read(id); got != before[id] {
			t.Errorf("block %s: migration changed its vectors (TOAST %+v -> %+v, text equal: de %v en %v)",
				id, before[id].toast, got.toast, got.de == before[id].de, got.en == before[id].en)
		}
	}

	if _, err := store.DeleteBlock(ctx, pool, ids[0], scopes); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := read(ids[0]); got != before[ids[0]] {
		t.Errorf("archive after 178 rewrote the vectors: TOAST %+v -> %+v", before[ids[0]].toast, got.toast)
	}
	newContent := "Das Astrolabium liegt im Regal."
	if _, _, err := store.UpdateBlock(ctx, pool, ids[1], store.UpdateBlockData{Content: &newContent}, scopes); err != nil {
		t.Fatalf("content update: %v", err)
	}
	assertFTSVectorsOfText(t, pool, ids[1], "content update after 178")
	if got := read(ids[1]); got.de == before[ids[1]].de {
		t.Errorf("content update after 178 left the old vector in place")
	}
}

// ftsTextAssignment finds a PL/pgSQL statement that writes the new row's
// title or content, or the whole new row, at the start of a line.
var ftsTextAssignment = regexp.MustCompile(`(?im)^\s*NEW\s*(\.\s*"?(title|content)"?\s*)?:?=`)

// TestFullTextVectorTriggersSeeTheFinalText pins the invariant migration 178
// rests on: the vectors are computed from the text the row is stored with.
// BEFORE ROW triggers fire in name order (byte order), and a generated column
// was computed after all of them. So:
//   - an insert: no BEFORE INSERT trigger that fires after trg_block_fulltext
//     may change title or content;
//   - an update: no BEFORE UPDATE trigger may change them at all.
//     trg_block_fulltext_text fires only when the statement names title or
//     content; a trigger that changes the text of an update that names
//     neither would leave the vectors of the old text, whatever its name.
//
// The rule is read from the catalog: every such trigger's function must be
// PL/pgSQL (another language cannot be read here) and must not assign
// NEW.title, NEW.content or NEW as a whole.
func TestFullTextVectorTriggersSeeTheFinalText(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	const (
		tgRow    = 1
		tgBefore = 2
		tgInsert = 4
		tgUpdate = 16
	)
	rows, err := pool.Query(ctx, `
		SELECT t.tgname, t.tgtype, l.lanname, p.prosrc
		  FROM pg_trigger t
		  JOIN pg_proc p ON p.oid = t.tgfoid
		  JOIN pg_language l ON l.oid = p.prolang
		 WHERE t.tgrelid = 'context_blocks'::regclass AND NOT t.tgisinternal`)
	if err != nil {
		t.Fatalf("read triggers: %v", err)
	}
	defer rows.Close()
	own := map[string]int16{}
	checked := 0
	for rows.Next() {
		var name, lang, src string
		var tgtype int16
		if err := rows.Scan(&name, &tgtype, &lang, &src); err != nil {
			t.Fatal(err)
		}
		if name == "trg_block_fulltext" || name == "trg_block_fulltext_text" {
			own[name] = tgtype
			continue
		}
		if tgtype&tgRow == 0 || tgtype&tgBefore == 0 {
			continue
		}
		afterInsert := tgtype&tgInsert != 0 && name > "trg_block_fulltext"
		if !afterInsert && tgtype&tgUpdate == 0 {
			continue
		}
		checked++
		if lang != "plpgsql" {
			t.Errorf("BEFORE ROW trigger %s runs a %s function; this test can read only PL/pgSQL — check by hand that it leaves title and content alone, then extend the test", name, lang)
			continue
		}
		if m := ftsTextAssignment.FindString(src); m != "" {
			t.Errorf("BEFORE ROW trigger %s writes the text of the row (%q): the full-text vectors would not follow it", name, m)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := int16(tgRow | tgBefore | tgInsert | tgUpdate); own["trg_block_fulltext"] != want {
		t.Errorf("trg_block_fulltext has tgtype %d, want BEFORE INSERT OR UPDATE FOR EACH ROW (%d)", own["trg_block_fulltext"], want)
	}
	if want := int16(tgRow | tgBefore | tgUpdate); own["trg_block_fulltext_text"] != want {
		t.Errorf("trg_block_fulltext_text has tgtype %d, want BEFORE UPDATE FOR EACH ROW (%d)", own["trg_block_fulltext_text"], want)
	}
	if checked == 0 {
		t.Errorf("no other BEFORE ROW trigger was checked; the table has trg_block_retype and trg_block_embed_excluded")
	}
}

// TestFullTextVectorsIgnoreTheSessionSearchPath: the generated expression
// bound pg_catalog's to_tsvector and configurations when it was created; the
// trigger function resolves them at run time and pins search_path to
// pg_catalog for that. A writer whose session puts a schema with its own
// configuration 'german' ahead of pg_catalog (or holds one in pg_temp) gets
// the same vectors as everyone else.
func TestFullTextVectorsIgnoreTheSessionSearchPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for _, s := range []string{
		`CREATE SCHEMA hostile`,
		`CREATE TEXT SEARCH CONFIGURATION hostile.german (COPY = pg_catalog.simple)`,
		`CREATE TEXT SEARCH CONFIGURATION hostile.english (COPY = pg_catalog.simple)`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "hostile, pg_catalog, public"
	hostile, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer hostile.Close()

	b, err := store.UpsertBlock(ctx, hostile, "notes", "Häuser laufen", "Die Häuser laufen schnell über die Brücken.", nil, nil,
		"private", true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("insert on the hostile search_path: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "insert on the hostile search_path")
	content := "Die Bäume wachsen langsam."
	if _, _, err := store.UpdateBlock(ctx, hostile, b.ID, store.UpdateBlockData{Content: &content}, []string{"private"}); err != nil {
		t.Fatalf("content update on the hostile search_path: %v", err)
	}
	assertFTSVectorsOfText(t, pool, b.ID, "content update on the hostile search_path")

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `CREATE TEXT SEARCH CONFIGURATION pg_temp.german (COPY = pg_catalog.simple)`); err != nil {
		t.Fatalf("temporary configuration: %v", err)
	}
	var id string
	if err := conn.QueryRow(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name)
		VALUES ('notes', 'Brücken', 'Die Häuser laufen schnell.', 'private', 'knowledge') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("insert with pg_temp.german: %v", err)
	}
	assertFTSVectorsOfText(t, pool, id, "insert with a configuration german in pg_temp")
}

// TestFullTextVectorsUpdateCost bounds the WAL of archiving wide blocks by
// the WAL of archiving the same number of narrow ones: an update that leaves
// the text alone must not cost with the width of the text. Before migration
// 178 every archive wrote both vectors of the block anew (about 40 KB of TOAST
// per 35 000-character block, a multiple of a narrow block's whole update).
// The blocks are of type checkpoint, which the partial full-text GIN indexes
// leave out (migration 145): an update of a ranked block that is not HOT also
// enters every lexeme of its vectors into those indexes again, a cost that
// grows with the text as well and that migration 178 does not change.
func TestFullTextVectorsUpdateCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	scopes := []string{"private"}
	const k = 40
	write := func(prefix string, wide bool) []string {
		t.Helper()
		ids := make([]string, 0, k)
		for i := 0; i < k; i++ {
			content := "short note body"
			if wide {
				content = ftsWideText(uint64(100 + i))
			}
			b, err := store.UpsertBlock(ctx, pool, "compaction-checkpoints", prefix+string(rune('A'+i%26))+string(rune('a'+i/26)), content, nil, nil,
				"private", true, store.SensitivityWrite{}, "checkpoint")
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			ids = append(ids, b.ID)
		}
		return ids
	}
	archiveWAL := func(ids []string) int64 {
		t.Helper()
		if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		var lsn string
		if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if _, err := store.DeleteBlock(ctx, pool, id, scopes); err != nil {
				t.Fatalf("archive: %v", err)
			}
		}
		var wal int64
		if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint`, lsn).Scan(&wal); err != nil {
			t.Fatal(err)
		}
		return wal
	}
	narrow := write("narrow ", false)
	wide := write("wide ", true)
	narrowWAL := archiveWAL(narrow)
	wideWAL := archiveWAL(wide)
	t.Logf("WAL of %d archives: narrow %d bytes, wide %d bytes (%.2fx)", k, narrowWAL, wideWAL, float64(wideWAL)/float64(narrowWAL))
	if wideWAL > 2*narrowWAL {
		t.Errorf("archiving %d wide blocks wrote %d bytes of WAL, more than twice the %d of %d narrow ones: the update costs with the text width",
			k, wideWAL, narrowWAL, k)
	}
}

// TestFullTextVectorsConcurrentWriters: an update that leaves the text alone
// and waits for the row lock of a concurrent text change keeps the vectors of
// the text that committed first, and a text change that waits for a
// concurrent metadata write computes the vectors of its own text. Under READ
// COMMITTED the waiting update re-reads the row once the lock is free; the
// vectors must follow that row, not the snapshot the statement started with.
func TestFullTextVectorsConcurrentWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	for _, c := range []struct {
		name          string
		first, second string
	}{
		{"metadata update waits for a content change",
			`UPDATE context_blocks SET content = 'Der Sextant liegt im Regal.' WHERE id = $1`,
			`UPDATE context_blocks SET metadata = metadata || '{"w": 2}' WHERE id = $1`},
		{"content change waits for a metadata update",
			`UPDATE context_blocks SET metadata = metadata || '{"w": 1}' WHERE id = $1`,
			`UPDATE context_blocks SET content = 'Das Astrolabium liegt im Regal.' WHERE id = $1`},
		{"title change waits for a content change",
			`UPDATE context_blocks SET content = 'Die Quarzuhr liegt im Regal.' WHERE id = $1`,
			`UPDATE context_blocks SET title = 'Werkbank' WHERE id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := store.UpsertBlock(ctx, pool, "notes", "Kartentisch "+c.name, "Der Kompass liegt im Regal.", nil, nil,
				"private", true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, c.first, b.ID); err != nil {
				t.Fatalf("first: %v", err)
			}
			done := make(chan error, 1)
			go func() { _, err := pool.Exec(ctx, c.second, b.ID); done <- err }()
			// The second statement has to be waiting for the row lock.
			waiting := false
			for i := 0; i < 200 && !waiting; i++ {
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE 'UPDATE context_blocks%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if !waiting {
					time.Sleep(10 * time.Millisecond)
				}
			}
			if !waiting {
				t.Fatalf("the second update never waited for the row lock")
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("second: %v", err)
			}
			assertFTSVectorsOfText(t, pool, b.ID, c.name)
		})
	}
}
