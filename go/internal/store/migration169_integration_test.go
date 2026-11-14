//go:build integration

// Migration 169 gates (issue #191): chunk rows that a pre-fix binary moved out
// of the 'chunk' lifecycle — the dream supersedes side-effect ('snapshot'),
// its reverse or migration 154 ('knowledge'), a dream promotion after that
// ('canonical'), the daily report on a chunk-held title ('synthesis') — are
// outside InsertChunk's arbiter, and a changed-content ingest of their slot
// fails with 23505. The repair puts them back so that every slot is
// ingestable again.
//
// The chunks are written by store.InsertChunk (the production writer). The
// damage itself is seeded with a direct UPDATE: the fixed binary has no path
// that produces it any more, and these are exactly the column values the old
// writers left behind (store/supersedes_state.go, dream/synthesize_report.go
// before the fix). Every case shares one database, so each statement proves
// its predicate in the others' presence.
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestMigration169 -count=1 -v
package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m169File = "169_chunk_lifecycle_repair.sql"

func m169Apply(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.FS.ReadFile(m169File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", m169File, err)
	}
	ctx := context.Background()
	if err := pgxdb.Write(ctx, pool, pgxdb.At("m169 apply"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(sql))
		return err
	}); err != nil {
		t.Fatalf("apply %s: %v", m169File, err)
	}
}

func m169Source(t *testing.T, pool *pgxpool.Pool, path string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ($1, 'm169', 'private') RETURNING id::text`,
		path).Scan(&id); err != nil {
		t.Fatalf("seed source %s: %v", path, err)
	}
	return id
}

func m169Chunk(t *testing.T, pool *pgxpool.Pool, sourceID, title, content string) string {
	t.Helper()
	b, err := store.InsertChunk(context.Background(), pool, sourceID, 0, "vault", title, content, nil, nil, "private")
	if err != nil {
		t.Fatalf("ingest chunk %q: %v", title, err)
	}
	return b.ID
}

// m169Damage leaves a row as a pre-fix lifecycle writer left it.
func m169Damage(t *testing.T, pool *pgxpool.Pool, id, lifecycle, pointer string) {
	t.Helper()
	var ptr any
	if pointer != "" {
		ptr = pointer
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET lifecycle_state = $2, superseded_by = $3::uuid WHERE id = $1::uuid`,
		id, lifecycle, ptr); err != nil {
		t.Fatalf("damage %s: %v", id, err)
	}
}

type m169State struct {
	lifecycle, pointer string
	archived           bool
	repair             string // metadata->'chunk_lifecycle_repair' as text, "" = absent
	updatedAt          time.Time
}

