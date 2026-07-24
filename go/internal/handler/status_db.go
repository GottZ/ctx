package handler

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbStatus is the server-admin-only /api/status "db" section (Evokoa-Clean-
// Room design/03 §4.7 — VERBINDLICH wire shape, K4 status-merge-slot 1b).
// It rides its OWN async cadence (events.db_stats_interval), the QueueStats
// pattern applied a second time (status.go's qs/qsAt/scanQueueAsync is the
// "Nachbar-Refresher" design/03 §4.7 names as the muster to follow) — never
// buildCheap: catalog/pg_stat reads are cheap but pointless at the 5s base
// tick. Field names/JSON tags are pinned 1:1 to design/03 §4.7's struct
// literal; TestStatusGoldenKeys is the anchor (K4: additive-only, this type
// is new — no existing field/signature changes).
type dbStatus struct {
	MigrationsApplied int      `json:"migrations_applied"` // _migrations count
	MigrationsMax     int      `json:"migrations_max"`
	Contract          string   `json:"contract"` // ok|drift|unchecked|off (Kurzform; Details nur /api/contract)
	ContractDrifts    int      `json:"contract_drifts"`
	Extensions        []extRow `json:"extensions"`
	ServerGUCs        []gucRow `json:"server_gucs"` // informativ, nie Drift (design/03 §4.3)
	Relations         []relRow `json:"relations"`
	HNSW              hnswRow  `json:"hnsw"`
	// EmbedBacklog is a pointer: null when the Introspektions-Guard (§4.7)
	// finds no partial index covering the backfill predicate — NEVER a
	// scanned/scan-avoided 0, that would be a Schein-Wert (Gate G3).
	EmbedBacklog *int `json:"embed_backlog"`
	// ChannelProbe is reserved for W03-8 (status.channel_probe_interval) and
	// stays null for the entirety of this wave (task brief: "bleibt in
	// dieser Welle IMMER null").
	ChannelProbe *probeRow `json:"channel_probe"`
}

// extRow is one installed extension (design/03 §4.7: {name, version}).
type extRow struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// gucRow is one informative server GUC (design/03 §4.7: {name, value,
// source}) — never a Drift source (design/03 §4.3's "bewusst NICHT im
// Vertrag").
type gucRow struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// relRow is one relation's size/bloat telemetry (design/03 §4.7: {name,
// total_bytes, dead_tuples, live_tuples, last_autovacuum, hypertable bool}).
// For a hypertable, every field is chunk-aggregated (readHypertableRelation)
// — the parent alone is a near-empty shell (design/03 §2 live finding: ~104
// kB forever).
type relRow struct {
	Name           string     `json:"name"`
	TotalBytes     int64      `json:"total_bytes"`
	DeadTuples     int64      `json:"dead_tuples"`
	LiveTuples     int64      `json:"live_tuples"`
	LastAutovacuum *time.Time `json:"last_autovacuum"`
	Hypertable     bool       `json:"hypertable"`
}

// hnswRow is idx_embedding_hnsw's size + density (design/03 §4.7:
// {index_bytes, bytes_per_row *float64, m, ef_construction,
// ef_search_effective}). BytesPerRow is nil when the index's OWN
// pg_class.reltuples is <=0 (unanalyzed/empty) — never a division by a
// placeholder (Gate G4).
type hnswRow struct {
	IndexBytes        int64    `json:"index_bytes"`
	BytesPerRow       *float64 `json:"bytes_per_row"`
	M                 int      `json:"m"`
	EfConstruction    int      `json:"ef_construction"`
	EfSearchEffective string   `json:"ef_search_effective"`
}

// probeRow is the RESERVED shape for the per-channel latency probe
// (status.channel_probe_interval, W03-8 — not built in this wave).
// dbStatus.ChannelProbe is always nil here; this type exists purely so the
// pointer/wire convention is already correct before W03-8 lands. W03-8 owns
// the real field set and may redefine this freely (no consumer depends on
// its shape yet).
type probeRow struct{}

// hnswIndexName is the one ANN index this section reports on (design/03
// §4.7's HNSW row; 001_initial.sql:250-252).
const hnswIndexName = "idx_embedding_hnsw"

// embedBacklogCap is the LIMIT the backlog count query stops at (design/03
// §4.7: "Wert ab 10.000 als gedeckelt kennzeichnen"). The field is *int, so
// the cap is expressed as the value 10000 itself (not a "10000+" string) —
// a deliberate, documented reading of the design's *int constraint.
const embedBacklogCap = 10000

// dbStatusRelationNames is the eight-relation Code-Konstante (design/03
// §4.7 Revision: context_blobs + context_pending_writes added — blob
// payloads are the largest single objects at Ziel-Scale, pending_writes is
// the second live hypertable). Order is the wire order.
var dbStatusRelationNames = []string{
	"context_blocks", "context_dream_links", "context_structural_links",
	"context_access_log", "context_embed_cache", "context_llm_log",
	"context_pending_writes", "context_blobs",
}

// dbStatusServerGUCNames is the four informative server GUCs (design/03
// §4.7/Konsistenz-Linse-minor: shared_buffers/work_mem/
// maintenance_work_mem/effective_cache_size, Inventur §6) — deployment
// properties, never Drift sources (design/03 §4.3).
var dbStatusServerGUCNames = []string{
	"shared_buffers", "work_mem", "maintenance_work_mem", "effective_cache_size",
}

// buildDBStatus gathers the full db-section in one pass. Every sub-query is
// best-effort: a single relation/extension/GUC failure logs a WARN and
// degrades that slice/row rather than failing the whole section (the same
// resilience posture buildCheap already applies to llm24h/last-cycle).
func buildDBStatus(ctx context.Context, pool *pgxpool.Pool) *dbStatus {
	db := &dbStatus{
		Extensions:   queryExtensions(ctx, pool),
		ServerGUCs:   queryServerGUCs(ctx, pool),
		Relations:    queryRelations(ctx, pool),
		HNSW:         queryHNSW(ctx, pool),
		EmbedBacklog: queryEmbedBacklog(ctx, pool),
		// ChannelProbe intentionally left nil — W03-8.
	}

	if applied, maxVer, err := queryMigrationsCounts(ctx, pool); err != nil {
		slog.Warn("status: db-section migrations count query failed", "error", err)
	} else {
		db.MigrationsApplied = applied
		db.MigrationsMax = maxVer
	}

	report, hasReport := schemacontract.LatestReport()
	db.Contract = dbStatusContractValue(report, hasReport)
	if hasReport {
		db.ContractDrifts = len(report.Drifts)
	}

	return db
}

// dbStatusContractValue derives the db-section's Kurzform contract value
// (design/03 §4.7: "ok|drift|unchecked|off ... Details nur /api/contract").
// This is the server-admin-only ADMIN view, deliberately NOT the public
// /health D03 collapse (health.go's schemaContractHealthValue folds
// drift|unchecked into "attention") — an admin polling /api/status is
// exactly the audience the collapse was designed to protect FROM losing
// detail.
func dbStatusContractValue(report schemacontract.Report, hasReport bool) string {
	if !hasReport {
		return schemacontract.StatusUnchecked
	}
	if report.Mode == schemacontract.ModeOff {
		return "off"
	}
	return report.Status
}

func queryMigrationsCounts(ctx context.Context, pool *pgxpool.Pool) (applied, maxVersion int, err error) {
	err = pool.QueryRow(ctx,
		`SELECT count(*)::int, COALESCE(max(version), 0)::int FROM _migrations`,
	).Scan(&applied, &maxVersion)
	return applied, maxVersion, err
}

func queryExtensions(ctx context.Context, pool *pgxpool.Pool) []extRow {
	out := []extRow{}
	rows, err := pool.Query(ctx, `SELECT extname, extversion FROM pg_extension ORDER BY extname`)
	if err != nil {
		slog.Warn("status: db-section extensions query failed", "error", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e extRow
		if err := rows.Scan(&e.Name, &e.Version); err != nil {
			slog.Warn("status: db-section extensions scan failed", "error", err)
			return out
		}
		out = append(out, e)
	}
	if rows.Err() != nil {
		slog.Warn("status: db-section extensions rows error", "error", rows.Err())
	}
	return out
}

// queryServerGUCs reads the four informative GUCs in ONE query and re-orders
// the result onto dbStatusServerGUCNames's fixed order (ORDER-BY-name-style
// stability, the buildStatusProfiles convention) — a missing name (should
// never happen, pg_settings always carries these four) is simply omitted
// rather than faked.
func queryServerGUCs(ctx context.Context, pool *pgxpool.Pool) []gucRow {
	out := make([]gucRow, 0, len(dbStatusServerGUCNames))
	rows, err := pool.Query(ctx,
		`SELECT name, setting, source FROM pg_settings WHERE name = ANY($1::text[])`,
		dbStatusServerGUCNames)
	if err != nil {
		slog.Warn("status: db-section server_gucs query failed", "error", err)
		return out
	}
	defer rows.Close()
	byName := make(map[string]gucRow, len(dbStatusServerGUCNames))
	for rows.Next() {
		var g gucRow
		if err := rows.Scan(&g.Name, &g.Value, &g.Source); err != nil {
			slog.Warn("status: db-section server_gucs scan failed", "error", err)
			return out
		}
		byName[g.Name] = g
	}
	if rows.Err() != nil {
		slog.Warn("status: db-section server_gucs rows error", "error", rows.Err())
		return out
	}
	for _, n := range dbStatusServerGUCNames {
		if g, ok := byName[n]; ok {
			out = append(out, g)
		}
	}
	return out
}

// queryRelations reads the eight-relation Code-Konstante, branching each one
// through the hypertable-aware reader (design/03 §4.7 Revision). A single
// relation's read failure is logged and that relation is simply absent from
// the slice — never a zero-value row masquerading as a real reading.
func queryRelations(ctx context.Context, pool *pgxpool.Pool) []relRow {
	out := make([]relRow, 0, len(dbStatusRelationNames))
	for _, name := range dbStatusRelationNames {
		r, err := queryOneRelation(ctx, pool, name)
		if err != nil {
			slog.Warn("status: db-section relation read failed", "relation", name, "error", err)
			continue
		}
		out = append(out, r)
	}
	return out
}

// queryOneRelation is the hypertable-aware branch point (design/03 §4.7
// Revision): timescaledb_information.hypertables decides which reader runs
// — TestStatusDBHypertableAware (G2) is the negative probe proving the
// naive pg_total_relation_size(parent) path undercounts.
func queryOneRelation(ctx context.Context, pool *pgxpool.Pool, name string) (relRow, error) {
	var isHypertable bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM timescaledb_information.hypertables
			 WHERE hypertable_schema = 'public' AND hypertable_name = $1)`,
		name,
	).Scan(&isHypertable); err != nil {
		return relRow{}, err
	}
	if isHypertable {
		return queryHypertableRelation(ctx, pool, name)
	}
	return queryPlainRelation(ctx, pool, name)
}

// queryPlainRelation is the N9 Size-Introspektions-Anker pattern
// (store/blocks.go:990, blobs.go:275) extended with pg_stat_user_tables
// dead/live/last_autovacuum.
func queryPlainRelation(ctx context.Context, pool *pgxpool.Pool, name string) (relRow, error) {
	r := relRow{Name: name}
	err := pool.QueryRow(ctx, `
		SELECT pg_total_relation_size(c.oid)::bigint,
		       COALESCE(s.n_dead_tup, 0)::bigint,
		       COALESCE(s.n_live_tup, 0)::bigint,
		       s.last_autovacuum
		  FROM pg_class c
		  LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
		 WHERE c.relname = $1 AND c.relnamespace = 'public'::regnamespace`,
		name,
	).Scan(&r.TotalBytes, &r.DeadTuples, &r.LiveTuples, &r.LastAutovacuum)
	return r, err
}

// queryHypertableRelation is the design/03 §4.7 Revision reader: the parent
// relation alone is a near-empty shell (§2 live finding: context_llm_log
// parent ~104 kB forever, reltuples=-1) — total_bytes comes from
// hypertable_size() (chunks included), dead/live/last_autovacuum are
// aggregated over every chunk via timescaledb_information.chunks joined to
// pg_stat_user_tables on the CHUNK relation.
func queryHypertableRelation(ctx context.Context, pool *pgxpool.Pool, name string) (relRow, error) {
	r := relRow{Name: name, Hypertable: true}
	if err := pool.QueryRow(ctx,
		`SELECT hypertable_size($1::regclass)::bigint`, name,
	).Scan(&r.TotalBytes); err != nil {
		return relRow{}, err
	}
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(s.n_dead_tup), 0)::bigint,
		       COALESCE(sum(s.n_live_tup), 0)::bigint,
		       max(s.last_autovacuum)
		  FROM timescaledb_information.chunks ch
		  JOIN pg_stat_user_tables s
		    ON s.relname = ch.chunk_name AND s.schemaname = ch.chunk_schema
		 WHERE ch.hypertable_schema = 'public' AND ch.hypertable_name = $1`,
		name,
	).Scan(&r.DeadTuples, &r.LiveTuples, &r.LastAutovacuum)
	if err != nil {
		return relRow{}, err
	}
	return r, nil
}

