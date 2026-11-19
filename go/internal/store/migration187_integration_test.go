//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	migration187File = "187_derived_identity_index.sql"
	m187IndexDef     = `CREATE INDEX idx_context_derived_identity ON public.context_blocks USING btree (category, title COLLATE "C", scope) WHERE (metadata ? 'provenance'::text)`
	m187DDL          = `CREATE INDEX CONCURRENTLY idx_context_derived_identity ON context_blocks (category, title COLLATE "C", scope) WHERE metadata ? 'provenance'`
	m187Read         = `SELECT count(*) FROM context_blocks WHERE category = 'm187'`
	m187Write        = `UPDATE context_blocks SET updated_at = updated_at WHERE category = 'm187'`
)

// m187ProbeBuffers bounds the shared buffers of one provenance claim probe
// while idx_context_derived_identity stands, hit or miss, custom, generic or
// cached plan, in every statistics state, whatever the size of the archive:
// the descent of the index (metapage, root, leaf) and, for a hit, the heap
// page of the entry. Measured 3 for a miss and 4-5 for a hit (#386
// follow-up); twice the hit.
const m187ProbeBuffers = 10

// m187Seed writes, through the production write path, a live derivative and
// an archived one per title (arm m187, stratum derived), plus a client block
// per title in category m187.
func m187Seed(t *testing.T, pool *pgxpool.Pool, titles []string) {
	t.Helper()
	for _, ti := range titles {
		u386Derivative(t, pool, "m187d", ti, "m187", "m187", false)
		u386Derivative(t, pool, "m187a", ti, "m187", "m187", true)
		u386Client(t, pool, "m187", ti, "m187")
	}
}

// m187Titles are the identities whose comparison a collation could change:
// case, the German sharp s, the Turkish dotted I, composed and decomposed
// accents, a ligature, blanks around the text, a tab, wide characters, an
// emoji.
var m187Titles = []string{
	"Straße", "STRASSE", "İstanbul", "istanbul", "café", "café", "ﬁle", "file",
	" padded", "padded ", "Tab\tTitle", "日本語のタイトル", "emoji 🙂", "MiXeD Case",
}

// m187Variants are the probe titles: each seeded title, its upper and lower
// case, trimmed, with a blank added, and the other normal form of an accent.
func m187Variants() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, ti := range m187Titles {
		add(ti)
		add(strings.ToUpper(ti))
		add(strings.ToLower(ti))
		add(strings.TrimSpace(ti))
		add(ti + " ")
		add(" " + ti)
	}
	add("café")
	add("café")
	add("strasse")
	add("never written")
	sort.Strings(out)
	return out
}

// m187Probe is a pool of one connection whose statements auto_explain
// reports as notices (query text and plan with buffers).
type m187Probe struct {
	t     *testing.T
	pool  *pgxpool.Pool
	mu    sync.Mutex
	plans []m187Plan
}

type m187Plan struct {
	query   string
	buffers int64
	nodes   string
}

func newM187Probe(t *testing.T, dsn, planCacheMode string) *m187Probe {
	t.Helper()
	p := &m187Probe{t: t}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		i := strings.Index(n.Message, "{")
		if i < 0 || !strings.Contains(n.Message, "plan:") {
			return
		}
		var out struct {
			Query string         `json:"Query Text"`
			Plan  map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(n.Message[i:]), &out); err != nil {
			return
		}
		hit, _ := out.Plan["Shared Hit Blocks"].(float64)
		read, _ := out.Plan["Shared Read Blocks"].(float64)
		p.mu.Lock()
		p.plans = append(p.plans, m187Plan{query: out.Query, buffers: int64(hit + read), nodes: m187Nodes(out.Plan)})
		p.mu.Unlock()
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		for _, s := range []string{
			`LOAD 'auto_explain'`,
			`SET auto_explain.log_min_duration = 0`,
			`SET auto_explain.log_analyze = on`,
			`SET auto_explain.log_buffers = on`,
			`SET auto_explain.log_timing = off`,
			`SET auto_explain.log_format = 'json'`,
			`SET auto_explain.log_level = 'notice'`,
			`SET plan_cache_mode = ` + planCacheMode,
		} {
			if _, err := c.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	}
	if p.pool, err = pgxpool.NewWithConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.pool.Close)
	return p
}

