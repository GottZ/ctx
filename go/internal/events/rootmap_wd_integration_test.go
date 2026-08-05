//go:build integration

// Wave W-D scheduler gates (Cluster-Topic-Map, design/02 §7 "W-D"): the map is
// written from the rebuild path, it reports the cap of the exit it was written
// after, and — the mandated double probe — it NEVER replaces a good map with an
// empty or stale one (BP-6), including against the two states that exist live:
// a partition that was built and is empty (`scope='work'`), and a meta row that
// outlived its partition.
//
//	go test -tags=integration ./internal/events/ -run TestRootMap -count=1 -v
package events

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// wdConfig enables the rebuild AND the map with the shipped defaults. Anything
// left at the zero value would be a different artefact than production writes.
func wdConfig() *config.Config {
	c := &config.Config{}
	c.GraphOverview.Enabled = true
	c.GraphOverview.Resolution = 1.0
	c.GraphOverview.RebuildTimeout = time.Minute
	c.GraphOverview.RebuildInterval = 6 * time.Hour
	c.Scheduler.HomeScope = "private"
	c.Scheduler.ReadScopes = []string{"private"}
	c.RootMap.Enabled = true
	c.RootMap.BudgetBytes = 15360
	c.RootMap.FooterReserveBytes = 512
	c.RootMap.SmallClusterMax = 2
	c.RootMap.CountTimeout = 5 * time.Second
	return c
}

type mapRow struct {
	id          string
	content     string
	typeName    string
	sensitivity string
	category    string
	metadata    map[string]any
	updatedAt   time.Time
}

// readMap returns the root map of a scope, or ok=false when it does not exist.
func readMap(t *testing.T, pool *pgxpool.Pool, scope string) (mapRow, bool) {
	t.Helper()
	var r mapRow
	err := pool.QueryRow(context.Background(), `
		SELECT id::text, content, COALESCE(type_name, ''), COALESCE(sensitivity, ''), category, metadata, updated_at
		  FROM context_blocks
		 WHERE category = 'index' AND title = $1 AND scope = $2 AND NOT is_archived`,
		"root-map-"+scope, scope).Scan(
		&r.id, &r.content, &r.typeName, &r.sensitivity, &r.category, &r.metadata, &r.updatedAt)
	if err != nil {
		return mapRow{}, false
	}
	return r, true
}

