//go:build integration

// Gate of Entflechtung-Stufe-2 wave α6 (A04-W4, design/04 §3.2b + §7 row 4,
// user decision E12 = §8 E-A04-4 variant b): the coupled diff survives a
// restart. W3 closed the online window through the NOTIFY funnel; this closes
// the one the funnel cannot see — a pool edit applied while ctxd is stopped,
// which leaves no notification and hands the next boot a baseline taken from
// the already-edited pool.
//
// The gate cases, all driven through the real boot entry point
// (reconcileCoupledFingerprint against a freshly loaded pool), never by writing
// the meta row by hand:
//
//   - first boot, no stamp on record    → fingerprint SEEDED, cache untouched (E12 b)
//   - unchanged reboot                  → no flush
//   - offline psql edit, then boot      → cache flushed, stamp advanced
//   - injected flush error              → stamp stays behind, next boot retries
//   - online flush via HandleNotification → stamp advanced, next boot quiet
//
// Run: go test -tags=integration ./internal/events/ -run TestCoupledFingerprint -count=1 -v
//
// Negative probes of the wave (both belong to the wave's evidence):
//   - reconcileCoupledFingerprint's flush call removed → the offline case and
//     the retry case go red, the seed/quiet cases stay green (they assert an
//     absence).
//   - storeCoupledFingerprint call removed from listener.go's
//     flushIfCoupledChanged → TestCoupledFingerprintOnlineFlushStamps goes red.
package events

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bootPool mimics what cmd/ctxd/main.go holds when the reconcile runs: a pool
// constructed and reloaded from the CURRENT table state — including any edit
// that happened while the process was down.
func bootPool(t *testing.T, db *pgxpool.Pool) *backends.Pool {
	t.Helper()
	bp := backends.NewPool(db, nil)
	if err := bp.Reload(context.Background()); err != nil {
		t.Fatalf("boot pool reload: %v", err)
	}
	return bp
}

// boot runs the production boot step against a freshly loaded pool and reports
// whether it flushed.
func boot(t *testing.T, db *pgxpool.Pool) bool {
	t.Helper()
	flushed, err := reconcileCoupledFingerprint(context.Background(), db, bootPool(t, db), embedcache.Flush)
	if err != nil {
		t.Fatalf("boot reconcile: %v", err)
	}
	return flushed
}

// storedFingerprint reads the stamp on record; ok=false covers both a missing
// row and a NULL column — the two shapes of "never stamped".
func storedFingerprint(t *testing.T, db *pgxpool.Pool) (string, bool) {
	t.Helper()
	fp, ok, err := loadCoupledFingerprint(context.Background(), db)
	if err != nil {
		t.Fatalf("load fingerprint: %v", err)
	}
	return fp, ok
}

// TestCoupledFingerprintSeedsOnFirstBootWithoutFlush is the E12 decision itself
// (design/04 §8 E-A04-4 variant b). Migration 132 seeds neither row nor value,
// so every existing installation reaches its first post-upgrade boot unstamped.
// Variant (a) would have flushed there — a full embed warmup on EVERY
// installation of the upgrade to heal the minority that ever moved its embed
// host. This pins the chosen posture: stamp, do not flush.
func TestCoupledFingerprintSeedsOnFirstBootWithoutFlush(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testdb.SetupTestDB(t)
	coupledSeedBackend(t, db, "fp-embed-a", "http://127.0.0.1:11434", 70)
	coupledSeedCache(t, db)

	if _, ok := storedFingerprint(t, db); ok {
		t.Fatal("a fresh migration chain already carries a fingerprint — migration 132 must seed none")
	}
	if flushed := boot(t, db); flushed {
		t.Fatal("first boot flushed the embed cache — E12 chose variant (b): seed without flush")
	}
	if n := coupledCacheCount(t, db); n != 1 {
		t.Fatalf("embed cache rows after first boot = %d, want 1 (untouched)", n)
	}
	fp, ok := storedFingerprint(t, db)
	if !ok {
		t.Fatal("first boot left no fingerprint — the offline window would stay open forever")
	}
	if want := coupledSetOf(bootPool(t, db)).fingerprint(); fp != want {
		t.Fatalf("seeded fingerprint = %s, want the boot topology's %s", fp, want)
	}
}

// TestCoupledFingerprintQuietReboot: a restart without any intervening edit must
// not touch the cache. This is the counter-case that keeps the whole mechanism
// from degenerating into "flush on every boot" — the exact failure a digest
// taken over unsorted map iteration would produce.
func TestCoupledFingerprintQuietReboot(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testdb.SetupTestDB(t)
	coupledSeedBackend(t, db, "fp-embed-a", "http://127.0.0.1:11434", 70)
	boot(t, db) // seed
	coupledSeedCache(t, db)

	for i := range 3 {
		if flushed := boot(t, db); flushed {
			t.Fatalf("reboot %d flushed without an intervening edit", i+1)
		}
	}
	if n := coupledCacheCount(t, db); n != 1 {
		t.Fatalf("embed cache rows after three quiet reboots = %d, want 1", n)
	}
}