func m187Nodes(pl map[string]any) string {
	s := fmt.Sprint(pl["Node Type"])
	if ix, ok := pl["Index Name"]; ok {
		s += "(" + fmt.Sprint(ix) + ")"
	}
	if kids, ok := pl["Plans"].([]any); ok {
		var ks []string
		for _, k := range kids {
			ks = append(ks, m187Nodes(k.(map[string]any)))
		}
		s += "[" + strings.Join(ks, ",") + "]"
	}
	return s
}

// claim runs store.ProvenanceClaimed on the probe's connection and returns
// the answer and the statements it ran.
func (p *m187Probe) claim(category, title, scope string) (bool, []m187Plan) {
	p.t.Helper()
	p.mu.Lock()
	p.plans = nil
	p.mu.Unlock()
	got, err := store.ProvenanceClaimed(context.Background(), p.pool, category, title, scope)
	if err != nil {
		p.t.Fatalf("ProvenanceClaimed(%q, %q, %q): %v", category, title, scope, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return got, append([]m187Plan(nil), p.plans...)
}

// split returns the readiness reading (if one ran) and the claim statement.
func m187Split(t *testing.T, plans []m187Plan) (reading *m187Plan, probe m187Plan) {
	t.Helper()
	for i := range plans {
		switch {
		case strings.Contains(plans[i].query, "FROM pg_index"):
			reading = &plans[i]
		case strings.Contains(plans[i].query, "metadata ?"):
			probe = plans[i]
		}
	}
	if probe.query == "" {
		t.Fatalf("no claim statement among %+v", plans)
	}
	return reading, probe
}

// TestMigration187 applies migration 187 to a database at 186:
// idx_context_derived_identity is built with the definition the file and
// store.derivedIdentityIndexReadySQL compare against, holding the
// derivatives only, live and archived; no block row is written. Below the
// threshold it is built inline under SHARE (a reader goes on, a writer
// waits); an index built out of band with the runbook is adopted and a second
// application changes nothing; above the threshold, on an INVALID index of
// the name and on a same-named index of another definition the file only
// warns and takes no lock on context_blocks. lock_timeout bounds the wait for
// SHARE beside an open writer.
func TestMigration187(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration187File)
	if err != nil {
		t.Fatalf("read %s: %v", migration187File, err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 186)
		m187Seed(t, pool, m187Titles)
		before := m182Fingerprint(t, pool)
		if err := store.RunMigrationsUpTo(ctx, pool, 187); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if m182Fingerprint(t, pool) != before {
			t.Errorf("migration 187 wrote rows of context_blocks")
		}
		ix := m182Index(t, pool, "idx_context_derived_identity")
		if ix.def != m187IndexDef || !ix.valid {
			t.Errorf("idx_context_derived_identity deparses as\n  %s (valid=%v)\nwant\n  %s", ix.def, ix.valid, m187IndexDef)
		}
		if want := int64(2 * len(m187Titles)); ix.entries != want {
			t.Errorf("idx_context_derived_identity holds %d entries, want %d (the live and the archived derivatives)", ix.entries, want)
		}
		// The title column has the database's default collation, and that
		// collation is deterministic: equality is byte equality in both forms
		// of the probe.
		var coll string
		var deterministic bool
		if err := pool.QueryRow(ctx, `SELECT c.collname, c.collisdeterministic FROM pg_attribute a JOIN pg_collation c ON c.oid = a.attcollation
			 WHERE a.attrelid = 'context_blocks'::regclass AND a.attname = 'title'`).Scan(&coll, &deterministic); err != nil {
			t.Fatal(err)
		}
		if coll != "default" || !deterministic {
			t.Errorf("context_blocks.title has collation %s (deterministic %v), want the deterministic default", coll, deterministic)
		}
	})

	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 186)
		m187Seed(t, pool, m187Titles[:3])
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m187Read, m187Write)
		t.Logf("migration 187 inline: %s; locks %v; reader waited %v, writer waited %v", run.took, run.modes, run.readerWaited, run.writerWaited)
		if run.err != nil {
			t.Fatal(run.err)
		}
		if !m182Holds(run.modes, "ShareLock") || m182Holds(run.modes, "AccessExclusiveLock") || m182Holds(run.modes, "ExclusiveLock") {
			t.Errorf("the inline build holds %v on context_blocks, want SHARE and nothing stronger", run.modes)
		}
		if run.readerWaited || !run.writerWaited {
			t.Errorf("reader waited %v (want false), writer waited %v (want true)", run.readerWaited, run.writerWaited)
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings: %v", run.warnings)
		}
		ix := m182Index(t, pool, "idx_context_derived_identity")
		if ix.def != m187IndexDef || !ix.valid {
			t.Errorf("after the file: %s (valid=%v)", ix.def, ix.valid)
		}
		again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m187Read, m187Write)
		if again.err != nil || len(again.modes) != 0 || again.writerWaited || len(again.warnings) != 0 {
			t.Errorf("second application: err %v, locks %v, writer waited %v, warnings %v", again.err, again.modes, again.writerWaited, again.warnings)
		}
		if ix2 := m182Index(t, pool, "idx_context_derived_identity"); ix2.oid != ix.oid {
			t.Errorf("the second application rebuilt the index: oid %d -> %d", ix.oid, ix2.oid)
		}
	})

	t.Run("built_out_of_band", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 186)
		m182Exec(t, pool, m187DDL)
		before := m182Index(t, pool, "idx_context_derived_identity")
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m187Read, m187Write)
		if run.err != nil || len(run.modes) != 0 || run.writerWaited || len(run.warnings) != 0 {
			t.Errorf("over the index built out of band: err %v, locks %v, writer waited %v, warnings %v", run.err, run.modes, run.writerWaited, run.warnings)
		}
		if ix := m182Index(t, pool, "idx_context_derived_identity"); ix.oid != before.oid || ix.def != m187IndexDef {
			t.Errorf("not adopted: oid %d -> %d, %s", before.oid, ix.oid, ix.def)
		}
	})

	for _, c := range []struct {
		name      string
		setup     []string
		reltuples float64
		clear     string // the runbook's step for what holds the name
	}{
		{"above_threshold", nil, 600000, ""},
		{"invalid", []string{m187DDL[len("CREATE INDEX CONCURRENTLY "):], `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_context_derived_identity'::regclass`}, 0,
			`DROP INDEX CONCURRENTLY idx_context_derived_identity`},
		{"foreign_definition", []string{`idx_context_derived_identity ON context_blocks (category, title, scope) WHERE metadata ? 'provenance'`}, 0,
			`ALTER INDEX idx_context_derived_identity RENAME TO idx_context_derived_identity_foreign`},
		// A relation that is not an index holds the name: the file warns
		// instead of failing on the CREATE INDEX (42P07).
		{"view", []string{`CREATE VIEW idx_context_derived_identity AS SELECT 1 AS x`}, 0,
			`ALTER VIEW idx_context_derived_identity RENAME TO idx_context_derived_identity_foreign`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 186)
			for _, s := range c.setup {
				if !strings.HasPrefix(s, "UPDATE") && !strings.HasPrefix(s, "CREATE") {
					s = "CREATE INDEX " + s
				}
				m182Exec(t, pool, s)
			}
			before := m182Index(t, pool, "idx_context_derived_identity")
			kind := m18xRelkind(t, pool, "idx_context_derived_identity")
			run := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m187Read, m187Write)
			t.Logf("%s: %s; locks %v; warnings %v", c.name, run.took, run.modes, run.warnings)
			if run.err != nil {
				t.Fatal(run.err)
			}
			if len(run.warnings) != 1 || m18xWarns(run, "idx_context_derived_identity") != 1 {
				t.Errorf("warnings %v, want one naming idx_context_derived_identity", run.warnings)
			}
			if m182Holds(run.modes, "ShareLock") || run.writerWaited {
				t.Errorf("the file holds %v on context_blocks (writer waited %v)", run.modes, run.writerWaited)
			}
			if after := m182Index(t, pool, "idx_context_derived_identity"); after.found != before.found || after.oid != before.oid {
				t.Errorf("the file changed the index: %+v -> %+v", before, after)
			}
			if k := m18xRelkind(t, pool, "idx_context_derived_identity"); k != kind {
				t.Errorf("the file changed what holds the name: relkind %q -> %q", kind, k)
			}
			// The runbook (drop the INVALID leftover, rename the foreign
			// object), then the file adopts the index.
			if c.clear != "" {
				m182Exec(t, pool, c.clear)
			}
			m182Exec(t, pool, m187DDL)
			again := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m187Read, m187Write)
			if again.err != nil || len(again.warnings) != 0 || len(again.modes) != 0 {
				t.Errorf("after the runbook: err %v, warnings %v, locks %v", again.err, again.warnings, again.modes)
			}
		})
	}

	t.Run("lock_timeout", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 186)
		m187Seed(t, pool, m187Titles[:1])
		hold, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, m187Write); err != nil {
			t.Fatal(err)
		}
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, "", "")
		var pgErr *pgconn.PgError
		if !errors.As(run.err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("the file beside an open writer: %v, want 55P03", run.err)
		}
		_ = hold.Rollback(ctx)
		if ix := m182Index(t, pool, "idx_context_derived_identity"); ix.found {
			t.Errorf("the failed file left %s", ix.def)
		}
	})
}