// queryHNSW reads idx_embedding_hnsw's size, reloptions and density
// (design/03 §4.7). BytesPerRow uses the INDEX's OWN pg_class.reltuples as
// denominator, NOT the table's (Revision: the index only carries entries
// where aminsert actually ran — pgvector's HNSW skips NULL embeddings — so
// its reltuples undercounts the table on purpose; dividing by the bigger
// table count would UNDERSTATE bytes/row, masking the up-signal the field
// exists to show). Empirically verified against PG18/pgvector 0.8.2 (this
// wave's rot probe, see status_db_integration_test.go's TestStatusDBHNSW*
// doc comment): plain ANALYZE (and a plain, nothing-to-delete VACUUM)
// re-mirror the index's reltuples onto the TABLE's estimate — only a fresh
// index build (CREATE INDEX / REINDEX / VACUUM FULL, all of which rescan
// the heap through aminsert) yields the accurate NULL-aware count. Reading
// pg_class.reltuples verbatim is still the semantically correct mechanism
// (design/03 §4.7's own wording) regardless of which maintenance operation
// last set it; a steady-state production instance may see this value drift
// back toward the table's count between REINDEXes — a live-system caveat,
// not a defect in this reader.
func queryHNSW(ctx context.Context, pool *pgxpool.Pool) hnswRow {
	row := hnswRow{}
	var reltuples float64
	var relopts []string
	err := pool.QueryRow(ctx, `
		SELECT pg_relation_size(c.oid)::bigint, c.reltuples, c.reloptions
		  FROM pg_class c
		 WHERE c.relname = $1 AND c.relnamespace = 'public'::regnamespace`,
		hnswIndexName,
	).Scan(&row.IndexBytes, &reltuples, &relopts)
	if err != nil {
		slog.Warn("status: db-section hnsw read failed", "error", err)
		return row
	}
	opts := parseIndexRelOptions(relopts)
	row.M, _ = strconv.Atoi(opts["m"])
	row.EfConstruction, _ = strconv.Atoi(opts["ef_construction"])
	// reltuples <= 0 covers BOTH the documented -1 "never analyzed" sentinel
	// (design/03 §3.3) and a genuinely empty (0-row) fresh index — neither
	// is a valid denominator (Gate G4).
	if reltuples > 0 {
		bpr := float64(row.IndexBytes) / reltuples
		row.BytesPerRow = &bpr
	}
	row.EfSearchEffective = queryEfSearchEffective(ctx, pool)
	return row
}