// TestCoupledFingerprintFlushesOfflineEdit is the wave's reason to exist: the
// base_url moves while ctxd is DOWN. No notification is emitted to anyone, and
// the listener baseline of the next boot is taken from the edited pool — so the
// in-memory diff of W3 is empty by construction. Only the stamp on record still
// knows the old topology.
func TestCoupledFingerprintFlushesOfflineEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	db := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, db, "fp-embed-a", "http://127.0.0.1:11434", 70)
	boot(t, db) // seed
	before, _ := storedFingerprint(t, db)
	coupledSeedCache(t, db)

	// The maintenance-window edit: straight SQL, no handler, no listener.
	if _, err := db.Exec(ctx,
		`UPDATE context_backends SET base_url = $2 WHERE id = $1`, id, "http://10.13.37.19:11434"); err != nil {
		t.Fatalf("offline base_url edit: %v", err)
	}

	if flushed := boot(t, db); !flushed {
		t.Fatal("boot after an offline host change did not flush — stale vectors keep serving under the same model name")
	}
	if n := coupledCacheCount(t, db); n != 0 {
		t.Fatalf("embed cache rows after the flushing boot = %d, want 0", n)
	}
	after, ok := storedFingerprint(t, db)
	if !ok || after == before {
		t.Fatalf("fingerprint after flush = %q (was %q) — it must advance, or every later boot re-flushes", after, before)
	}

	// And the flush is once, not per boot: the stamp now describes the new
	// topology.
	coupledSeedCache(t, db)
	if flushed := boot(t, db); flushed {
		t.Fatal("second boot after the offline edit flushed again — the stamp did not take")
	}
}

// TestCoupledFingerprintFlushFailureRetries pins the failure posture of
// design/04 §4.2(b) on the boot path: the stamp advances ONLY after a
// successful flush. Advancing it on failure would make the retry a phrase — the
// next boot would compare equal, and the stale vectors would serve unbounded.
func TestCoupledFingerprintFlushFailureRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	db := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, db, "fp-embed-a", "http://127.0.0.1:11434", 70)
	boot(t, db) // seed
	before, _ := storedFingerprint(t, db)
	coupledSeedCache(t, db)

	if _, err := db.Exec(ctx,
		`UPDATE context_backends SET base_url = $2 WHERE id = $1`, id, "http://10.13.37.19:11434"); err != nil {
		t.Fatalf("offline base_url edit: %v", err)
	}

	boom := errors.New("injected flush failure")
	flushed, err := reconcileCoupledFingerprint(ctx, db, bootPool(t, db),
		func(context.Context, *pgxpool.Pool) (int64, error) { return 0, boom })
	if flushed || !errors.Is(err, boom) {
		t.Fatalf("failing boot: flushed=%v err=%v, want flushed=false and the injected error", flushed, err)
	}
	if got, _ := storedFingerprint(t, db); got != before {
		t.Fatalf("fingerprint advanced past a FAILED flush (%q != %q) — the next boot would compare equal and never retry", got, before)
	}
	if n := coupledCacheCount(t, db); n != 1 {
		t.Fatalf("embed cache rows after the failing boot = %d, want 1 (untouched)", n)
	}

	// The retry is real: the next boot re-diffs against the un-advanced stamp.
	if flushed := boot(t, db); !flushed {
		t.Fatal("the boot after a failed flush did not retry")
	}
	if n := coupledCacheCount(t, db); n != 0 {
		t.Fatalf("embed cache rows after the retry = %d, want 0", n)
	}
}

// TestCoupledFingerprintOnlineFlushStamps is the seam between W3 and W4: an
// ONLINE coupled change flushes through the listener and must leave the stamp
// describing the new topology. Without that write the listener and the boot
// check would drift apart — every restart after an online host change would
// flush a cache the listener already emptied, and the boot check would report a
// change nobody made.
func TestCoupledFingerprintOnlineFlushStamps(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	db := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, db, "fp-embed-a", "http://127.0.0.1:11434", 70)
	boot(t, db) // seed
	before, _ := storedFingerprint(t, db)

	h, _ := coupledHandler(t, db)
	coupledSeedCache(t, db)
	if _, err := db.Exec(ctx,
		`UPDATE context_backends SET base_url = $2 WHERE id = $1`, id, "http://10.13.37.19:11434"); err != nil {
		t.Fatalf("online base_url edit: %v", err)
	}
	if err := h.HandleNotification(ctx, coupledNotify("context_backends"), nil); err != nil {
		t.Fatalf("handle notification: %v", err)
	}
	if n := coupledCacheCount(t, db); n != 0 {
		t.Fatalf("embed cache rows after the online edit = %d, want 0 (W3 flush)", n)
	}

	after, ok := storedFingerprint(t, db)
	if !ok || after == before {
		t.Fatalf("fingerprint after the listener flush = %q (was %q) — the listener must stamp what it flushed", after, before)
	}
	coupledSeedCache(t, db)
	if flushed := boot(t, db); flushed {
		t.Fatal("the boot after an online flush flushed again — listener and boot check disagree about the topology")
	}
}
