// sample.go — stratification (design/01 §4.2.1) and query sampling (§4.2.2)
// for recall_check. Wire-free by doctrine (E-01-2): logged queries join
// against context_embed_cache via the touch-free PeekByHash (cache hits
// only, never an embed call), small strata fill up leave-one-out with
// document vectors. Both paths are deterministic and scope-local.
//
// Source: https://github.com/GottZ/ctx
package recall

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
)

// Log-sampling mechanics (§4.2.2): the access-log read is a BOUNDED backward
// stream over idx_access_log_created — cost ceiling = rows actually read
// (index-bound, newest first), never the table size. At target scale
// context_access_log sits in the 100M+ row class (one row per result block),
// so any full-table dedup (GROUP BY / DISTINCT ON without a window) would be
// a full scan per run. These two are sampler mechanics pinned by the design,
// not policy knobs — the tunable sample size is recall_check.queries_per_stratum.
const (
	logWindowDays  = 30
	logMaxDistinct = 500
)

// StratumPlan describes one stratum to be probed: which scope window, with
// which per-scope type allowlist, and how many embedded blocks it holds.
type StratumPlan struct {
	Stratum        string   // "small" | "medium" | "large" | "all"
	Scope          *string  // measured scope; nil for the pseudo-stratum "all"
	Scopes         []string // predicate scope arm: [scope], or all scopes for "all"
	VisibleTypes   []string // per-scope allowlist (SnapshotForTenant) / base set for "all"
	CorpusEmbedded int      // visible embedded blocks in the measurement window
	ScopeChanged   bool     // the class's largest scope differs from the previous run (§4.2.1 object stability)
}