// wdLink joins two blocks so Louvain sees a community instead of two singletons —
// the collector-line threshold (2) means only a cluster of THREE becomes a topic
// line, which is what makes the byte comparison of the idempotency gate
// meaningful.
func wdLink(t *testing.T, pool *pgxpool.Pool, src, dst string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'private')`, src, dst); err != nil {
		t.Fatalf("link %s->%s: %v", src, dst, err)
	}
}

func wdID(n int) string {
	return "019d0000-0000-7000-9000-0000000d" + string(rune('0'+n/10)) + string(rune('0'+n%10)) + "00"
}

// seedTriangle plants a three-block community in `scope` — big enough to be a
// TOPIC line rather than a collector-line entry.
func seedTriangle(t *testing.T, pool *pgxpool.Pool, scope string, base int) []string {
	t.Helper()
	ids := []string{wdID(base), wdID(base + 1), wdID(base + 2)}
	for i, id := range ids {
		stampBlock(t, pool, id, scope, "wd-"+scope+"-"+string(rune('a'+i)))
	}
	wdLink(t, pool, ids[0], ids[1])
	wdLink(t, pool, ids[1], ids[2])
	wdLink(t, pool, ids[0], ids[2])
	return ids
}

// TestRootMapWrittenFromEveryExit is the wiring gate: the map exists after a
// SUCCESSFUL rebuild and, after a skipped one, it says so in its own text.
//
// RED against HEAD: rebuildOverviewOnce ends with its success log — no map is
// written at all, from any exit. RED against a version wired into the success
// branch only: the second half fails, because the exit that produces no
// partition is precisely the one whose cap the map has to report.
func TestRootMapWrittenFromEveryExit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}
	seedTriangle(t, pool, "private", 10)

	s := stampScheduler(t, pool, wdConfig())
	s.rebuildOverviewOnce(ctx, bt)

	m, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("no root map after a successful rebuild")
	}
	if !strings.HasPrefix(m.content, "ctx Root Map v1 | scope:private") {
		t.Fatalf("map does not start with its head line:\n%s", m.content)
	}
	if strings.Contains(m.content, "!! Partition eingefroren") {
		t.Error("a map written after a SUCCESSFUL rebuild carries a freeze line")
	}
	if !strings.Contains(m.content, "## Themen") {
		t.Errorf("map has no topic section:\n%s", m.content)
	}

	// Node-cap exit: the rebuild skips, the map must now say the partition is
	// frozen while still showing the last good cluster state.
	capped := wdConfig()
	capped.GraphOverview.MaxNodes = 1
	s2 := stampScheduler(t, pool, capped)
	s2.rebuildOverviewOnce(ctx, bt)

	m2, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("root map disappeared after a skipped rebuild")
	}
	if !strings.HasPrefix(m2.content, "!! Partition eingefroren") {
		t.Fatalf("node-cap skip did not put the freeze line FIRST (rule R2):\n%s", m2.content)
	}
	if !strings.Contains(m2.content, "node-cap") && !strings.Contains(m2.content, "Knoten-Cap") {
		t.Errorf("freeze line does not name the cap:\n%s", m2.content)
	}
	if !strings.Contains(m2.content, "## Themen") {
		t.Error("the frozen map dropped its topic lines — a cap makes a map OLD, not empty")
	}
}

// TestRootMapBlockShape pins what the map IS: an index block of type
// system-meta with sensitivity internal, inside its budget.
//
// The sensitivity half is E4-02: the pool default is `credentials`, which the
// three legacy map blocks carry and which seals them against every external
// backend. The map holds labels, counts and IDs — no block content — and the
// window to set that without a confirm-gated downgrade is while the block is
// NEW.
func TestRootMapBlockShape(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}
	seedTriangle(t, pool, "private", 20)

	s := stampScheduler(t, pool, wdConfig())
	s.rebuildOverviewOnce(ctx, bt)

	m, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("no root map written")
	}
	if m.typeName != "system-meta" {
		t.Errorf("type_name = %q, want system-meta — anything else is a retrieval candidate", m.typeName)
	}
	if m.sensitivity != "internal" {
		t.Errorf("sensitivity = %q, want internal (E4-02; the pool default credentials would seal the map)", m.sensitivity)
	}
	if m.category != "index" {
		t.Errorf("category = %q, want index (guard and dream sieve on it)", m.category)
	}
	if v, _ := m.metadata["is_meta"].(bool); !v {
		t.Errorf("metadata.is_meta = %v, want true — it is the classify input for system-meta", m.metadata["is_meta"])
	}
	if len(m.content) > 15360 {
		t.Errorf("map is %d B, over its own budget", len(m.content))
	}
	// The coverage section follows E11-02: checkpoints are a decision, not a gap.
	if !strings.Contains(m.content, "Deckung:") {
		t.Errorf("map has no coverage section:\n%s", m.content)
	}
}

// TestRootMapIdempotency is gate 2: an unchanged partition writes NOTHING.
//
// Every content-changing upsert invalidates the embedding and rewrites the TOAST
// pages; at the target scale a 6-hourly rewrite of an identical map is a
// recurring cost for zero information. The comparison is only decidable because
// no wall clock reaches the text (R3) — a `generated_at` line would make part
// (i) fail by construction.
//
// RED against a version without the content comparison: updated_at moves on the
// second run. RED against a renderer that prints the render time: same.
//
// The steady state starts at the SECOND run, and that is a property rather than
// a test convenience: the map's own creation adds a block to the corpus, so the
// raw corpus number it prints is one higher the next time it is rendered. The
// step happens exactly once — a content change does not move a count — and
// pretending otherwise would mean either hiding the raw number (which E11-02
// wants visible) or excluding the map from a corpus it demonstrably belongs to.
func TestRootMapIdempotency(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}
	ids := seedTriangle(t, pool, "private", 30)

	s := stampScheduler(t, pool, wdConfig())
	s.rebuildOverviewOnce(ctx, bt) // creates the map — and thereby the +1 above
	s.rebuildOverviewOnce(ctx, bt) // absorbs it; from here the corpus is still
	first, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("no root map after the first run")
	}

	// (i) identical partition ⇒ no write.
	s.rebuildOverviewOnce(ctx, bt)
	second, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("root map vanished on the second run")
	}
	if !second.updatedAt.Equal(first.updatedAt) {
		t.Errorf("unchanged partition still wrote: updated_at %v → %v", first.updatedAt, second.updatedAt)
	}
	if second.content != first.content {
		t.Errorf("two runs over the same partition rendered different bytes:\n--- 1 ---\n%s\n--- 2 ---\n%s",
			first.content, second.content)
	}

	// (ii) changed membership ⇒ the comparison must DISCRIMINATE, otherwise the
	// no-op above proves nothing.
	grown := wdID(33)
	stampBlock(t, pool, grown, "private", "wd-private-grown")
	wdLink(t, pool, ids[0], grown)
	wdLink(t, pool, ids[1], grown)
	s.rebuildOverviewOnce(ctx, bt)

	third, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("root map vanished after the corpus grew")
	}
	if third.content == first.content {
		t.Error("a grown cluster produced byte-identical text — the comparison does not discriminate")
	}
}

// TestRootMapBP6 is the mandated DOUBLE probe of the wave: an empty or stale
// window must never replace a good map — against both states that exist live.
//
//	(a) TRUNCATE window: a global rebuild empties the three cluster tables
//	    inside its transaction; a render landing there sees zero clusters.
//	(b) built-and-empty (`scope='work'` live): computed_at set, cluster_n 0.
//	(c) stale-only: a meta row that outlived its partition.
//	(d) the counter-case — a genuinely fresh tenant DOES get its "no clusters
//	    yet" map, without which the rule would just be "never write".
//
// RED against a rule keyed on `cluster_n == 0` alone: case (b) writes an empty
// map over the good one — that is why AllowsUpsert carries the computed_at term.
// RED against no rule at all: (a), (b) and (c) all overwrite.
func TestRootMapBP6(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}
	seedTriangle(t, pool, "private", 40)

	s := stampScheduler(t, pool, wdConfig())
	s.rebuildOverviewOnce(ctx, bt)
	good, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("no root map to protect")
	}

	// (a) The cluster tables are empty while the meta row still reports mass —
	// exactly what a render inside the TRUNCATE window sees. The rebuild itself
	// is not re-run: renderRootMap is called directly, which is the only way to
	// hold the window still.
	if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_member; DELETE FROM graph_cluster_node; DELETE FROM graph_cluster_edge`); err != nil {
		t.Fatalf("empty the cluster tables: %v", err)
	}
	s.renderRootMap(ctx, bt)
	afterTruncate, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("BP-6 (a): the map was DELETED")
	}
	if afterTruncate.content != good.content {
		t.Errorf("BP-6 (a): an empty window replaced the good map:\n%s", afterTruncate.content)
	}

	// (b) built-and-empty: the live state of scope='work' — a successful
	// rebuild, zero clusters. A rule without the computed_at term reads this as
	// "fresh tenant" and writes the empty map.
	if _, err := pool.Exec(ctx, `UPDATE graph_overview_meta SET cluster_n = 0, node_n = 0 WHERE scope = 'private'`); err != nil {
		t.Fatalf("built-and-empty fixture: %v", err)
	}
	s.renderRootMap(ctx, bt)
	afterEmpty, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("BP-6 (b): the map was DELETED")
	}
	if afterEmpty.content != good.content {
		t.Errorf("BP-6 (b): the built-and-empty partition replaced the good map:\n%s", afterEmpty.content)
	}

	// (c) stale-only: the meta row claims mass for a partition with no cluster
	// rows left — the shape the W-A teardown filter leaves behind when a scope
	// vanishes from the corpus.
	if _, err := pool.Exec(ctx, `UPDATE graph_overview_meta SET cluster_n = 7, node_n = 42 WHERE scope = 'private'`); err != nil {
		t.Fatalf("stale-only fixture: %v", err)
	}
	s.renderRootMap(ctx, bt)
	afterStale, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("BP-6 (c): the map was DELETED")
	}
	if afterStale.content != good.content {
		t.Errorf("BP-6 (c): a stale-only window replaced the good map:\n%s", afterStale.content)
	}

	// (d) the counter-case: a scope with neither clusters nor a meta row is a
	// genuinely fresh tenant and DOES get its honest empty map. Without this the
	// rule would be indistinguishable from "never write an empty map".
	fresh := backgroundTenant{scope: "shared", owned: []string{"shared"}}
	freshCfg := wdConfig()
	freshCfg.Scheduler.HomeScope = "shared"
	freshCfg.Scheduler.ReadScopes = []string{"shared"}
	sf := stampScheduler(t, pool, freshCfg)
	sf.renderRootMap(ctx, fresh)

	fm, ok := readMap(t, pool, "shared")
	if !ok {
		t.Fatal("BP-6 (d): a fresh tenant got no map at all — the rule degenerated to 'never write'")
	}
	if !strings.Contains(fm.content, "Noch keine Cluster gebaut") {
		t.Errorf("BP-6 (d): the fresh map does not say it is empty rather than incomplete:\n%s", fm.content)
	}
}

