//go:build integration

// Integration coverage for the W01-2 sampling layer (design/01 §4.2.1/§4.2.2):
// stratification (per-scope counts, largest-scope-per-class, pseudo-stratum
// "all", scope_changed stamp), log-query sampling (bounded stream, dedup,
// cache-hits-only) with the gate-(e) touch-freedom assertion, and the
// deterministic loo sampler.
//
// Run with:
//
//	go test -tags=integration ./internal/recall/ -run TestSampling -count=1 -v
package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/testdb"
)

const sampleModel = "qwen3-embedding:test"

func seedCacheEntry(t *testing.T, pool *pgxpool.Pool, text string, seed int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, $4)`,
		embedcache.HashKey(embed.PrefixQuery, text), sampleModel,
		pgvec.NewVector(seededVec(seed)), text)
	if err != nil {
		t.Fatalf("seed cache entry %q: %v", text, err)
	}
}

func seedLogRow(t *testing.T, pool *pgxpool.Pool, action, text string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_access_log (action, query_text, created_at)
		 VALUES ($1, $2, now() - make_interval(secs => $3))`,
		action, text, age.Seconds())
	if err != nil {
		t.Fatalf("seed log row %q: %v", text, err)
	}
}

func TestSampling(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	registry := blocktype.NewRegistry()

	t.Run("Stratification", func(t *testing.T) {
		seedScope(t, pool, "s-small", 2, 100)
		seedScope(t, pool, "s-small2", 3, 200) // same class, larger — must win
		seedScope(t, pool, "s-med", 5, 300)
		seedScope(t, pool, "s-large", 9, 400)

		plans, err := recall.PlanStrata(ctx, pool, registry, 3, 6, map[string]string{"small": "someone-else"})
		if err != nil {
			t.Fatalf("PlanStrata: %v", err)
		}
		byStratum := map[string]recall.StratumPlan{}
		for _, p := range plans {
			byStratum[p.Stratum] = p
		}

		small, ok := byStratum["small"]
		if !ok || small.Scope == nil || *small.Scope != "s-small2" || small.CorpusEmbedded != 3 {
			t.Errorf("small stratum = %+v, want largest scope of the class (s-small2, n=3)", small)
		}
		if ok && !small.ScopeChanged {
			t.Error("small stratum: prevScopes named a different scope — ScopeChanged must be true")
		}
		med, ok := byStratum["medium"]
		if !ok || med.Scope == nil || *med.Scope != "s-med" || med.CorpusEmbedded != 5 {
			t.Errorf("medium stratum = %+v, want s-med n=5", med)
		}
		if ok && med.ScopeChanged {
			t.Error("medium stratum: no previous scope given — ScopeChanged must be false")
		}
		large, ok := byStratum["large"]
		if !ok || large.Scope == nil || *large.Scope != "s-large" || large.CorpusEmbedded != 9 {
			t.Errorf("large stratum = %+v, want s-large n=9", large)
		}
		all, ok := byStratum["all"]
		if !ok || all.Scope != nil {
			t.Fatalf("all stratum = %+v, want present with nil scope", all)
		}
		if all.CorpusEmbedded != 2+3+5+9 {
			t.Errorf("all corpus = %d, want %d (union of all scopes)", all.CorpusEmbedded, 2+3+5+9)
		}
		if len(all.Scopes) != 4 {
			t.Errorf("all scope arm = %v, want the 4 seeded scopes", all.Scopes)
		}
		for _, p := range plans {
			if len(p.VisibleTypes) == 0 {
				t.Errorf("stratum %s has an empty type allowlist", p.Stratum)
			}
		}
	})

	t.Run("LogSamplingTouchFree", func(t *testing.T) {
		// Log rows: q1/q2 cached, q1 duplicated (dedup), q3 uncached (miss —
		// must be skipped, NEVER embedded), one wrong action, one outside
		// the 30d window despite a cache entry.
		seedLogRow(t, pool, "query", "q1 how does the guard work", time.Hour)
		seedLogRow(t, pool, "query", "q1 how does the guard work", 2*time.Hour)
		seedLogRow(t, pool, "query", "q2 what is the rrf policy", 3*time.Hour)
		seedLogRow(t, pool, "query", "q3 uncached question", 4*time.Hour)
		seedLogRow(t, pool, "get", "q4 wrong action", time.Hour)
		seedLogRow(t, pool, "query", "q5 too old", 31*24*time.Hour)
		seedCacheEntry(t, pool, "q1 how does the guard work", 11)
		seedCacheEntry(t, pool, "q2 what is the rrf policy", 12)
		seedCacheEntry(t, pool, "q4 wrong action", 14)
		seedCacheEntry(t, pool, "q5 too old", 15)

		type touch struct {
			hits int
			last time.Time
		}
		snapshot := func() map[string]touch {
			t.Helper()
			rows, err := pool.Query(ctx,
				`SELECT text_preview, hit_count, last_access FROM context_embed_cache WHERE model = $1`,
				sampleModel)
			if err != nil {
				t.Fatalf("snapshot cache: %v", err)
			}
			defer rows.Close()
			out := map[string]touch{}
			for rows.Next() {
				var preview string
				var tc touch
				if err := rows.Scan(&preview, &tc.hits, &tc.last); err != nil {
					t.Fatalf("scan cache row: %v", err)
				}
				out[preview] = tc
			}
			return out
		}

		before := snapshot()
		vecs, err := recall.SampleLogQueries(ctx, pool, sampleModel, 10)
		if err != nil {
			t.Fatalf("SampleLogQueries: %v", err)
		}
		if len(vecs) != 2 {
			t.Fatalf("sampled %d vectors, want 2 (q1+q2: cache hits inside the window with action='query', deduplicated)", len(vecs))
		}
		for i, v := range vecs {
			if len(v) != 1024 {
				t.Errorf("vec %d has %d dims, want 1024", i, len(v))
			}
		}

		// Gate (e), green side: the sampler read the cache without touching
		// hit_count/last_access of ANY entry (the red side against the
		// cacheProbe UPDATE path lives in internal/embedcache).
		after := snapshot()
		for preview, b := range before {
			a, ok := after[preview]
			if !ok {
				t.Errorf("cache entry %q disappeared during sampling", preview)
				continue
			}
			if a.hits != b.hits || !a.last.Equal(b.last) {
				t.Errorf("cache entry %q touched by the sampler: hit_count %d->%d, last_access %v->%v",
					preview, b.hits, a.hits, b.last, a.last)
			}
		}
		t.Logf("sampled %d vectors, %d cache entries untouched", len(vecs), len(before))

		// No model string ⇒ no cache join possible ⇒ empty, never a wire call.
		none, err := recall.SampleLogQueries(ctx, pool, "", 10)
		if err != nil || none != nil {
			t.Errorf("empty model: got %v/%v, want nil/nil", none, err)
		}
	})

	t.Run("LooDeterministic", func(t *testing.T) {
		seedScope(t, pool, "loo-scope", 10, 600)
		visible := []string{"knowledge"}

		s1, err := recall.SampleLOO(ctx, pool, []string{"loo-scope"}, visible, 5)
		if err != nil {
			t.Fatalf("SampleLOO: %v", err)
		}
		if len(s1) != 5 {
			t.Fatalf("sampled %d, want 5", len(s1))
		}
		seen := map[string]struct{}{}
		for _, s := range s1 {
			if _, dup := seen[s.ID]; dup {
				t.Errorf("duplicate loo id %s", s.ID)
			}
			seen[s.ID] = struct{}{}
			if len(s.Vec) != 1024 {
				t.Errorf("loo vec of %s has %d dims", s.ID, len(s.Vec))
			}
		}

		s2, err := recall.SampleLOO(ctx, pool, []string{"loo-scope"}, visible, 5)
		if err != nil {
			t.Fatalf("SampleLOO repeat: %v", err)
		}
		for i := range s1 {
			if s1[i].ID != s2[i].ID {
				t.Errorf("loo sample %d differs between runs: %s vs %s (must be deterministic)", i, s1[i].ID, s2[i].ID)
			}
		}

		// Asking for more than the window holds returns the whole window.
		s3, err := recall.SampleLOO(ctx, pool, []string{"loo-scope"}, visible, 25)
		if err != nil {
			t.Fatalf("SampleLOO overdraw: %v", err)
		}
		if len(s3) != 10 {
			t.Errorf("overdraw sampled %d, want all 10", len(s3))
		}

		// Empty window: no rows, no error.
		s4, err := recall.SampleLOO(ctx, pool, []string{"no-such-scope"}, visible, 5)
		if err != nil || len(s4) != 0 {
			t.Errorf("empty window: got %d/%v, want 0/nil", len(s4), err)
		}
	})
}
