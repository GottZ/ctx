package events

// The persisted half of the embed-cache coupled diff (A04-W4, design/04
// §3.2b). W3 (listener.go) diffs the coupled set after every pool reload in
// the NOTIFY funnel; that funnel only ever sees writes made while ctxd is
// RUNNING. An edit applied by psql during a maintenance window — pool row or
// disable profile, ctxd stopped — produces no notification, and the next boot
// takes its in-memory baseline from exactly that edited pool. The diff is then
// empty by construction and context_embed_cache keeps serving vectors from the
// old host under an unchanged model name (design/04 §5.1 R5).
//
// This file closes that window with the one witness that survives a restart: a
// fingerprint of the coupled set on record in the database (migration 132),
// compared against the freshly loaded boot snapshot.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// coupledFingerprintVersion prefixes the hash input so the DERIVATION itself is
// versioned, not just the data. If a later wave changes what a coupled pair is
// made of, every installation reads a mismatch on the first boot of that
// release and flushes once — the fail-closed direction, and a deliberate one:
// a silently redefined fingerprint that keeps comparing equal would claim a
// coverage it no longer has.
const coupledFingerprintVersion = "v1"

// Field and record separators of the hash input. Explicit non-printable bytes
// rather than a joining character that could occur inside a host: an
// unambiguous encoding is what makes two different sets hash differently.
const (
	coupledFieldSep  = 0x1f
	coupledRecordSep = 0x1e
)

// fingerprint is the deterministic digest of a coupled set: sha256 over the
// version tag followed by the SORTED (host, protocol) pairs. Sorting is what
// makes it a set digest — Go map iteration order is randomized, so hashing in
// iteration order would produce a different value for the same topology on
// every boot and flush the cache on each one.
//
// The empty set has a well-defined digest of its own (the version tag alone),
// distinct from "never stamped" — that state is NULL in the table, never a
// hash value.
func (s coupledSet) fingerprint() string {
	pairs := make([]string, 0, len(s))
	for p := range s {
		pairs = append(pairs, p.host+string(rune(coupledFieldSep))+p.protocol)
	}
	slices.Sort(pairs)

	h := sha256.New()
	h.Write([]byte(coupledFingerprintVersion))
	for _, p := range pairs {
		h.Write([]byte{coupledRecordSep})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// loadCoupledFingerprint reads the fingerprint on record. The second return
// value distinguishes "stamped" from "never stamped" — a missing row and a
// NULL column both mean the latter (migration 132 seeds neither), and that is
// the state E12 / E-A04-4 variant (b) answers with a seed instead of a flush.
func loadCoupledFingerprint(ctx context.Context, db *pgxpool.Pool) (string, bool, error) {
	var fp *string
	err := db.QueryRow(ctx,
		`SELECT coupled_fingerprint FROM context_embed_cache_meta WHERE singleton`).Scan(&fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("events: reading coupled fingerprint: %w", err)
	}
	if fp == nil || *fp == "" {
		return "", false, nil
	}
	return *fp, true, nil
}

// storeCoupledFingerprint stamps the given set as the state on record. Upsert
// on the singleton key: the row may or may not exist yet, and both callers
// (boot reconcile, listener flush) must be able to run first.
func storeCoupledFingerprint(ctx context.Context, db *pgxpool.Pool, s coupledSet) error {
	if _, err := db.Exec(ctx,
		`INSERT INTO context_embed_cache_meta (singleton, coupled_fingerprint, coupled_pair_n, updated_at)
		 VALUES (true, $1, $2, now())
		 ON CONFLICT (singleton) DO UPDATE SET
		     coupled_fingerprint = EXCLUDED.coupled_fingerprint,
		     coupled_pair_n      = EXCLUDED.coupled_pair_n,
		     updated_at          = EXCLUDED.updated_at`,
		s.fingerprint(), len(s)); err != nil {
		return fmt.Errorf("events: persisting coupled fingerprint: %w", err)
	}
	return nil
}

// ReconcileCoupledFingerprint is the boot step (cmd/ctxd/main.go, right after
// the initial pool reload): it compares the coupled set of the loaded pool
// against the fingerprint on record and flushes context_embed_cache when they
// differ — the offline-edit case the NOTIFY funnel structurally cannot see.
//
// Call it AFTER backendPool.Reload and BEFORE the listener starts, so the
// listener's in-memory baseline and the stamp describe the same topology.
//
// It is non-fatal by contract, like the bootstrap and reload steps around it:
// the error is returned for the caller to log, never to abort a boot that
// served queries yesterday. Nothing is stamped on the failing paths, so the
// next boot re-diffs against the un-advanced stand and tries again.
func ReconcileCoupledFingerprint(ctx context.Context, db *pgxpool.Pool, backendPool *backends.Pool) error {
	_, err := reconcileCoupledFingerprint(ctx, db, backendPool, embedcache.Flush)
	return err
}

// reconcileCoupledFingerprint carries the flush behind a seam and reports
// whether it ran — the failure posture (§4.2b: the stamp advances only after a
// SUCCESSFUL flush, so the retry is real rather than a phrase) is only pinnable
// with an injectable error and an observable outcome.
func reconcileCoupledFingerprint(
	ctx context.Context,
	db *pgxpool.Pool,
	backendPool *backends.Pool,
	flush func(ctx context.Context, pool *pgxpool.Pool) (int64, error),
) (bool, error) {
	if db == nil || backendPool == nil {
		return false, nil
	}
	cur := coupledSetOf(backendPool)
	prev, stamped, err := loadCoupledFingerprint(ctx, db)
	if err != nil {
		return false, err
	}

	// E12 / design/04 §8 E-A04-4 variant (b): a first boot SEEDS the stamp and
	// does not flush. Variant (a) would have forced a full cache warmup on
	// every installation of this upgrade to heal the minority that ever moved
	// its embed host; that minority is healed by a documented one-time step
	// instead (docs/operations.md, "Migration 132"), and from this stamp on the
	// offline window is closed deterministically.
	if !stamped {
		if err := storeCoupledFingerprint(ctx, db, cur); err != nil {
			return false, err
		}
		slog.Info("boot: embed-cache coupled fingerprint seeded — no flush; "+
			"if this installation ever changed its embed host or protocol, run "+
			"DELETE FROM context_embed_cache once (docs/operations.md, Migration 132)",
			"pairs", len(cur))
		return false, nil
	}

	if prev == cur.fingerprint() {
		return false, nil
	}

	n, err := flush(ctx, db)
	if err != nil {
		// Deliberately NOT stamping: the recorded fingerprint stays behind, so
		// the next boot — and the next coupled NOTIFY — re-diff against the
		// un-flushed stand and retry.
		return false, fmt.Errorf("events: embed-cache flush after offline coupled change failed: %w", err)
	}
	if err := storeCoupledFingerprint(ctx, db, cur); err != nil {
		// The flush DID happen; only the stamp is missing. The next boot reads
		// the old fingerprint, diffs again and flushes an already-empty cache —
		// wasteful, never wrong.
		return true, err
	}
	slog.Warn("boot: embed-cache-coupled pool topology changed while ctxd was down — flushed context_embed_cache",
		"rows", n, "pairs", len(cur))
	return true, nil
}
