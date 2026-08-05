//go:build integration

// W5 DB gates — the deterministic fallback label and the drift materialization
// (design/01 §7 W5, §4.6; decisions E4-01/E5-01).
//
// The wave's promise is a guarantee, not a feature: after every rebuild EVERY
// living topic carries a non-empty label, without a single LLM call. The gates
// below therefore probe the two ways that guarantee can break — a stage that
// yields nothing (G1) and a stage that yields too much (G1b) — plus the three
// state transitions the label row has to survive: a stronger source above it
// (G3), two identical runs (G4) and a drifting core (G5).
//
// Fixtures reuse the W3 helpers: same package, same hand-built clustering, one
// scope per subtest.
package overview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// w5Tag puts one tag on every listed block.
func w5Tag(t *testing.T, pool *pgxpool.Pool, ids []string, tag string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET tags = ARRAY[$2::text] WHERE id = ANY($1::uuid[])`, ids, tag); err != nil {
		t.Fatalf("w5Tag(%s): %v", tag, err)
	}
}

// w5Label reads the label row of the topic a member currently sits in.
type w5Row struct {
	label     string
	source    string
	stale     bool
	attempts  int
	coreHash  string
	builtAtNs int64
}

func w5LabelOf(t *testing.T, pool *pgxpool.Pool, scope, member string) w5Row {
	t.Helper()
	var r w5Row
	var built *time.Time
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(t.label, ''), t.label_source, t.label_stale, t.label_attempts,
		       COALESCE(n.core_hash, ''), t.label_built_at
		  FROM graph_cluster_member m
		  JOIN graph_cluster_node  n ON n.cluster_id = m.cluster_id AND n.scope = m.scope
		  JOIN graph_cluster_topic t ON t.topic_id   = n.topic_id
		 WHERE m.block_id = $1::uuid AND m.scope = $2`, member, scope).
		Scan(&r.label, &r.source, &r.stale, &r.attempts, &r.coreHash, &built)
	if err != nil {
		t.Fatalf("w5LabelOf(%s, %s): %v", scope, member, err)
	}
	if built != nil {
		r.builtAtNs = built.UnixNano()
	}
	return r
}