// TestDerivedIdentityProbeParity: store.ProvenanceClaimed and the foreign
// claim answer alike with idx_context_derived_identity standing (the
// C-collated comparison) and without it (the comparison before 187), for
// every identity a collation could tell apart — case, sharp s, dotted I,
// composed and decomposed accents, a ligature, blanks around the title, a
// tab, wide characters, an emoji — hit and miss, live and archived
// derivatives, a client block of the same title, another category or scope.
// The forms are checked by the statement the probe sends.
func TestDerivedIdentityProbeParity(t *testing.T) {
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	m187Seed(t, pool, m187Titles)
	probe := newM187Probe(t, dsn, "auto")
	type key struct{ category, title, scope string }
	var keys []key
	for _, ti := range m187Variants() {
		for _, cat := range []string{"m187d", "m187a", "m187", "M187D"} {
			for _, sc := range []string{"m187", "m187x"} {
				keys = append(keys, key{cat, ti, sc})
			}
		}
	}
	answers := func(form string) (map[key]bool, map[key][2]bool) {
		t.Helper()
		claims := map[key]bool{}
		foreign := map[key][2]bool{}
		for _, k := range keys {
			got, plans := probe.claim(k.category, k.title, k.scope)
			_, st := m187Split(t, plans)
			if c := strings.Contains(st.query, `COLLATE "C"`); c != (form == "C") {
				t.Fatalf("%s form: the probe sent %q", form, st.query)
			}
			claims[k] = got
			own, err := store.ForeignProvenanceClaimForTest(ctx, probe.pool, k.category, k.title, k.scope, "m187", fmt.Sprint(derived.StratumDerived))
			if err != nil {
				t.Fatal(err)
			}
			other, err := store.ForeignProvenanceClaimForTest(ctx, probe.pool, k.category, k.title, k.scope, "other-arm", fmt.Sprint(derived.StratumDerived))
			if err != nil {
				t.Fatal(err)
			}
			foreign[k] = [2]bool{own, other}
		}
		return claims, foreign
	}
	store.ForgetDerivedIdentityReadingsForTest()
	cClaims, cForeign := answers("C")
	m182Exec(t, pool, `DROP INDEX idx_context_derived_identity`)
	store.ForgetDerivedIdentityReadingsForTest()
	oClaims, oForeign := answers("default")
	hits := 0
	for _, k := range keys {
		if cClaims[k] != oClaims[k] {
			t.Errorf("ProvenanceClaimed(%q, %q, %q): %v with the C-collated probe, %v before 187", k.category, k.title, k.scope, cClaims[k], oClaims[k])
		}
		if cForeign[k] != oForeign[k] {
			t.Errorf("foreign claim (%q, %q, %q): own/other arm %v with the C-collated probe, %v before 187", k.category, k.title, k.scope, cForeign[k], oForeign[k])
		}
		if cClaims[k] {
			hits++
		}
	}
	// Exactly the seeded derivative identities are claimed: each title in
	// categories m187d and m187a of scope m187, nothing of the client blocks.
	if want := 2 * len(m187Titles); hits != want {
		t.Errorf("%d identities claimed, want %d", hits, want)
	}
	for _, k := range keys {
		if cForeign[k][0] {
			t.Errorf("the owning arm reads its own derivative (%q, %q, %q) as a foreign claim", k.category, k.title, k.scope)
		}
		if cForeign[k][1] != cClaims[k] {
			t.Errorf("another arm: foreign claim %v, claim %v for (%q, %q, %q)", cForeign[k][1], cClaims[k], k.category, k.title, k.scope)
		}
	}
	t.Logf("%d probe identities, %d claimed, both forms agree", len(keys), hits)
}