func m169Read(t *testing.T, pool *pgxpool.Pool, id string) m169State {
	t.Helper()
	var s m169State
	if err := pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, COALESCE(superseded_by::text, ''), is_archived,
		        COALESCE((metadata->'chunk_lifecycle_repair')::text, ''), updated_at
		   FROM context_blocks WHERE id = $1::uuid`, id,
	).Scan(&s.lifecycle, &s.pointer, &s.archived, &s.repair, &s.updatedAt); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

// m169Ingest writes changed content into slot 0 of sourceID and returns the
// row it landed on, or the error.
func m169Ingest(pool *pgxpool.Pool, sourceID, title, content string) (string, error) {
	b, err := store.InsertChunk(context.Background(), pool, sourceID, 0, "vault", title, content, nil, nil, "private")
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func m169Is23505(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func TestMigration169_ChunkLifecycleRepair(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 168)

	pointer, err := store.UpsertBlock(ctx, pool, "decisions", "m169 superseder", "m169 newer block",
		nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed superseder: %v", err)
	}

	// (1) one damaged row alone in its slot, for every lifecycle an old
	// writer could leave → back to chunk, pointer cleared, live.
	type single struct{ lifecycle, pointer, source, id string }
	singles := []*single{
		{lifecycle: "snapshot", pointer: pointer.ID},
		{lifecycle: "knowledge"},
		{lifecycle: "canonical"},
		{lifecycle: "synthesis"},
	}
	for _, c := range singles {
		c.source = m169Source(t, pool, "/m169/single-"+c.lifecycle+".md")
		c.id = m169Chunk(t, pool, c.source, "m169 single "+c.lifecycle, "m169 content "+c.lifecycle)
		m169Damage(t, pool, c.id, c.lifecycle, c.pointer)
	}

	// (2) the slot was re-ingested under a new title after the damage: the
	// fresh insert missed the arbiter and now holds the slot as a live chunk.
	// The stale row keeps the old title, so ingesting the old title again
	// would collide with it → archived (the ingest would have replaced it).
	takenSrc := m169Source(t, pool, "/m169/taken.md")
	takenOld := m169Chunk(t, pool, takenSrc, "m169 taken old title", "m169 taken v1")
	m169Damage(t, pool, takenOld, "snapshot", pointer.ID)
	takenLive, err := m169Ingest(pool, takenSrc, "m169 taken new title", "m169 taken v2")
	if err != nil || takenLive == takenOld {
		t.Fatalf("re-ingest under a new title: id=%s err=%v, want a fresh row", takenLive, err)
	}
	takenLiveBefore := m169Read(t, pool, takenLive)

	// (3) two damaged rows in one slot, no live chunk → the newest returns to
	// the slot, the older one is archived.
	twoSrc := m169Source(t, pool, "/m169/two.md")
	twoOld := m169Chunk(t, pool, twoSrc, "m169 two first title", "m169 two v1")
	m169Damage(t, pool, twoOld, "knowledge", "")
	twoNew, err := m169Ingest(pool, twoSrc, "m169 two second title", "m169 two v2")
	if err != nil || twoNew == twoOld {
		t.Fatalf("second row in the slot: id=%s err=%v", twoNew, err)
	}
	m169Damage(t, pool, twoNew, "snapshot", pointer.ID)

	// (4) an OLDER live chunk next to a newer moved row: migration 156
	// un-archived chunks into slots whose only live row had left the
	// lifecycle (its guard looked for a live 'chunk' holder). The live chunk
	// is the arbiter's row and stays, whatever the ages; the moved row is
	// archived. Ranking by age alone would keep the moved row and fail the
	// boot on uq_source_chunk.
	revSrc := m169Source(t, pool, "/m169/revived.md")
	revOld := m169Chunk(t, pool, revSrc, "m169 revived old title", "m169 revived v1")
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, revOld); err != nil {
		t.Fatalf("archive the older chunk: %v", err)
	}
	revNew, err := m169Ingest(pool, revSrc, "m169 revived new title", "m169 revived v2")
	if err != nil || revNew == revOld {
		t.Fatalf("newer row in the slot: id=%s err=%v", revNew, err)
	}
	m169Damage(t, pool, revNew, "snapshot", pointer.ID)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = false WHERE id = $1::uuid`, revOld); err != nil {
		t.Fatalf("un-archive the older chunk (as 156 did): %v", err)
	}
	revOldBefore := m169Read(t, pool, revOld)

	// (5) scope-moved rows: a manage/MCP update can move any block, chunks
	// included, into another scope (direct UPDATE here — the effect of
	// store.UpdateBlock's scope arm). The ingest only writes in the source's
	// scope, so a slot with a live row outside it is left exactly as it is —
	// a moved row alone in its slot (5a) and a moved row next to the slot's
	// live chunk (5b), which would otherwise be archived in a foreign scope.
	aloneSrc := m169Source(t, pool, "/m169/moved-alone.md")
	movedAlone := m169Chunk(t, pool, aloneSrc, "m169 moved alone", "m169 moved alone content")
	m169Damage(t, pool, movedAlone, "snapshot", pointer.ID)
	mixSrc := m169Source(t, pool, "/m169/moved-mix.md")
	movedMix := m169Chunk(t, pool, mixSrc, "m169 moved mix old", "m169 moved mix v1")
	m169Damage(t, pool, movedMix, "knowledge", "")
	mixLive, err := m169Ingest(pool, mixSrc, "m169 moved mix new", "m169 moved mix v2")
	if err != nil || mixLive == movedMix {
		t.Fatalf("live chunk next to the moved row: id=%s err=%v", mixLive, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET scope = 'hth' WHERE id = ANY($1::uuid[])`,
		[]string{movedAlone, movedMix}); err != nil {
		t.Fatalf("move rows to another scope: %v", err)
	}
	scopeMovedBefore := map[string]m169State{
		movedAlone: m169Read(t, pool, movedAlone),
		movedMix:   m169Read(t, pool, movedMix),
		mixLive:    m169Read(t, pool, mixLive),
	}

	// Controls the repair must not touch.
	healthySrc := m169Source(t, pool, "/m169/healthy.md")
	healthy := m169Chunk(t, pool, healthySrc, "m169 healthy", "m169 healthy content")
	healthyBefore := m169Read(t, pool, healthy)

	blockSnapshot := pointer.ID // a block-mode row (no source) that is a real snapshot
	other, err := store.UpsertBlock(ctx, pool, "decisions", "m169 other superseder", "m169 other",
		nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed other block: %v", err)
	}
	m169Damage(t, pool, blockSnapshot, "snapshot", other.ID)

	archivedSrc := m169Source(t, pool, "/m169/archived.md")
	archived := m169Chunk(t, pool, archivedSrc, "m169 archived", "m169 archived content")
	m169Damage(t, pool, archived, "snapshot", pointer.ID)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, archived); err != nil {
		t.Fatalf("archive control: %v", err)
	}

	orphanSrc := m169Source(t, pool, "/m169/orphan.md")
	orphan := m169Chunk(t, pool, orphanSrc, "m169 orphan", "m169 orphan content")
	m169Damage(t, pool, orphan, "knowledge", "")
	if _, err := pool.Exec(ctx, `DELETE FROM context_sources WHERE id = $1::uuid`, orphanSrc); err != nil {
		t.Fatalf("delete orphan source: %v", err) // ON DELETE SET NULL → no slot any more
	}

	// The seeded damage reproduces the issue before the repair.
	if _, err := m169Ingest(pool, singles[0].source, "m169 single snapshot", "m169 edited"); !m169Is23505(err) {
		t.Fatalf("pre-repair changed-content ingest into a damaged slot: err=%v, want 23505", err)
	}
	if _, err := m169Ingest(pool, takenSrc, "m169 taken old title", "m169 taken v3"); !m169Is23505(err) {
		t.Fatalf("pre-repair ingest of the old title into a taken slot: err=%v, want 23505", err)
	}
	restoredBefore := m169Read(t, pool, singles[0].id)
	archivedBefore := m169Read(t, pool, takenOld)

	m169Apply(t, pool)

	// Neither statement changes content, so neither bumps updated_at.
	if got := m169Read(t, pool, singles[0].id); !got.updatedAt.Equal(restoredBefore.updatedAt) {
		t.Errorf("restored row: updated_at %v, was %v", got.updatedAt, restoredBefore.updatedAt)
	}
	if got := m169Read(t, pool, takenOld); !got.updatedAt.Equal(archivedBefore.updatedAt) {
		t.Errorf("archived row: updated_at %v, was %v", got.updatedAt, archivedBefore.updatedAt)
	}
	for id, before := range scopeMovedBefore {
		if got := m169Read(t, pool, id); got != before {
			t.Errorf("slot with a scope-moved row touched, row %s: %+v, was %+v", id, got, before)
		}
	}

	for _, c := range singles {
		got := m169Read(t, pool, c.id)
		if got.lifecycle != "chunk" || got.pointer != "" || got.archived {
			t.Errorf("single %s: lifecycle=%q pointer=%q archived=%v, want chunk, NULL, live", c.lifecycle, got.lifecycle, got.pointer, got.archived)
		}
		if !strings.Contains(got.repair, `"lifecycle_before": "`+c.lifecycle+`"`) || !strings.Contains(got.repair, `"archived": false`) {
			t.Errorf("single %s: repair record %s", c.lifecycle, got.repair)
		}
		if c.pointer != "" && !strings.Contains(got.repair, c.pointer) {
			t.Errorf("single %s: repair record %s does not keep the old pointer %s", c.lifecycle, got.repair, c.pointer)
		}
	}

	if got := m169Read(t, pool, takenOld); got.lifecycle != "chunk" || !got.archived || !strings.Contains(got.repair, `"archived": true`) {
		t.Errorf("taken slot, stale row: %+v, want chunk, archived, repair record", got)
	}
	if got := m169Read(t, pool, takenLive); got != takenLiveBefore {
		t.Errorf("taken slot, live chunk changed: %+v, was %+v", got, takenLiveBefore)
	}
	if got := m169Read(t, pool, twoNew); got.lifecycle != "chunk" || got.archived || got.pointer != "" {
		t.Errorf("two damaged, newest: %+v, want chunk, live, NULL pointer", got)
	}
	if got := m169Read(t, pool, twoOld); got.lifecycle != "chunk" || !got.archived {
		t.Errorf("two damaged, older: %+v, want chunk, archived", got)
	}
	if got := m169Read(t, pool, revOld); got != revOldBefore {
		t.Errorf("older live chunk changed: %+v, was %+v", got, revOldBefore)
	}
	if got := m169Read(t, pool, revNew); got.lifecycle != "chunk" || !got.archived {
		t.Errorf("newer moved row next to a live chunk: %+v, want chunk, archived", got)
	}

	if got := m169Read(t, pool, healthy); got != healthyBefore {
		t.Errorf("healthy chunk changed: %+v, was %+v", got, healthyBefore)
	}
	if got := m169Read(t, pool, blockSnapshot); got.lifecycle != "snapshot" || got.pointer != other.ID || got.repair != "" {
		t.Errorf("block-mode snapshot touched: %+v", got)
	}
	if got := m169Read(t, pool, archived); got.lifecycle != "snapshot" || !got.archived || got.repair != "" {
		t.Errorf("archived damaged row touched: %+v", got)
	}
	if got := m169Read(t, pool, orphan); got.lifecycle != "knowledge" || got.repair != "" {
		t.Errorf("row without a source touched: %+v", got)
	}

	// Every repaired slot takes a changed-content ingest again, on its row.
	for _, c := range singles {
		id, err := m169Ingest(pool, c.source, "m169 single "+c.lifecycle, "m169 edited "+c.lifecycle)
		if err != nil || id != c.id {
			t.Errorf("post-repair ingest, single %s: id=%s err=%v, want %s", c.lifecycle, id, err, c.id)
		}
	}
	if id, err := m169Ingest(pool, takenSrc, "m169 taken old title", "m169 taken v3"); err != nil || id != takenLive {
		t.Errorf("post-repair ingest of the old title into the taken slot: id=%s err=%v, want %s", id, err, takenLive)
	}
	if id, err := m169Ingest(pool, twoSrc, "m169 two first title", "m169 two v3"); err != nil || id != twoNew {
		t.Errorf("post-repair ingest into the two-row slot: id=%s err=%v, want %s", id, err, twoNew)
	}
	if id, err := m169Ingest(pool, revSrc, "m169 revived new title", "m169 revived v3"); err != nil || id != revOld {
		t.Errorf("post-repair ingest of the moved row's title into the revived slot: id=%s err=%v, want %s", id, err, revOld)
	}

	// Idempotent: a second application finds nothing.
	before := m169Read(t, pool, twoOld)
	m169Apply(t, pool)
	if got := m169Read(t, pool, twoOld); got != before {
		t.Errorf("second application changed %s: %+v, was %+v", twoOld, got, before)
	}
	for id, before := range scopeMovedBefore {
		if got := m169Read(t, pool, id); got != before {
			t.Errorf("second application touched the scope-moved slot, row %s: %+v, was %+v", id, got, before)
		}
	}
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM context_blocks
		  WHERE source_id IS NOT NULL AND chunk_index IS NOT NULL
		    AND NOT is_archived AND lifecycle_state <> 'chunk'
		  ORDER BY id`)
	if err != nil {
		t.Fatalf("list moved rows: %v", err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list moved rows: %v", err)
	}
	want := []string{movedAlone, movedMix}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Errorf("live rows with a chunk slot outside the chunk lifecycle after the repair: %v, want only the scope-moved %v", left, want)
	}
}
