//go:build integration

// W8 DB gates — retention for dead topics (design/01 §7 W8, §4.8, §6.7).
//
// The gates that matter are not "does it delete": that is one predicate. They
// are the three properties the purge was moved OUT of the persist transaction
// for — it holds no advisory lock, it works in bounded batches, and it never
// tears a lineage chain.
package overview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

const w8Retention = 90 * 24 * time.Hour

// w8Grave inserts one retired topic that died `age` ago and returns its id.
func w8Grave(t *testing.T, pool *pgxpool.Pool, scope string, age time.Duration) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO graph_cluster_topic (scope, created_at, last_seen_at, retired_at)
		VALUES ($1, now() - make_interval(secs => $2), now() - make_interval(secs => $2),
		        now() - make_interval(secs => $2))
		RETURNING topic_id::text`, scope, age.Seconds()).Scan(&id); err != nil {
		t.Fatalf("w8Grave(%s): %v", scope, err)
	}
	return id
}

// w8Living inserts one living topic, optionally long-lived.
func w8Living(t *testing.T, pool *pgxpool.Pool, scope string, age time.Duration) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO graph_cluster_topic (scope, created_at, last_seen_at)
		VALUES ($1, now() - make_interval(secs => $2), now() - make_interval(secs => $2))
		RETURNING topic_id::text`, scope, age.Seconds()).Scan(&id); err != nil {
		t.Fatalf("w8Living(%s): %v", scope, err)
	}
	return id
}