// PlanStrata builds the run's stratum list (§4.2.1). Two steps, because the
// type allowlist can differ PER SCOPE (per-tenant registries): (1) distinct
// scopes over embedded active blocks, (2) per-scope count under that scope's
// SnapshotForTenant allowlist — the SAME allowlist later used in both probe
// legs of the stratum, so the probe predicate is the production predicate of
// that scope. SnapshotForTenant is the background contract (registry.go:
// "BACKGROUND iteration ... ONLY"); SnapshotForRequest would silently fall
// back to the base generation without an auth context and measure a foreign
// predicate. Classification: small n<=b1 < medium n<=b2 < large; per class
// the LARGEST scope is measured (deterministic, comparable across runs; ties
// break on scope name). The pseudo-stratum "all" is the union of all scopes
// under the BASE-set allowlist (Snapshot()) — deliberately the server-global
// view, not a sum of tenant views. Empty classes are skipped (visible as
// "stratum absent", never a silent recall=NULL). prevScopes maps stratum →
// previously measured scope; a class whose winner changed is stamped
// ScopeChanged (T1 requires scope constancy across its 3 runs).
//
// Both the DISTINCT-scope probe and the per-scope counts ride the partial
// covering index idx_blocks_stratify_covering (migration 111, K3).
func PlanStrata(ctx context.Context, pool *pgxpool.Pool, registry *blocktype.Registry, boundSmall, boundMedium int, prevScopes map[string]string) ([]StratumPlan, error) {
	if boundSmall <= 0 || boundMedium <= boundSmall {
		return nil, fmt.Errorf("recall: invalid strata bounds %d,%d (need 0 < b1 < b2)", boundSmall, boundMedium)
	}

	rows, err := pool.Query(ctx,
		`SELECT DISTINCT scope FROM context_blocks
		 WHERE NOT is_archived AND embedding IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("recall: distinct scopes: %w", err)
	}
	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recall: scan scope: %w", err)
		}
		scopes = append(scopes, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall: scope rows: %w", err)
	}
	sort.Strings(scopes)

	type scoped struct {
		scope   string
		count   int
		visible []string
	}
	// winner per class: index 0=small, 1=medium, 2=large
	var winner [3]*scoped
	classNames := [3]string{"small", "medium", "large"}

	for _, scope := range scopes {
		snap := registry.SnapshotForTenant(ctx, scope)
		visible := snap.VisibleTypes()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks
			 WHERE NOT is_archived AND embedding IS NOT NULL
			   AND scope = $1 AND type_name = ANY($2)`,
			scope, visible,
		).Scan(&n); err != nil {
			return nil, fmt.Errorf("recall: count scope %s: %w", scope, err)
		}
		if n == 0 {
			continue // allowlist filtered everything — nothing measurable
		}
		cls := 2
		switch {
		case n <= boundSmall:
			cls = 0
		case n <= boundMedium:
			cls = 1
		}
		if winner[cls] == nil || n > winner[cls].count ||
			(n == winner[cls].count && scope < winner[cls].scope) {
			winner[cls] = &scoped{scope: scope, count: n, visible: visible}
		}
	}

	var plans []StratumPlan
	for cls, w := range winner {
		if w == nil {
			continue
		}
		scope := w.scope
		plans = append(plans, StratumPlan{
			Stratum:        classNames[cls],
			Scope:          &scope,
			Scopes:         []string{scope},
			VisibleTypes:   w.visible,
			CorpusEmbedded: w.count,
			ScopeChanged:   prevScopes != nil && prevScopes[classNames[cls]] != "" && prevScopes[classNames[cls]] != scope,
		})
	}

	if len(scopes) > 0 {
		baseVisible := registry.Snapshot().VisibleTypes()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks
			 WHERE NOT is_archived AND embedding IS NOT NULL
			   AND scope = ANY($1) AND type_name = ANY($2)`,
			scopes, baseVisible,
		).Scan(&n); err != nil {
			return nil, fmt.Errorf("recall: count all: %w", err)
		}
		if n > 0 {
			plans = append(plans, StratumPlan{
				Stratum:        "all",
				Scopes:         scopes,
				VisibleTypes:   baseVisible,
				CorpusEmbedded: n,
			})
		}
	}
	return plans, nil
}

// SampleLogQueries collects up to limit REAL query vectors: a bounded
// backward stream over the access log (30d window, hard stop after 500
// distinct texts, Go-side dedup — see the constants above), each distinct
// text joined against context_embed_cache via HashKey(PrefixQuery, text) and
// the TOUCH-FREE PeekByHash. Cache hits only — no embed wire call, no
// admission lease, and no hit_count/last_access mutation (§4.2.2/§5.4; gate
// (e) pins the touch-freedom). model is the effective embed model string
// (injected by the caller); empty means "no cache join possible" and returns
// nil — the loo path covers the run, wire-free doctrine intact.
//
// The source != 'armsweep' predicate is mandatory (design/05 §5 B4): the
// ctx-armsweep driver's own arm_ranks requests log their query text into this
// very table, so a single sweep of ~950 constructed queries would dominate the
// daily recall check's query distribution. Same wording as the one other
// consumer that already filters (goldset/db.go:141); IS DISTINCT FROM because
// metadata is nullable and its DEFAULT '{}' has no 'source' key either — a
// plain <> would drain the real queries this sampler exists for.
func SampleLogQueries(ctx context.Context, pool *pgxpool.Pool, model string, limit int) ([][]float32, error) {
	if model == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT query_text FROM context_access_log
		 WHERE action = 'query' AND query_text IS NOT NULL
		   AND created_at > now() - make_interval(days => $1)
		   AND metadata->>'source' IS DISTINCT FROM 'armsweep'
		 ORDER BY created_at DESC`,
		logWindowDays,
	)
	if err != nil {
		return nil, fmt.Errorf("recall: log stream: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	var vecs [][]float32
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("recall: scan query_text: %w", err)
		}
		if _, dup := seen[text]; dup {
			continue
		}
		seen[text] = struct{}{}
		vec, hit, err := embedcache.PeekByHash(ctx, pool, embedcache.HashKey(embed.PrefixQuery, text), model)
		if err != nil {
			return nil, fmt.Errorf("recall: peek %q-class hash: %w", "query", err)
		}
		if hit {
			vecs = append(vecs, vec)
		}
		if len(vecs) >= limit || len(seen) >= logMaxDistinct {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall: log rows: %w", err)
	}
	return vecs, nil
}

// LooSample is one leave-one-out probe input: the block's own document
// embedding serves as the query vector, the block ID is excluded from both
// legs (id != $self). Asymmetry note (§4.2.2, documented, no blocker): loo
// probes use document-prefix vectors, not query-prefix — irrelevant for the
// ANN-vs-exact DIFFERENCE, because both legs get the same vector; recall
// measures index fidelity, not embedding quality.
type LooSample struct {
	ID  string
	Vec []float32
}

