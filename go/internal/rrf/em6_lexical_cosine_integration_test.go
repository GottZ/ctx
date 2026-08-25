//go:build integration

// E-M6 premise gate. The semantic floor's rescue clause (handler/
// semantic_floor.go) rests on one property of ctx_rrf: a block that only the
// lexical arms found comes back with cosine_sim NULL. That is not an
// implementation detail the gate may assume — it is the whole reason exact
// identifiers survive a floor that their embedding distance would not.
//
// The claim is readable in the SQL (the rrf CTE FULL OUTER JOINs the four arms
// and projects s.cos_sim, so a row the semantic arm never produced has no
// similarity), but "readable in the SQL" is how a rescue clause quietly stops
// working. This pins it against the real function.
//
//	go test -tags=integration ./internal/rrf/ -run TestEM6 -count=1 -v
package rrf_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

// em6ConfidentThreshold is the query.confident_threshold default the rescue
// clause compares a lexical-only hit against.
const em6ConfidentThreshold = 0.008

// em6Insert seeds one block with an explicit title/content pair, because the
// lexical arms are what this fixture drives — the shared w021Insert writes a
// fixed vocabulary and cannot separate the matching block from the rest.
func em6Insert(t *testing.T, pool *pgxpool.Pool, id, scope, title, content string, emb []float32) {
	t.Helper()
	var embParam interface{}
	if emb != nil {
		embParam = pgvec.NewVector(emb)
	}
	ts := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, type_name)
		 VALUES ($1::uuid, 'knowledge', $2, $3, $4, $5, $6, $6, 'knowledge')`,
		id, title, content, scope, embParam, ts,
	); err != nil {
		t.Fatalf("insert em6 block %s: %v", id, err)
	}
}

// TestEM6LexicalOnlyHitCarriesNoCosine seeds a corpus in which exactly one
// block matches the query text lexically and exactly that block has no
// embedding — the shape of a bare identifier lookup, where the embedding says
// nothing and the literal match says everything.
func TestEM6LexicalOnlyHitCarriesNoCosine(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		scope     = "em6lex"
		lexID     = "019fa403-0000-7000-9000-000000004001"
		queryText = "quuxterm"
	)

	// Three embedded blocks that share no vocabulary with the query: they can
	// only enter through the semantic arm, and must therefore all carry a
	// cosine similarity.
	for i := 0; i < 3; i++ {
		n := string(rune('1' + i))
		em6Insert(t, pool, "019fa403-0000-7000-9000-00000000300"+n,
			scope, "em6 alpha bravo "+n, "em6 alpha bravo charlie", w021Embedding(i))
	}
	// The lexical-only block: no embedding at all, so Generation 16 excludes it
	// from BOTH semantic arms, and the query term is in title and content.
	em6Insert(t, pool, lexID, scope, queryText+" identifier", queryText+" identifier body", nil)

	rows, err := em6Call(ctx, pool, w021Query(), queryText, scope)
	if err != nil {
		t.Fatalf("ctx_rrf: %v", err)
	}

	var lex *g15Row
	for i := range rows {
		r := &rows[i]
		if r.id == lexID {
			lex = r
			continue
		}
		if r.cos == nil {
			t.Errorf("embedded block %s came back with a NULL cosine — the semantic arm is not the only "+
				"path that produces one, and the rescue clause would read it as lexical evidence", r.id)
		}
	}
	if lex == nil {
		t.Fatalf("the unembedded, lexically matching block never entered the result set (%d rows) — "+
			"fixture broken, the gate below would be vacuous", len(rows))
	}
	if lex.cos != nil {
		t.Errorf("lexical-only hit carries cosine_sim %v, want NULL — E-M6's rescue clause identifies a "+
			"lexical hit by exactly this absence", *lex.cos)
	}
	t.Logf("lexical-only hit: rrf_score=%g (confident_threshold %g)", lex.score, em6ConfidentThreshold)
	if lex.score < em6ConfidentThreshold {
		t.Errorf("lexical-only hit scored %g, below the %g the rescue clause requires — a block that tops "+
			"every lexical arm must clear it, or the clause can never fire for the exact-identifier case "+
			"it exists for", lex.score, em6ConfidentThreshold)
	}
}

// em6Call is g15Call with a caller-chosen query text (g15Call pins 'zzqqxx',
// which is deliberate there and useless here).
func em6Call(ctx context.Context, q g15Querier, emb []float32, queryText, scope string) ([]g15Row, error) {
	rows, err := q.Query(ctx,
		`SELECT id, rrf_score, cosine_sim FROM ctx_rrf($1, $2, $2, $3::text[],
			p_limit => 50,
			p_types_visible => $4::text[],
			p_semantic_mode => 'ann')`,
		pgvNewHalfVec(emb), queryText, []string{scope}, testVisibleTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []g15Row
	for rows.Next() {
		var r g15Row
		if err := rows.Scan(&r.id, &r.score, &r.cos); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