// TestRootMapScopeIsolation is BP-1/BP-4 on the artefact that outlives the
// request: a scope leak in a response is one byte on the wire, a scope leak in
// the map is a database row travelling through backups and exports.
//
// RED against a version that drops the scope filter anywhere in the read chain:
// tenant A's map carries B's representative IDs and B's cluster count.
func TestRootMapScopeIsolation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedTriangle(t, pool, "private", 50)
	foreign := seedTriangle(t, pool, "work", 60)

	cfgA := wdConfig()
	sA := stampScheduler(t, pool, cfgA)
	sA.rebuildOverviewOnce(ctx, backgroundTenant{scope: "private", owned: []string{"private"}})

	cfgB := wdConfig()
	cfgB.Scheduler.HomeScope = "work"
	cfgB.Scheduler.ReadScopes = []string{"work"}
	sB := stampScheduler(t, pool, cfgB)
	sB.rebuildOverviewOnce(ctx, backgroundTenant{scope: "work", owned: []string{"work"}})

	a, ok := readMap(t, pool, "private")
	if !ok {
		t.Fatal("no private root map")
	}
	for _, id := range foreign {
		if strings.Contains(a.content, id) {
			t.Errorf("private map contains the foreign block id %s", id)
		}
	}
	if strings.Contains(a.content, "scopes=work") || strings.Contains(a.content, ",work") {
		t.Errorf("private map names a foreign scope:\n%s", a.content)
	}
	if !strings.Contains(a.content, "1/1 Themen") {
		t.Errorf("private map does not count exactly its own single cluster:\n%s", a.content)
	}

	b, ok := readMap(t, pool, "work")
	if !ok {
		t.Fatal("no work root map")
	}
	if !strings.Contains(b.content, "scope:work") {
		t.Errorf("work map is not written under its own scope:\n%s", b.content)
	}
}
