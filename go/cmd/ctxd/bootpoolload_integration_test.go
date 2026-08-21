//go:build integration

// The boot seam around the initial backend-pool read against a real PG18
// testcontainer:
//
//	go test -tags=integration ./cmd/ctxd/ -run TestBootLoadBackendPoolDegraded -count=1 -v
//
// The unit half (cmd/ctxd/bootpoolload_test.go) proves the guard on injected
// seams; this half proves the CONSEQUENCE the guard exists for, through the
// production reconcile (events.ReconcileCoupledFingerprint), the production
// meta row (migration 132) and a real context_embed_cache. It lives here rather
// than in internal/events because the guard itself lives here — events sees
// only a pool handed to it, and cannot tell a pool that read an empty table
// from one that never read at all.
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

const bootSeamEmbedModel = "boot-seam-embed-model"

// bootSeamSeedBackend inserts one enabled, global-scoped embed backend through
// the production CreateBackend path — a coupled pair the fingerprint can see.
func bootSeamSeedBackend(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &backends.Backend{
		Name: "boot-seam-embed", Host: "http://127.0.0.1:11434",
		Protocol: backends.ProtocolOllama, ProviderClass: backends.ProviderGeneric,
		Trust: backends.TrustFull, Locality: backends.LocalityLocal,
		Roles:    []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: bootSeamEmbedModel}},
		Priority: 70, Enabled: true, Scope: backends.GlobalScope,
	}
	if _, err := store.CreateBackend(ctx, tx, b, nil); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit backend: %v", err)
	}
}

// bootSeamSeedCache puts one row into context_embed_cache — the thing a flush
// is visible in.
func bootSeamSeedCache(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, 'boot seam probe')`,
		[]byte("boot-seam-probe-hash"), bootSeamEmbedModel, pgvec.NewVector(make([]float32, 1024))); err != nil {
		t.Fatalf("seed embed cache: %v", err)
	}
}

func bootSeamCacheCount(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM context_embed_cache`).Scan(&n); err != nil {
		t.Fatalf("count embed cache: %v", err)
	}
	return n
}

// bootSeamStamp reads the fingerprint on record plus the pair count that goes
// with it. The pair count is what makes the empty-set stamp RECOGNIZABLE: the
// empty coupled set hashes to a perfectly valid digest, and only `0 pairs`
// tells it apart from a topology that was actually read.
func bootSeamStamp(t *testing.T, db *pgxpool.Pool) (string, int) {
	t.Helper()
	var fp *string
	var pairs *int
	err := db.QueryRow(context.Background(),
		`SELECT coupled_fingerprint, coupled_pair_n FROM context_embed_cache_meta WHERE singleton`).Scan(&fp, &pairs)
	if err != nil {
		return "", -1 // no row yet = never stamped
	}
	if fp == nil || pairs == nil {
		return "", -1
	}
	return *fp, *pairs
}

// bootSeamRun drives the production boot seam with the production reconcile and
// the given reload outcome.
func bootSeamRun(t *testing.T, db *pgxpool.Pool, reloadErr error) {
	t.Helper()
	bp := backends.NewPool(db, nil)
	reload := bp.Reload
	if reloadErr != nil {
		// A transient DB failure at boot: the pool keeps the empty NewPool
		// snapshot, exactly as pool.go leaves it on a failed read.
		reload = func(context.Context) error { return reloadErr }
	}
	bootLoadBackendPool(context.Background(), bp, reload, func(ctx context.Context) error {
		return events.ReconcileCoupledFingerprint(ctx, db, bp)
	})
}

// TestBootLoadBackendPoolDegradedBootKeepsCache is the reason the guard exists.
// A boot whose pool read failed holds the empty NewPool snapshot; its coupled
// set is the EMPTY set, which is a legitimate fingerprint value rather than an
// error — so an unguarded reconcile reads a mismatch against the stand on
// record, flushes context_embed_cache whole and stamps the empty set. The next
// healthy boot then mismatches against THAT stamp and flushes a second time.
// Two cold-cache spikes out of a read that never happened.
//
// The three boots below are that scenario end to end: healthy (stamp seeded,
// E12 without flush), degraded (nothing may move), healthy again (still quiet).
//
// Mutation probe: drop the `return` in bootLoadBackendPool's error arm — the
// degraded boot empties the cache and stamps `0 pairs`, and the third boot
// flushes the already-empty cache a second time; both halves go red.
func TestBootLoadBackendPoolDegradedBootKeepsCache(t *testing.T) {
	db := testdb.SetupTestDB(t)
	bootSeamSeedBackend(t, db)

	// Boot 1, healthy: E12 variant (b) — the first boot stamps, it does not flush.
	bootSeamRun(t, db, nil)
	fp, pairs := bootSeamStamp(t, db)
	if fp == "" || pairs != 1 {
		t.Fatalf("stamp after the first healthy boot = (%q, %d pairs), want a digest over 1 pair", fp, pairs)
	}
	bootSeamSeedCache(t, db)

	// Boot 2, degraded: the reload fails. Nothing was read, so nothing may be
	// diffed, flushed or stamped.
	bootSeamRun(t, db, errors.New("dial tcp: connection refused"))
	if n := bootSeamCacheCount(t, db); n != 1 {
		t.Errorf("embed cache rows after a degraded boot = %d, want 1 — a failed read flushed the cache", n)
	}
	gotFP, gotPairs := bootSeamStamp(t, db)
	if gotFP != fp || gotPairs != pairs {
		t.Errorf("stamp after a degraded boot = (%q, %d pairs), want the unchanged (%q, %d pairs) — the empty set was stamped as the truth",
			gotFP, gotPairs, fp, pairs)
	}

	// Boot 3, healthy again: the topology never changed, so this stays quiet.
	// Against an empty-set stamp it would flush — the second spike.
	bootSeamRun(t, db, nil)
	if n := bootSeamCacheCount(t, db); n != 1 {
		t.Errorf("embed cache rows after the recovery boot = %d, want 1 — the degraded boot left a stamp that mismatches the real topology", n)
	}
	if gotFP, gotPairs := bootSeamStamp(t, db); gotFP != fp || gotPairs != pairs {
		t.Errorf("stamp after the recovery boot = (%q, %d pairs), want the unchanged (%q, %d pairs)", gotFP, gotPairs, fp, pairs)
	}
}