// SampleLOO draws up to n deterministic leave-one-out samples from the scope
// window: n boundary UUIDs interpolated between min(id) and max(id) in
// uuidv7 space (the uuidv7 timestamp prefix spreads boundaries over insert
// time), each resolved to the first matching row at or after the boundary,
// duplicates filled by a forward keyset walk. Deliberately NOT
// `TABLESAMPLE SYSTEM` (samples pages of the WHOLE table before the scope
// filter — a 1% scope in a 10M table would discard ~99% of read pages) and
// never `ORDER BY random()` (full sort per run). Deterministic for a given
// corpus, wire-free, scope-local (§4.2.2).
func SampleLOO(ctx context.Context, pool *pgxpool.Pool, scopes, visibleTypes []string, n int) ([]LooSample, error) {
	if n <= 0 {
		return nil, nil
	}
	// Window endpoints via ORDER BY (PostgreSQL has no min/max aggregate for
	// uuid); ErrNoRows means an empty window.
	endpoint := func(order string) (*string, error) {
		var id string
		err := pool.QueryRow(ctx,
			`SELECT id::text FROM context_blocks
			 WHERE NOT is_archived AND embedding IS NOT NULL
			   AND scope = ANY($1) AND type_name = ANY($2)
			 ORDER BY id `+order+` LIMIT 1`,
			scopes, visibleTypes,
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("recall: loo window endpoint: %w", err)
		}
		return &id, nil
	}
	minID, err := endpoint("ASC")
	if err != nil {
		return nil, err
	}
	maxID, err := endpoint("DESC")
	if err != nil {
		return nil, err
	}
	if minID == nil || maxID == nil {
		return nil, nil // empty window
	}

	bounds, err := interpolateUUIDs(*minID, *maxID, n)
	if err != nil {
		return nil, err
	}

	pick := func(cmpSQL, boundary string) (*LooSample, error) {
		var id string
		var v pgvec.Vector
		err := pool.QueryRow(ctx,
			`SELECT id::text, embedding FROM context_blocks
			 WHERE NOT is_archived AND embedding IS NOT NULL
			   AND scope = ANY($1) AND type_name = ANY($2) AND id `+cmpSQL+` $3::uuid
			 ORDER BY id LIMIT 1`,
			scopes, visibleTypes, boundary,
		).Scan(&id, &v)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("recall: loo pick: %w", err)
		}
		return &LooSample{ID: id, Vec: v.Slice()}, nil
	}

	picked := make(map[string]struct{})
	var out []LooSample
	for _, b := range bounds {
		s, err := pick(">=", b)
		if err != nil {
			return nil, err
		}
		if s == nil {
			continue
		}
		if _, dup := picked[s.ID]; dup {
			continue
		}
		picked[s.ID] = struct{}{}
		out = append(out, *s)
		if len(out) >= n {
			break
		}
	}
	// Boundary clustering left gaps: walk forward from the last pick.
	last := *minID
	if len(out) > 0 {
		last = out[len(out)-1].ID
	}
	for len(out) < n {
		s, err := pick(">", last)
		if err != nil {
			return nil, err
		}
		if s == nil {
			break // window exhausted — fewer than n rows exist
		}
		last = s.ID
		if _, dup := picked[s.ID]; dup {
			continue
		}
		picked[s.ID] = struct{}{}
		out = append(out, *s)
	}
	return out, nil
}

// interpolateUUIDs returns n boundary UUIDs linearly interpolated between lo
// and hi on the first 8 bytes (uuidv7: 48-bit ms timestamp + version nibble —
// monotone, so the prefix interpolation spreads over insert time). The
// remaining 8 bytes are zeroed; boundary i=0 equals lo's prefix, so the first
// pick lands on the window's first row.
func interpolateUUIDs(lo, hi string, n int) ([]string, error) {
	lu, err := uuid.Parse(lo)
	if err != nil {
		return nil, fmt.Errorf("recall: parse loo min id: %w", err)
	}
	hu, err := uuid.Parse(hi)
	if err != nil {
		return nil, fmt.Errorf("recall: parse loo max id: %w", err)
	}
	loN := binary.BigEndian.Uint64(lu[:8])
	hiN := binary.BigEndian.Uint64(hu[:8])
	if hiN < loN {
		loN, hiN = hiN, loN
	}
	span := hiN - loN
	bounds := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var step uint64
		if n > 1 {
			step = span / uint64(n) * uint64(i)
		}
		var b uuid.UUID
		binary.BigEndian.PutUint64(b[:8], loN+step)
		bounds = append(bounds, b.String())
	}
	return bounds, nil
}