// TestDerivedIdentityProbeGate: the probe sends its C-collated form only
// while idx_context_derived_identity stands as 187 builds it — not while the
// index is missing, INVALID or of another definition — and reads that state
// once per interval and database: a second probe within the interval runs no
// catalog query, one after it reads the catalog again. A probe that still
// trusts a reading taken before the index was dropped answers as before.
// The plan is TestDerivedIdentityProbeCost's (this store is too small for
// an index to pay).
func TestDerivedIdentityProbeGate(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	m187Seed(t, pool, m187Titles[:2])
	probe := newM187Probe(t, dsn, "auto")
	form := func(label string, wantC bool, wantReading bool) {
		t.Helper()
		got, plans := probe.claim("m187a", m187Titles[0], "m187")
		reading, st := m187Split(t, plans)
		if !got {
			t.Errorf("%s: the archived derivative is not claimed", label)
		}
		if c := strings.Contains(st.query, `COLLATE "C"`); c != wantC {
			t.Errorf("%s: C-collated %v, want %v (%s)", label, c, wantC, st.nodes)
		}
		if (reading != nil) != wantReading {
			t.Errorf("%s: catalog reading %v, want %v", label, reading != nil, wantReading)
		}
		t.Logf("%s: reading %v, probe %d buffers %s", label, reading != nil, st.buffers, st.nodes)
	}
	store.ForgetDerivedIdentityReadingsForTest()
	form("index stands, first probe", true, true)
	form("index stands, within the interval", true, false)
	store.AgeDerivedIdentityReadingsForTest(store.DerivedIdentityProbeIntervalForTest)
	form("index stands, after the interval", true, true)
	m182Exec(t, pool, `DROP INDEX idx_context_derived_identity`)
	form("index dropped, reading still trusted", true, false)
	store.AgeDerivedIdentityReadingsForTest(store.DerivedIdentityProbeIntervalForTest)
	form("index dropped, after the interval", false, true)
	for _, c := range []struct {
		name  string
		setup []string
	}{
		{"invalid", []string{"CREATE INDEX " + m187DDL[len("CREATE INDEX CONCURRENTLY "):],
			`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_context_derived_identity'::regclass`}},
		{"foreign definition", []string{`CREATE INDEX idx_context_derived_identity ON context_blocks (category, title, scope) WHERE metadata ? 'provenance'`}},
	} {
		m182Exec(t, pool, `DROP INDEX IF EXISTS idx_context_derived_identity`)
		for _, s := range c.setup {
			m182Exec(t, pool, s)
		}
		store.ForgetDerivedIdentityReadingsForTest()
		form("index "+c.name, false, true)
	}
	m182Exec(t, pool, `DROP INDEX IF EXISTS idx_context_derived_identity`)
	m182Exec(t, pool, m187DDL)
	store.ForgetDerivedIdentityReadingsForTest()
	form("index built again", true, true)
}

