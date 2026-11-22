//go:build integration

package settings_test

// Issue #483 point 12: Reload loads the override rows, builds the snapshot
// — resolving each secret_ref through the sealbox on the reload's context —
// and replaces the active one. A context that ends after the rows were read
// cuts every secret resolution short; config.Build turns each failure into a
// WARN and keeps the env/default value, so a snapshot built on an ended
// context can lack every secret the rows name. Reload keeps the active
// snapshot then. No registry key is secret-class today (config/synthreg_test.go
// carries the class on an injected entry), so the test ends the context at
// the moment a secret would be resolved: right after the override read
// (a pgx tracer on the pool cancels it when that statement ends) — the
// production path is unchanged.
//
//	go test -tags=integration ./internal/settings/ -run 'TestReloadKeepsTheSnapshot' -count=1 -v

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/testdb"
)

// cancelAfter is a pgx tracer that cancels a context when a statement whose
// text contains match has ended (for a query: when its rows are closed).
type cancelAfter struct {
	match  string
	cancel context.CancelFunc
}

type cancelAfterSQL struct{}

func (c cancelAfter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, cancelAfterSQL{}, d.SQL)
}

func (c cancelAfter) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if sql, _ := ctx.Value(cancelAfterSQL{}).(string); strings.Contains(sql, c.match) {
		c.cancel()
	}
}

func TestReloadKeepsTheSnapshotWhenItsContextEndsAfterTheRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	resetEnvIT(t)
	t.Setenv("CTX_DREAM_LANGUAGE", "de")
	upsertIT(t, pool, "dream.language", `"pt-br"`)
	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.Tracer = cancelAfter{match: "FROM context_settings", cancel: cancel}
	cut, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cut.Close()

	st := config.NewStore(envCfg)
	err = settings.Reload(ctx, cut, st)
	if ctx.Err() == nil {
		t.Fatal("the override read never ended the context — the probe tested nothing")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Reload on a context that ended after the rows: err = %v, want the cancellation", err)
	}
	if snap := st.Snapshot(); snap != envCfg || snap.Dream.Language != "de" {
		t.Errorf("Reload replaced the snapshot after its context ended: dream.language = %q (source %q), want the active env snapshot",
			snap.Dream.Language, snap.Source("dream.language"))
	}

	// Control: the same reload on a live context applies the row.
	if err := settings.Reload(context.Background(), pool, st); err != nil {
		t.Fatalf("control reload: %v", err)
	}
	if snap := st.Snapshot(); snap.Dream.Language != "pt-br" {
		t.Errorf("control reload: dream.language = %q, want pt-br", snap.Dream.Language)
	}
}