func parseIndexRelOptions(opts []string) map[string]string {
	m := make(map[string]string, len(opts))
	for _, o := range opts {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

// queryEfSearchEffective probes hnsw.ef_search the same library-load-first
// way schemacontract's GUC probe does (check.go's probeOneGUC — a fresh
// backend does not recognize any hnsw.* GUC until the pgvector library has
// been loaded via one real vector operation, design/03 §4.3/§2 live
// finding). The probe runs in a rolled-back transaction (read-only,
// mirrors the contract package's own probe) so both statements land on the
// SAME backend. "(default)" is appended when pg_settings.source reports the
// compiled-in default — design/03 §4.7's "Fallback-Kennzeichnung 'default
// 40'" — rather than string-matching the value 40, which would misclassify
// an operator who has explicitly SET ef_search to 40.
func queryEfSearchEffective(ctx context.Context, pool *pgxpool.Pool) string {
	const fallback = "40 (default)"
	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.Warn("status: db-section ef_search probe begin failed", "error", err)
		return fallback
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT ('[0]'::vector)::text`); err != nil {
		slog.Warn("status: db-section ef_search probe library load failed", "error", err)
		return fallback
	}
	var setting, source string
	if err := tx.QueryRow(ctx,
		`SELECT setting, source FROM pg_settings WHERE name = 'hnsw.ef_search'`,
	).Scan(&setting, &source); err != nil {
		slog.Warn("status: db-section ef_search probe read failed", "error", err)
		return fallback
	}
	if source == "default" {
		return setting + " (default)"
	}
	return setting
}

// queryEmbedBacklog is the design/03 §4.7 Revision metric: the real
// backfill predicate (embedding IS NULL AND NOT is_archived), guarded by an
// Introspektions-Guard (Defense-in-Depth alongside migration-owned
// idx_embedding_pending, Achse 04/K2) — it computes ONLY when a matching
// partial index exists on context_blocks, else null (never 0, never an
// unguarded scan; Gate G3). The LIMIT bounds RETURNED rows only — the
// guard is what keeps the worst case (an empty backlog) index-only instead
// of a full seq scan.
func queryEmbedBacklog(ctx context.Context, pool *pgxpool.Pool) *int {
	guarded, err := embedBacklogIndexExists(ctx, pool)
	if err != nil {
		slog.Warn("status: db-section embed-backlog guard query failed", "error", err)
		return nil
	}
	if !guarded {
		slog.Warn("status: embed_backlog guard — no partial index on context_blocks covering " +
			"(embedding IS NULL AND NOT is_archived) — backlog metric disabled (null), never an unguarded scan")
		return nil
	}
	var n int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM context_blocks
			 WHERE embedding IS NULL AND NOT is_archived
			 LIMIT $1
		) t`, embedBacklogCap+1,
	).Scan(&n)
	if err != nil {
		slog.Warn("status: db-section embed-backlog count query failed", "error", err)
		return nil
	}
	if n > embedBacklogCap {
		n = embedBacklogCap
	}
	return &n
}

// embedBacklogIndexExists looks for a partial index on context_blocks whose
// predicate covers "embedding IS NULL AND NOT is_archived" (matched via
// ILIKE fragments rather than an exact pg_get_expr string, so the guard
// survives Postgres's own deparse formatting rather than pinning it) — the
// migration-owned idx_embedding_pending (109_embed_provenance.sql, Achse
// 04/K2) is the index this finds on a fresh chain; the guard has no
// dependency on that index's NAME, only its shape.
func embedBacklogIndexExists(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_index i
			  JOIN pg_class t ON t.oid = i.indrelid
			  JOIN pg_namespace n ON n.oid = t.relnamespace
			 WHERE n.nspname = 'public' AND t.relname = 'context_blocks'
			   AND i.indpred IS NOT NULL
			   AND pg_get_expr(i.indpred, i.indrelid) ILIKE '%embedding%is null%'
			   AND pg_get_expr(i.indpred, i.indrelid) ILIKE '%not%is_archived%'
		)`,
	).Scan(&exists)
	return exists, err
}