// TestDerivedIdentityProbeCost (the cost pin of migration 187): a store of
// 50 000 live blocks and 200 000 archived ones without provenance beside
// 5 000 derivatives, half of them archived. Every probe — a hit on a live
// and on an archived derivative, a miss on the title of an archived client
// block and on an unknown title — reads at most m187ProbeBuffers, in custom,
// generic and cached plans and with statistics fresh, stale (taken before
// the archive grew) and never taken; the probe reads
// idx_context_derived_identity and nothing else. Before 187 the probe read
// idx_trgm_title_gist and grew with the archive (#386 follow-up: a miss
// p50 69 / 197 / 312 buffers at 20 000 / 220 000 / 420 000 archived blocks).
func TestDerivedIdentityProbeCost(t *testing.T) {
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for _, s := range []string{
		`DROP INDEX IF EXISTS idx_embedding_hnsw`,
		`ALTER TABLE context_blocks SET (autovacuum_enabled = false)`,
		`ALTER TABLE context_blocks DISABLE TRIGGER USER`,
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`,
	} {
		m182Exec(t, pool, s)
	}
	seed := func(sql string, args ...any) {
		t.Helper()
		t0 := time.Now()
		m182Exec(t, pool, sql, args...)
		t.Logf("seed %s", time.Since(t0).Round(time.Millisecond))
	}
	provenance := `jsonb_build_object('provenance', jsonb_build_object('v', 1, 'arm', 'm187', 'stratum', ` + fmt.Sprint(int(derived.StratumDerived)) + `))`
	seed(`INSERT INTO context_blocks (category, title, content, scope)
		SELECT 'learnings', 'live block ' || g, 'live ' || g, 'm187-' || (g % 50) FROM generate_series(1, 50000) g`)
	seed(`INSERT INTO context_blocks (category, title, content, scope, metadata, is_archived)
		SELECT 'insights', 'derived topic ' || g, 'derivative ' || g, 'm187-' || (g % 50), `+provenance+`, g % 2 = 0 FROM generate_series(1, 5000) g`)
	m182Exec(t, pool, `VACUUM ANALYZE context_blocks`)
	// The archive grows after the statistics were taken (stale).
	seed(`INSERT INTO context_blocks (category, title, content, scope, is_archived)
		SELECT 'insights', 'archived note ' || g, 'archived ' || g, 'm187-' || (g % 50), true FROM generate_series(1, 200000) g`)
	m182Exec(t, pool, `VACUUM context_blocks`)
	m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
	var ready bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.idx_context_derived_identity') IS NOT NULL`).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	t.Logf("idx_context_derived_identity stands: %v", ready)
	targets := []struct {
		name, category, title, scope string
		want                         bool
	}{
		{"hit, live derivative", "insights", "derived topic 7", "m187-7", true},
		{"hit, archived derivative", "insights", "derived topic 8", "m187-8", true},
		{"miss, archived client block", "insights", "archived note 12345", "m187-45", false},
		{"miss, unknown title", "insights", "never written 1", "m187-1", false},
		{"miss, derivative title in another scope", "insights", "derived topic 9", "m187-10", false},
	}
	clear := func() {
		rows, err := pool.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = 'context_blocks'::regclass AND a.attnum > 0 AND NOT a.attisdropped`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		m182Exec(t, pool, `SELECT pg_clear_relation_stats('public', 'context_blocks')`)
		for _, c := range cols {
			m182Exec(t, pool, `SELECT pg_clear_attribute_stats('public', 'context_blocks', $1::text, false)`, c)
		}
	}
	for _, st := range []struct {
		name string
		set  func()
	}{
		{"stale", func() {}},
		{"never taken", clear},
		{"fresh", func() { m182Exec(t, pool, `VACUUM ANALYZE context_blocks`) }},
	} {
		st.set()
		for _, mode := range []string{"force_custom_plan", "force_generic_plan", "auto"} {
			probe := newM187Probe(t, dsn, mode)
			store.ForgetDerivedIdentityReadingsForTest()
			runs := 1
			if mode == "auto" {
				runs = 6 // the driver's cached statement, past the fifth custom plan
			}
			for _, tg := range targets {
				var got bool
				var plans []m187Plan
				for i := 0; i < runs; i++ {
					got, plans = probe.claim(tg.category, tg.title, tg.scope)
				}
				_, pr := m187Split(t, plans)
				t.Logf("statistics %s, %s, %s: claimed=%v buffers=%d %s", st.name, mode, tg.name, got, pr.buffers, pr.nodes)
				if got != tg.want {
					t.Errorf("statistics %s, %s, %s: claimed %v, want %v", st.name, mode, tg.name, got, tg.want)
				}
				if pr.buffers > m187ProbeBuffers {
					t.Errorf("statistics %s, %s, %s: the probe read %d buffers beside 200 000 archived blocks, over %d (%s)", st.name, mode, tg.name, pr.buffers, m187ProbeBuffers, pr.nodes)
				}
				if !strings.Contains(pr.nodes, "idx_context_derived_identity") {
					t.Errorf("statistics %s, %s, %s: the probe does not read idx_context_derived_identity: %s", st.name, mode, tg.name, pr.nodes)
				}
			}
			probe.pool.Close()
		}
	}
}
