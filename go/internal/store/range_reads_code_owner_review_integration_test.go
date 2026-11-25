//go:build integration

// Issue #536, the code-owner review of the pull request.
//
//   - FullGraph's merge seed was sent as a prepared statement whatever the
//     pool's mode: a pool set to name no statement got one named all the
//     same (and lost it to a server-side DEALLOCATE ALL, 26000), and every
//     connection kept one generic plan per number of scopes and time window.
//     It is sent unnamed now.
//   - A failed index probe failed the issue search and the seed, which only
//     choose between two forms of one statement by it.
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// corSeed writes n blocks into each of the scopes cor-0 … cor-(k-1).
func corSeed(t *testing.T, pool *pgxpool.Pool, k, n int) []string {
	t.Helper()
	scopes := make([]string, k)
	for i := range scopes {
		scopes[i] = fmt.Sprintf("cor-%d", i)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO context_blocks (category, title, content, scope, type_name, workflow_status, created_at, updated_at)
		SELECT 'c', 'issue quokka ' || s || ' ' || g, 'quokka body ' || g, s, 'issue', 'open', now() - g * interval '1 minute', now() - g * interval '1 minute'
		  FROM unnest($1::text[]) s, generate_series(1, $2::int) g`, scopes, n); err != nil {
		t.Fatal(err)
	}
	return scopes
}

// corOneConn is a pool of one connection on dsn with the default query mode.
func corOneConn(t *testing.T, dsn string, mode pgx.QueryExecMode) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.DefaultQueryExecMode = mode
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestFullGraphSeedNamesNoStatement(t *testing.T) {
	admin, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	scopes := corSeed(t, admin, 8, 20)
	seedStatements := func(pool *pgxpool.Pool) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_prepared_statements WHERE statement LIKE '%ORDER BY u.created_at DESC, u.id%'`,
			pgx.QueryExecModeExec).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	read := func(pool *pgxpool.Pool, k int) error {
		res, err := store.FullGraph(ctx, pool, store.EgoParams{Limit: 50, EdgeLimit: 100}, scopes[:k], nil, []string{"issue"})
		if err == nil && len(res.Nodes) == 0 {
			err = fmt.Errorf("no node")
		}
		return err
	}

	t.Run("a pool that names its statements", func(t *testing.T) {
		pool := corOneConn(t, dsn, pgx.QueryExecModeCacheStatement)
		for k := 1; k <= len(scopes); k++ {
			if err := read(pool, k); err != nil {
				t.Fatalf("FullGraph of %d scopes: %v", k, err)
			}
		}
		if n := seedStatements(pool); n != 0 {
			t.Errorf("the connection holds %d prepared seed statements after readers of 1 to %d scopes, want none (one generic plan each)", n, len(scopes))
		}
	})
	t.Run("a pool that names none", func(t *testing.T) {
		pool := corOneConn(t, dsn, pgx.QueryExecModeExec)
		if err := read(pool, 3); err != nil {
			t.Fatalf("FullGraph: %v", err)
		}
		if n := seedStatements(pool); n != 0 {
			t.Errorf("the seed named %d statement(s) on a pool set to name none", n)
		}
		// What a pooler in transaction mode does to a named statement: the
		// next transaction finds another server session.
		if _, err := pool.Exec(ctx, `DEALLOCATE ALL`); err != nil {
			t.Fatal(err)
		}
		if err := read(pool, 3); err != nil {
			t.Errorf("FullGraph after the server forgot the session's statements: %v", err)
		}
	})
}

// A role that may not read pg_index: the probes fail, the reads answer on the
// forms that need no index reading.
func TestReadsAnswerWhenTheirIndexProbeFails(t *testing.T) {
	admin, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	scopes := corSeed(t, admin, 3, 30)
	for _, s := range []string{
		`CREATE ROLE cor_low LOGIN PASSWORD 'cor_low'`,
		`GRANT USAGE ON SCHEMA public TO cor_low`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO cor_low`,
		`REVOKE SELECT ON pg_catalog.pg_index FROM PUBLIC`,
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `GRANT SELECT ON pg_catalog.pg_index TO PUBLIC`)
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "cor_low", "cor_low"
	low, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(low.Close)
	var probe bool
	if err := low.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index LIMIT 1)`).Scan(&probe); err == nil || !strings.Contains(err.Error(), "42501") {
		t.Fatalf("the role reads pg_index (%v): the fixture does not make the probe fail", err)
	}

	rows, _, err := store.SearchIssues(ctx, low, store.IssueReadQuery{Scope: scopes[0], Q: "quokka", Limit: 20})
	if err != nil || len(rows) != 20 {
		t.Errorf("issue search by update with a failing index probe: %d rows, %v; want 20 and no error", len(rows), err)
	}
	res, err := store.FullGraph(ctx, low, store.EgoParams{Limit: 50, EdgeLimit: 100}, scopes, nil, []string{"issue"})
	if err != nil || res == nil || len(res.Nodes) != 50 {
		n := 0
		if res != nil {
			n = len(res.Nodes)
		}
		t.Errorf("FullGraph of 3 scopes with a failing index probe: %d nodes, %v; want 50 and no error", n, err)
	}
	// A request that has ended is still an error, not a fallback.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.SearchIssues(cctx, low, store.IssueReadQuery{Scope: scopes[0], Q: "quokka", Limit: 20}); err == nil {
		t.Error("issue search on a cancelled context answered")
	}
}