func w8Exists(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM graph_cluster_topic WHERE topic_id = $1::uuid`, id).Scan(&n); err != nil {
		t.Fatalf("w8Exists: %v", err)
	}
	return n > 0
}

// ─────────────────────────────────────────────────────────────────────────────

func TestW8TombstoneRetention(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// ── G1: the horizon. Older than the window goes, younger stays.
	t.Run("G1 the retention horizon", func(t *testing.T) {
		const scope = "w8g1"
		old := w8Grave(t, pool, scope, 100*24*time.Hour)
		young := w8Grave(t, pool, scope, 80*24*time.Hour)

		n, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if n != 1 {
			t.Fatalf("purged %d, want 1", n)
		}
		if w8Exists(t, pool, old) {
			t.Fatal("the 100-day-old grave survived the 90-day horizon")
		}
		if !w8Exists(t, pool, young) {
			t.Fatal("the 80-day-old grave was purged — the horizon is inverted")
		}
	})

	// RED PROBE for G1: the inverted comparison purges exactly the graves the
	// W3 re-attach still needs and keeps the ones nobody will ever read again.
	t.Run("G1 red probe — > instead of < purges the wrong half", func(t *testing.T) {
		const scope = "w8g1red"
		old := w8Grave(t, pool, scope, 100*24*time.Hour)
		young := w8Grave(t, pool, scope, 10*24*time.Hour)

		restore := purgeTemplate
		purgeTemplate = strings.Replace(purgeTemplate,
			"t.retired_at < now() - make_interval(secs => $1)",
			"t.retired_at > now() - make_interval(secs => $1)", 1)
		defer func() { purgeTemplate = restore }()
		if purgeTemplate == restore {
			t.Fatal("red probe did not patch the comparison")
		}

		if _, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if !w8Exists(t, pool, old) || w8Exists(t, pool, young) {
			t.Fatal("red probe reproduced nothing — the comparison is not what selects the doomed rows")
		}
	})

	// ── G2: a lineage chain shortens, it never tears. ON DELETE SET NULL is
	// what makes "died, destination unknown" the degraded state instead of a
	// dangling reference.
	t.Run("G2 lineage survives the purge as NULL", func(t *testing.T) {
		const scope = "w8g2"
		absorbed := w8Grave(t, pool, scope, 100*24*time.Hour)
		survivor := w8Living(t, pool, scope, time.Hour)
		if _, err := pool.Exec(ctx,
			`UPDATE graph_cluster_topic SET origin_topic_id = $2::uuid WHERE topic_id = $1::uuid`,
			survivor, absorbed); err != nil {
			t.Fatalf("wire the lineage: %v", err)
		}

		if _, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention); err != nil {
			t.Fatalf("purge: %v", err)
		}
		var origin *string
		if err := pool.QueryRow(ctx,
			`SELECT origin_topic_id::text FROM graph_cluster_topic WHERE topic_id = $1::uuid`, survivor).
			Scan(&origin); err != nil {
			t.Fatalf("read survivor: %v", err)
		}
		if origin != nil {
			t.Fatalf("origin_topic_id = %s, want NULL", *origin)
		}
	})

	// RED PROBE for G2: without ON DELETE SET NULL the same purge breaks with
	// 23503 and the whole batch rolls back — the retention would stall on the
	// first referenced grave and never make progress again.
	t.Run("G2 red probe — a plain FK turns the purge into 23503", func(t *testing.T) {
		const scope = "w8g2red"
		absorbed := w8Grave(t, pool, scope, 100*24*time.Hour)
		survivor := w8Living(t, pool, scope, time.Hour)

		if _, err := pool.Exec(ctx, `
			ALTER TABLE graph_cluster_topic DROP CONSTRAINT graph_cluster_topic_origin_topic_id_fkey,
			  ADD CONSTRAINT graph_cluster_topic_origin_topic_id_fkey
			      FOREIGN KEY (origin_topic_id) REFERENCES graph_cluster_topic(topic_id)`); err != nil {
			t.Fatalf("re-declare the FK without the action: %v", err)
		}
		defer func() {
			if _, err := pool.Exec(ctx, `
				ALTER TABLE graph_cluster_topic DROP CONSTRAINT graph_cluster_topic_origin_topic_id_fkey,
				  ADD CONSTRAINT graph_cluster_topic_origin_topic_id_fkey
				      FOREIGN KEY (origin_topic_id) REFERENCES graph_cluster_topic(topic_id) ON DELETE SET NULL`); err != nil {
				t.Fatalf("restore the FK: %v", err)
			}
		}()
		if _, err := pool.Exec(ctx,
			`UPDATE graph_cluster_topic SET origin_topic_id = $2::uuid WHERE topic_id = $1::uuid`,
			survivor, absorbed); err != nil {
			t.Fatalf("wire the lineage: %v", err)
		}

		_, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("err = %v, want SQLSTATE 23503 — the action is what keeps the chain intact", err)
		}
	})

	// ── G3: 0 is a documented operating state, not a missing value.
	t.Run("G3 retention 0 never deletes", func(t *testing.T) {
		const scope = "w8g3"
		ancient := w8Grave(t, pool, scope, 3650*24*time.Hour)
		n, err := PurgeTombstones(ctx, pool, []string{scope}, 0)
		if err != nil || n != 0 {
			t.Fatalf("purged %d (err %v), want 0", n, err)
		}
		if !w8Exists(t, pool, ancient) {
			t.Fatal("a ten-year-old grave was deleted with retention 0")
		}
	})

	// ── G4: the living are untouchable, however old.
	t.Run("G4 a living topic is never purged", func(t *testing.T) {
		const scope = "w8g4"
		veteran := w8Living(t, pool, scope, 200*24*time.Hour)
		if _, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if !w8Exists(t, pool, veteran) {
			t.Fatal("a 200-day-old LIVING topic was purged — the predicate reads created_at, not retired_at")
		}
	})

	// ── A grave a node row still points at stays, whatever its age. The
	// NOT EXISTS clause is redundancy against the foreign key; this pins that
	// the redundancy agrees with it.
	t.Run("a referenced grave is not purged", func(t *testing.T) {
		const scope = "w8ref"
		grave := w8Grave(t, pool, scope, 200*24*time.Hour)
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id,
			                                repr_title, repr_quality, topic_id)
			VALUES (gen_random_uuid(), $1, 1, '{"learnings":1}'::jsonb, gen_random_uuid(), 'r', 1.0, $2::uuid)`,
			scope, grave); err != nil {
			t.Fatalf("node row: %v", err)
		}
		if _, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if !w8Exists(t, pool, grave) {
			t.Fatal("a grave with a live node row was purged")
		}
	})

	// ── G6: the global run. The rebuild has two shapes and so must the purge —
	// the scope-only variant would have had no counterpart for the global pass.
	t.Run("G6 the global run purges without a scope predicate", func(t *testing.T) {
		a := w8Grave(t, pool, "w8globA", 100*24*time.Hour)
		b := w8Grave(t, pool, "w8globB", 100*24*time.Hour)
		if _, err := PurgeTombstones(ctx, pool, nil, w8Retention); err != nil {
			t.Fatalf("global purge: %v", err)
		}
		if w8Exists(t, pool, a) || w8Exists(t, pool, b) {
			t.Fatal("the global run left graves behind in one of the partitions")
		}
	})

	// ── The scoped run is a PARTITION run: a foreign partition's graves are
	// none of its business.
	t.Run("the scoped run leaves foreign partitions alone", func(t *testing.T) {
		mine := w8Grave(t, pool, "w8mine", 100*24*time.Hour)
		theirs := w8Grave(t, pool, "w8theirs", 100*24*time.Hour)
		if _, err := PurgeTombstones(ctx, pool, []string{"w8mine"}, w8Retention); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if w8Exists(t, pool, mine) {
			t.Fatal("the own grave survived")
		}
		if !w8Exists(t, pool, theirs) {
			t.Fatal("a foreign partition's grave was purged")
		}
	})

	// ── The FK companion indexes of migration 124 are what keep the purge
	// linear. Without them every deleted row forces the ON DELETE SET NULL
	// trigger into a sequential scan over the same table.
	t.Run("the FK companion indexes exist", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*)::int FROM pg_indexes
			 WHERE tablename = 'graph_cluster_topic'
			   AND indexname IN ('idx_gct_origin','idx_gct_merged_into')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("found %d of the 2 FK companion indexes — the purge would be quadratic", n)
		}
	})
}

