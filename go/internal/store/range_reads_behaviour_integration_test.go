//go:build integration

package store_test

// Behaviour pins of issue #536 (first half): what the guard list answers for
// a guard_matched_id or a guard_similarity written before issue #238, the one
// instant of an embed-backfill release, and the UTC horizon of the write-log
// retention.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestRangeReads(GuardList|Release|Retention)' -count=1 -v

import (
	"context"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Berlin for the retention's daylight-saving case

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRangeReadsGuardListReadsTheKeyAsAUUID: the guard list reads
// guard_matched_id as the raw read paths read it (guardMatchedIDSQL): any
// spelling of a uuid PostgreSQL accepts names its block, and the list
// answers that block's canonical id and title when the reader may read it,
// no id and the redaction mark when not. Before, the list compared the
// block's id text with the key, so another spelling — a database written
// before #238 may hold one — named no block on the list while get and search
// treated it as naming one. The flagged blocks come from the guard; their
// keys are then rewritten as a caller before #238 could write them.
func TestRangeReadsGuardListReadsTheKeyAsAUUID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope, other = "gl536", "gl536-other"
	ids := gkFlaggedPairs(t, pool, scope, "gl536", 3)
	readable, err := store.UpsertBlock(ctx, pool, "learnings", "gl536 partner", "the readable partner", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := store.UpsertBlock(ctx, pool, "learnings", "gl536 hidden partner", "the partner in another scope", nil, nil, other, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		matched *string
		title   *string
	}
	str := func(s string) *string { return &s }
	cases := []struct {
		key  string
		want want
	}{
		{strings.ToUpper(readable.ID), want{str(readable.ID), str("gl536 partner")}},
		{"{" + readable.ID + "}", want{str(readable.ID), str("gl536 partner")}},
		{strings.ReplaceAll(readable.ID, "-", ""), want{str(readable.ID), str("gl536 partner")}},
		{strings.ToUpper(hidden.ID), want{nil, str("[redacted]")}},
		{"not-a-uuid", want{nil, nil}},
		{readable.ID, want{str(readable.ID), str("gl536 partner")}},
	}
	for i, c := range cases {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = jsonb_set(metadata, '{guard_matched_id}', to_jsonb($2::text)) WHERE id = $1::uuid`, ids[i], c.key); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.GuardList(ctx, pool, []string{scope}, "", "", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.GuardListItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	opt := func(s *string) string {
		if s == nil {
			return "<nil>"
		}
		return *s
	}
	for i, c := range cases {
		it, ok := byID[ids[i]]
		if !ok {
			t.Fatalf("flagged block %s not listed", ids[i])
		}
		if opt(it.MatchedID) != opt(c.want.matched) || opt(it.MatchedTitle) != opt(c.want.title) {
			t.Errorf("key %q: the list answers matched_id %s, matched_title %s; want %s, %s",
				c.key, opt(it.MatchedID), opt(it.MatchedTitle), opt(c.want.matched), opt(c.want.title))
		}
		// The raw read path reads the key the same way: it keeps the key
		// exactly where the list names the partner.
		b, err := store.GetBlock(ctx, pool, nil, ids[i], []string{scope}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, kept := b.Metadata["guard_matched_id"]
		if kept != (it.MatchedID != nil) && c.key != "not-a-uuid" {
			t.Errorf("key %q: get keeps the key: %v, the list names the partner: %v — the two readers disagree", c.key, kept, it.MatchedID != nil)
		}
	}
}

// TestRangeReadsGuardListSortsOnlyJSONNumbers: the list sorts by a
// guard_similarity that is a JSON number, the type the guard writes, and
// sorts every other value last like a missing one. Before, a JSON string that
// PostgreSQL reads as a numeric — "NaN", "Infinity", "1e999", "1_000",
// "0x1F" — sorted above every similarity the guard wrote.
func TestRangeReadsGuardListSortsOnlyJSONNumbers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "gs536"
	ids := gkFlaggedPairs(t, pool, scope, "gs536", 4)
	values := []string{`0.97`, `"NaN"`, `"Infinity"`, `"1e999"`, `0.5`, `"1_000"`, `"0x1F"`, `"0.99"`}
	for i, v := range values {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = jsonb_set(metadata, '{guard_similarity}', $2::jsonb) WHERE id = $1::uuid`, ids[i], v); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.GuardList(ctx, pool, []string{scope}, "", "", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(ids) {
		t.Fatalf("%d items, want %d", len(items), len(ids))
	}
	if items[0].ID != ids[0] || items[1].ID != ids[4] {
		got := make([]string, len(items))
		for i, it := range items {
			got[i] = it.ID
		}
		t.Errorf("the list starts with %s, %s; want the two JSON numbers first (0.97 %s, 0.5 %s)\n order %v", items[0].ID, items[1].ID, ids[0], ids[4], got)
	}
}

// TestRangeReadsReleaseDecidesAtOneInstant: a confirmed backfill release
// counts and releases at one instant. A tenant reader's oversize memo whose
// hold ends between the count and the page was counted as held and then
// released by the page — released above the count it confirmed. The page is
// held back here until that memo's hold has ended.
func TestRangeReadsReleaseDecidesAtOneInstant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, "rel536", "rel536")
	if err != nil {
		t.Fatal(err)
	}
	const scope = "rel536:data"
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 2)
	for i, title := range []string{"rel536 free", "rel536 held"} {
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			b, err := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: title, Content: "body"}, []string{scope})
			if err == nil {
				ids[i] = b.ID
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, ids[i], store.EmbedFailureOversize,
			store.NormalizeEmbedError(store.EmbedFailureOversize, store.OversizeEstimateMessage(9, 1)), time.Minute, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	// The second memo was released once and parked again: its hold (Hold for
	// two attempts) ends three seconds from now.
	const hold = 10 * time.Second
	if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET attempts = 2, first_seen = now() - make_interval(secs => $2)
		WHERE block_id = $1::uuid`, ids[1], (hold - 3*time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	rel := store.BackfillRelease{Class: store.EmbedFailureOversize, TenantID: tn.ID, Hold: hold}
	dry, err := store.ReleaseBackfillMemos(ctx, pool, rel)
	if err != nil || dry.Counted != 1 || dry.Held != 1 {
		t.Fatalf("dry run: %+v %v, want counted 1, held 1", dry, err)
	}
	// The release's page waits until the held memo's hold has ended.
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = &relPageDelay{until: time.Now().Add(5 * time.Second)}
	delayed, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer delayed.Close()
	confirmed := rel
	confirmed.Confirm, confirmed.AsOf = &dry.Counted, &dry.AsOf
	res, err := store.ReleaseBackfillMemos(ctx, delayed, confirmed)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Counted != 1 || res.Released != 1 {
		t.Errorf("release: %+v, want counted 1, released 1: the page released a memo the count held", res)
	}
	var due int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND next_attempt_at <= now()`, ids[1]).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != 0 {
		t.Errorf("the held memo was made due by a release that counted it as held")
	}
}