// w5Unlabelled counts living topics of one scope without a usable label — the
// number the guarantee says is always zero.
func w5Unlabelled(t *testing.T, pool *pgxpool.Pool, scope string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)::int FROM graph_cluster_topic
		 WHERE scope = $1 AND retired_at IS NULL
		   AND (label IS NULL OR btrim(label) = '')`, scope).Scan(&n); err != nil {
		t.Fatalf("w5Unlabelled(%s): %v", scope, err)
	}
	return n
}

// w5PersistErr runs one generation and RETURNS the error instead of failing —
// the red probes need the SQLSTATE, not a dead test binary.
func w5PersistErr(pool *pgxpool.Pool, scope string, retention time.Duration, groups ...w3Group) error {
	assign, scopes, deg := map[string]string{}, map[string]string{}, map[string]float64{}
	for _, g := range groups {
		cluster := ""
		for _, m := range g.members {
			if cluster == "" || m < cluster {
				cluster = m
			}
		}
		for i, m := range g.members {
			assign[m], scopes[m], deg[m] = cluster, scope, 1
			if g.degrees != nil {
				deg[m] = g.degrees[i]
			}
		}
	}
	_, err := persist(context.Background(), pool,
		clustering{blockToCluster: assign, intraDegree: deg, clusterCount: len(groups)},
		Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope}, TombstoneRetention: retention},
		scopes, tallyScopes(scopes))
	return err
}

// ─────────────────────────────────────────────────────────────────────────────

func TestW5FallbackLabel(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const retention = 45 * 24 * time.Hour

	// ── G1: completeness. Every living topic carries a label after the run.
	//
	// RED against the pre-W5 state: nothing writes graph_cluster_topic.label,
	// so every living topic counts as unlabelled and the map is empty text.
	t.Run("G1 every living topic is labelled", func(t *testing.T) {
		const scope = "w5g1"
		ids := w3Blocks(t, pool, scope, 10000, 6)
		w5Tag(t, pool, ids, "retrieval")
		w3Run(t, pool, scope, retention, w3Group{members: ids[:3]}, w3Group{members: ids[3:]})

		if n := w5Unlabelled(t, pool, scope); n != 0 {
			t.Fatalf("%d living topics without a label — the map would render empty", n)
		}
	})

	// RED PROBE for G1: a variant that keeps ONLY the tag stage. Against a
	// fixture without tags the COALESCE collapses to NULL, label_source is
	// still set to 'fallback', and gct_label_present breaks with 23514 —
	// INSIDE the persist tx, so the whole rebuild rolls back and the map
	// freezes. That is the failure mode the three-stage cascade prevents.
	t.Run("G1 red probe — tag stage alone ⇒ 23514 and a rolled-back rebuild", func(t *testing.T) {
		const scope = "w5g1red"
		ids := w3Blocks(t, pool, scope, 10100, 4) // no tags

		restore := fallbackLabelTemplate
		fallbackLabelTemplate = strings.NewReplacer(
			fallbackCategoryStage, "NULL",
			fallbackTitleStage, "NULL",
			"'"+fallbackLastResort+"'", "NULL",
		).Replace(fallbackLabelTemplate)
		defer func() { fallbackLabelTemplate = restore }()
		if fallbackLabelTemplate == restore {
			t.Fatal("red probe did not patch the template — the stage constants drifted")
		}

		err := w5PersistErr(pool, scope, retention, w3Group{members: ids})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("tag stage alone: err=%v, want SQLSTATE 23514 (gct_label_present)", err)
		}
	})

	// ── G1b: length capping. A single overlong tag on a single core block
	// must not be able to freeze every future rebuild.
	t.Run("G1b a 200-character tag is capped, not fatal", func(t *testing.T) {
		const scope = "w5g1b"
		ids := w3Blocks(t, pool, scope, 10200, 3)
		w5Tag(t, pool, ids, strings.Repeat("x", 200))
		w3Run(t, pool, scope, retention, w3Group{members: ids})

		got := w5LabelOf(t, pool, scope, ids[0])
		if n := len([]rune(got.label)); n != 120 {
			t.Fatalf("label is %d runes, want exactly the 120-rune cap: %q", n, got.label)
		}
	})

	// RED PROBE for G1b: the Revision-1 shape, which capped only the
	// representative-title stage and let tags through unbounded ⇒ 23514 on
	// gct_label_len, again inside the persist tx.
	t.Run("G1b red probe — capping only the title stage ⇒ 23514", func(t *testing.T) {
		const scope = "w5g1bred"
		ids := w3Blocks(t, pool, scope, 10300, 3)
		w5Tag(t, pool, ids, strings.Repeat("y", 200))

		restore := fallbackLabelTemplate
		fallbackLabelTemplate = strings.NewReplacer(
			"btrim(left(btrim(regexp_replace(COALESCE(", "(COALESCE(",
			"), '\\s+', ' ', 'g')), 120))", "))",
		).Replace(fallbackLabelTemplate)
		defer func() { fallbackLabelTemplate = restore }()
		if fallbackLabelTemplate == restore {
			t.Fatal("red probe did not patch the template — the capping shape drifted")
		}

		err := w5PersistErr(pool, scope, retention, w3Group{members: ids})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("uncapped tag stage: err=%v, want SQLSTATE 23514 (gct_label_len)", err)
		}
	})

	// ── G2: the stage order. Tags beat categories, categories beat the
	// representative title, and a whitespace-only tag is NOT a tag.
	t.Run("G2 stage order", func(t *testing.T) {
		for _, tc := range []struct {
			name, scope, tag, want string
			first                  int
		}{
			{"A tags win", "w5g2a", "rrf-tuning", "rrf-tuning", 10400},
			{"B no tags ⇒ category", "w5g2b", "", "learnings", 10500},
			{"C blank tags ⇒ category", "w5g2c", "   ", "learnings", 10600},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ids := w3Blocks(t, pool, tc.scope, tc.first, 3)
				if tc.tag != "" {
					w5Tag(t, pool, ids, tc.tag)
				}
				w3Run(t, pool, tc.scope, retention, w3Group{members: ids})
				if got := w5LabelOf(t, pool, tc.scope, ids[0]); got.label != tc.want {
					t.Fatalf("label = %q, want %q", got.label, tc.want)
				}
			})
		}
	})

	// ── G3: a stronger source is never overwritten — but its drift flag is
	// still maintained. That split is the whole point of moving the
	// label_source filter from the WHERE into the CASE arms.
	t.Run("G3 llm labels survive a rebuild, their drift flag does not", func(t *testing.T) {
		const scope = "w5g3"
		ids := w3Blocks(t, pool, scope, 10700, 3)
		w5Tag(t, pool, ids, "before")
		w3Run(t, pool, scope, retention, w3Group{members: ids, degrees: []float64{3, 1, 1}})

		topic := w3TopicOf(t, pool, scope, ids[0])
		if _, err := pool.Exec(context.Background(), `
			UPDATE graph_cluster_topic
			   SET label = 'human-grade name', label_source = 'llm',
			       label_built_at = now(), label_core_hash = 'stale-anchor'
			 WHERE topic_id = $1::uuid`, topic); err != nil {
			t.Fatalf("seed llm label: %v", err)
		}
		w5Tag(t, pool, ids, "after")
		w3Run(t, pool, scope, retention, w3Group{members: ids, degrees: []float64{3, 1, 1}})

		got := w5LabelOf(t, pool, scope, ids[0])
		if got.label != "human-grade name" || got.source != "llm" {
			t.Fatalf("llm label overwritten: %q/%s", got.label, got.source)
		}
		if !got.stale {
			t.Fatal("label_stale not maintained for an llm row — W6 would never re-label it")
		}
	})

	// RED PROBE for G3: drop the CASE guard, i.e. write the fallback
	// unconditionally. The hand-written label is gone.
	t.Run("G3 red probe — unconditional write clobbers the llm label", func(t *testing.T) {
		const scope = "w5g3red"
		ids := w3Blocks(t, pool, scope, 10800, 3)
		w5Tag(t, pool, ids, "before")
		w3Run(t, pool, scope, retention, w3Group{members: ids})

		topic := w3TopicOf(t, pool, scope, ids[0])
		if _, err := pool.Exec(context.Background(), `
			UPDATE graph_cluster_topic
			   SET label = 'human-grade name', label_source = 'llm', label_built_at = now()
			 WHERE topic_id = $1::uuid`, topic); err != nil {
			t.Fatalf("seed llm label: %v", err)
		}

		restore := fallbackLabelTemplate
		fallbackLabelTemplate = strings.NewReplacer(
			"CASE WHEN t.label_source IN ('none','fallback') THEN f.fallback_label ELSE t.label END", "f.fallback_label",
			"CASE WHEN t.label_source IN ('none','fallback') THEN 'fallback' ELSE t.label_source END", "'fallback'",
		).Replace(fallbackLabelTemplate)
		defer func() { fallbackLabelTemplate = restore }()
		if fallbackLabelTemplate == restore {
			t.Fatal("red probe did not patch the template — the CASE shape drifted")
		}

		if err := w5PersistErr(pool, scope, retention, w3Group{members: ids}); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if got := w5LabelOf(t, pool, scope, ids[0]); got.source == "llm" {
			t.Fatal("red probe stayed green — the CASE guard is not what protects the llm label")
		}
	})

	// ── G4: determinism. Two runs over an unchanged corpus, byte-identical
	// label. The tag aggregation orders by (count DESC, tag) for exactly this.
	t.Run("G4 two runs produce the same label", func(t *testing.T) {
		const scope = "w5g4"
		ids := w3Blocks(t, pool, scope, 10900, 4)
		if _, err := pool.Exec(context.Background(),
			`UPDATE context_blocks SET tags = ARRAY['beta','alpha','gamma'] WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Fatalf("seed tags: %v", err)
		}
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		first := w5LabelOf(t, pool, scope, ids[0]).label
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		if second := w5LabelOf(t, pool, scope, ids[0]).label; second != first {
			t.Fatalf("label drifted between identical runs: %q → %q", first, second)
		}
		if first != "alpha · beta · gamma" {
			t.Fatalf("tag aggregation lost its tiebreak order: %q", first)
		}
	})

	// ── G5: drift flag and attempt-counter reset. A drifted core is a NEW
	// input, so the three-strikes counter starts over — otherwise a topic that
	// failed three times stays unlabelled forever, even after its content
	// changed completely.
	t.Run("G5 drift materialization and attempts reset", func(t *testing.T) {
		const scope = "w5g5"
		ids := w3Blocks(t, pool, scope, 11000, 3)
		w5Tag(t, pool, ids, "core")
		w3Run(t, pool, scope, retention, w3Group{members: ids, degrees: []float64{3, 1, 1}})

		topic := w3TopicOf(t, pool, scope, ids[0])
		hash := w5LabelOf(t, pool, scope, ids[0]).coreHash
		if _, err := pool.Exec(context.Background(), `
			UPDATE graph_cluster_topic
			   SET label = 'pinned by llm', label_source = 'llm', label_built_at = now(),
			       label_core_hash = $2, label_attempts = 3
			 WHERE topic_id = $1::uuid`, topic, hash); err != nil {
			t.Fatalf("seed llm label: %v", err)
		}

		// (a) unchanged core ⇒ not stale, counter untouched.
		w3Run(t, pool, scope, retention, w3Group{members: ids, degrees: []float64{3, 1, 1}})
		if got := w5LabelOf(t, pool, scope, ids[0]); got.stale || got.attempts != 3 {
			t.Fatalf("unchanged core: stale=%v attempts=%d, want false/3", got.stale, got.attempts)
		}

		// (b) shift the substance to another member ⇒ new core, new hash.
		w3Run(t, pool, scope, retention, w3Group{members: ids, degrees: []float64{1, 1, 3}})
		got := w5LabelOf(t, pool, scope, ids[0])
		if !got.stale || got.attempts != 0 {
			t.Fatalf("drifted core: stale=%v attempts=%d, want true/0", got.stale, got.attempts)
		}
		if got.coreHash == hash {
			t.Fatal("fixture did not actually move the core — the gate would pass vacuously")
		}
	})

	// RED PROBE for G5: without the attempts CASE the counter survives the
	// drift and the topic is locked out of the W6 selection forever.
	t.Run("G5 red probe — no reset ⇒ permanent lockout", func(t *testing.T) {
		const scope = "w5g5red"
		ids := w3Blocks(t, pool, scope, 11100, 3)
		w5Tag(t, pool, ids, "core")
		w3Run(t, pool, scope, retention, w3Group{members: ids, degrees: []float64{3, 1, 1}})

		topic := w3TopicOf(t, pool, scope, ids[0])
		hash := w5LabelOf(t, pool, scope, ids[0]).coreHash
		if _, err := pool.Exec(context.Background(), `
			UPDATE graph_cluster_topic
			   SET label = 'pinned by llm', label_source = 'llm', label_built_at = now(),
			       label_core_hash = $2, label_attempts = 3
			 WHERE topic_id = $1::uuid`, topic, hash); err != nil {
			t.Fatalf("seed llm label: %v", err)
		}

		restore := fallbackLabelTemplate
		fallbackLabelTemplate = strings.Replace(fallbackLabelTemplate,
			"label_attempts = CASE WHEN t.label_core_hash IS DISTINCT FROM f.core_hash THEN 0 ELSE t.label_attempts END",
			"label_attempts = t.label_attempts", 1)
		defer func() { fallbackLabelTemplate = restore }()
		if fallbackLabelTemplate == restore {
			t.Fatal("red probe did not patch the template — the reset shape drifted")
		}

		if err := w5PersistErr(pool, scope, retention, w3Group{members: ids, degrees: []float64{1, 1, 3}}); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if got := w5LabelOf(t, pool, scope, ids[0]); got.attempts == 0 {
			t.Fatal("red probe stayed green — the CASE is not what resets the counter")
		}
	})

	// ── G6: the guarantee survives the degenerate group — no tags, blank
	// category-free title. The last-resort stage exists so a CHECK violation
	// can never turn a display gap into a frozen map.
	t.Run("G6 last resort keeps a title-less group labelled", func(t *testing.T) {
		// ONE block: (category, title, scope) is unique, and a fixture that
		// blanks the title of two blocks in one scope collides on that index
		// before it ever reaches the label.
		const scope = "w5g6"
		ids := w3Blocks(t, pool, scope, 11200, 1)
		if _, err := pool.Exec(context.Background(),
			`UPDATE context_blocks SET title = '   ', tags = '{}' WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Fatalf("blank the titles: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`UPDATE context_blocks SET category = ' ' WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Fatalf("blank the category: %v", err)
		}
		w3Run(t, pool, scope, retention, w3Group{members: ids})

		got := w5LabelOf(t, pool, scope, ids[0])
		if got.label != fallbackLastResort {
			t.Fatalf("label = %q, want the last-resort constant %q", got.label, fallbackLastResort)
		}
		if n := w5Unlabelled(t, pool, scope); n != 0 {
			t.Fatalf("%d unlabelled topics despite the last-resort stage", n)
		}
	})
}