// G5 — the switch-on case. tombstone_retention is legitimately 0 for months;
// turning it on then makes the first purge a six-figure DELETE. Two properties
// have to hold, and they are the reason the purge left the persist transaction:
// it works in BOUNDED batches, and it holds NO advisory lock.
func TestW8PurgeIsBatchedAndLockFree(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "w8bulk"
	// The design's switch-on size, not a token amount: tombstone_retention is
	// legitimately 0 for months, and turning it on then makes the FIRST purge a
	// six-figure DELETE. That is the run this gate has to cover.
	//
	// MEASURED 2026-08-05, same host, same image, testcontainers PG18:
	//   200.000 graves purged in 4,85 s over 40 rounds of 5.000 — the whole time
	//   under a HELD persist advisory lock on a separate session. The lock is the
	//   sharper half of the assertion: the Revision-1 shape, which ran the purge
	//   inside the persist transaction, could not have made a single row of
	//   progress here.
	const graves = 200000

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_cluster_topic (scope, created_at, last_seen_at, retired_at)
		SELECT $1, now() - interval '200 days', now() - interval '200 days', now() - interval '200 days'
		  FROM generate_series(1, $2)`, scope, graves); err != nil {
		t.Fatalf("seed graves: %v", err)
	}

	// Hold the rebuild's advisory lock on a SEPARATE session for the whole
	// purge. If the purge took it — the Revision-1 shape, which ran inside the
	// persist transaction — it could not make a single row of progress here.
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder conn: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKeyForScopes([]string{scope})); err != nil {
		t.Fatalf("hold the persist lock: %v", err)
	}
	defer func() {
		if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`,
			lockKeyForScopes([]string{scope})); err != nil {
			t.Fatalf("release the lock: %v", err)
		}
	}()

	deadline, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	n, err := PurgeTombstones(deadline, pool, []string{scope}, w8Retention)
	if err != nil {
		t.Fatalf("purge under a held persist lock: %v", err)
	}
	if n != graves {
		t.Fatalf("purged %d of %d — the batch loop did not terminate on the full set", n, graves)
	}

	var left int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM graph_cluster_topic WHERE scope = $1`, scope).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d graves left behind", left)
	}

	// And the batch size is a real bound, not a comment: one round deletes at
	// most purgeBatch rows.
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_cluster_topic (scope, created_at, last_seen_at, retired_at)
		SELECT $1, now() - interval '200 days', now() - interval '200 days', now() - interval '200 days'
		  FROM generate_series(1, $2)`, scope, purgeBatch+10); err != nil {
		t.Fatalf("seed second wave: %v", err)
	}
	tag, err := pool.Exec(ctx, fmt.Sprintf(purgeTemplate, "\n       AND t.scope = ANY($2)"),
		w8Retention.Seconds(), []string{scope})
	if err != nil {
		t.Fatalf("single batch: %v", err)
	}
	if tag.RowsAffected() != purgeBatch {
		t.Fatalf("one batch deleted %d rows, want the %d cap", tag.RowsAffected(), purgeBatch)
	}
	if _, err := PurgeTombstones(ctx, pool, []string{scope}, w8Retention); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// The purge must not need a transaction of its own to be interruptible: a
// cancelled context ends it between batches, with everything already deleted
// staying deleted.
func TestW8PurgeHonoursCancellation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := PurgeTombstones(ctx, pool, []string{"w8cancel"}, w8Retention)
	if err == nil {
		t.Fatalf("purged %d without an error on a dead context", n)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// compile-time guard: the purge takes a pool, never a transaction — a tx
// parameter would be exactly the coupling this wave removed.
var _ = func(pool *pgxpool.Pool, _ pgx.Tx) {
	_, _ = PurgeTombstones(context.Background(), pool, nil, time.Hour)
}