// relPageDelay holds a release's page statement until a time.
type relPageDelay struct{ until time.Time }

func (d *relPageDelay) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.Contains(q.SQL, "WITH page AS") {
		time.Sleep(time.Until(d.until))
	}
	return ctx
}

func (d *relPageDelay) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestRangeReadsRetentionHorizonInUTC: the write-log retention's horizon is
// retention_days days of 24 hours before the database's clock, whatever the
// session's time zone — the horizon the backfill checks against (handler
// pastWriteLogHorizon, UTC). Before, timestamptz minus an interval of days
// stepped over the session's calendar and was an hour off across a change of
// daylight saving time.
func TestRangeReadsRetentionHorizonInUTC(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	base := testdb.SetupTestDB(t)
	pool := testdb.WithSessionSettings(t, base, map[string]string{"TimeZone": "Europe/Berlin"})
	ctx := context.Background()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	// A retention whose horizon lies on the other side of a daylight-saving
	// change than today.
	now := time.Now()
	_, off := now.In(berlin).Zone()
	days := 0
	for d := 1; d <= 366; d++ {
		if _, o := now.Add(-time.Duration(d) * 24 * time.Hour).In(berlin).Zone(); o != off {
			days = d
			break
		}
	}
	if days == 0 {
		t.Fatal("no daylight-saving change within a year")
	}
	var before, after time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EvictWriteLog(ctx, pool, days); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	boundary, err := store.WriteLogEvictedBefore(ctx, pool)
	if err != nil || boundary == nil {
		t.Fatalf("boundary: %v %v", boundary, err)
	}
	span := time.Duration(days) * 24 * time.Hour
	if boundary.Before(before.Add(-span)) || boundary.After(after.Add(-span)) {
		t.Errorf("retention of %d days in Europe/Berlin: horizon %s, want %d × 24 h before the run (%s … %s)",
			days, boundary.UTC().Format(time.RFC3339Nano), days, before.Add(-span).UTC().Format(time.RFC3339Nano), after.Add(-span).UTC().Format(time.RFC3339Nano))
	}
}