// TestW5FallbackLabelGlobalRun pins the second run shape: a rebuild without a
// ScopeFilter has to label too. The scoped variant carries a WHERE the global
// one must not have — a copy that kept it would break with 42P02.
func TestW5FallbackLabelGlobalRun(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const scope = "w5global"
	ids := w3Blocks(t, pool, scope, 12000, 3)
	w5Tag(t, pool, ids, "global-run")

	assign, scopes, deg := map[string]string{}, map[string]string{}, map[string]float64{}
	for _, m := range ids {
		assign[m], scopes[m], deg[m] = ids[0], scope, 1
	}
	if _, err := persist(context.Background(), pool,
		clustering{blockToCluster: assign, intraDegree: deg, clusterCount: 1},
		Options{Resolution: 1.0, VisibleTypes: w3Types},
		scopes, tallyScopes(scopes)); err != nil {
		t.Fatalf("global persist: %v", err)
	}
	if got := w5LabelOf(t, pool, scope, ids[0]); got.label != "global-run" || got.source != "fallback" {
		t.Fatalf("global run label = %q/%s, want %q/fallback", got.label, got.source, "global-run")
	}
	if n := w5Unlabelled(t, pool, scope); n != 0 {
		t.Fatalf("%d unlabelled topics after the global run", n)
	}
}
