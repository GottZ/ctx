//go:build integration

// Red integration gates of W06-18 (design/06 §4.10 row consumer_usage, §5
// B-06-21, §7 W06-18; RC-D S1) against a real context_settings_audit:
//
//   - soft_limit: 50 notes of one consumer × op × dimension × window ⇒ exactly
//     one row; the next day's window, the monthly dimension and another
//     consumer each write their own row; the row carries tenant_id, op,
//     dimension, window_start, used, limit and via 'gateway', no actor.
//   - hard_limit: 20 notes of one exceedance ⇒ one row; an envelope event
//     (level tenant) carries no used/limit figures (B6), even when the caller
//     handed some.
//   - first_use (B-06-21): 1 000 concurrent notes of one model key in
//     scrambled order and over every op ⇒ one row; a second key ⇒ a second
//     row; a new recorder (restart) ⇒ still one row for the first key (the
//     NOT EXISTS of the INSERT).
//   - peer_bound writes no consumer_usage row and is counted in
//     Stats.PeerBound.
//   - Note never blocks: QueueCap + 500 notes without a flush return at once,
//     the overflow is counted as dropped.
//
// Every gate is red against the S1 stub (Note drops, Flush answers
// ErrNotImplemented, Stats zero, DedupeKey ""), and stays red through its row
// count or counters when a Flush answers nil without writing.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestRecorder' ./internal/usageevents/
package usageevents_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/internal/usageevents"
)

// ueFlushTimeout bounds one Flush of a test (a real writer drains a few
// dozen rows in milliseconds).
const ueFlushTimeout = 30 * time.Second

func ueSetup(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	return context.Background(), testdb.SetupTestDB(t)
}

func ueTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	return id
}

func ueConsumer(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenant, label, kind string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label, kind) VALUES ($1::uuid, $2, $3) RETURNING id::text`,
		tenant, label, kind).Scan(&id); err != nil {
		t.Fatalf("insert consumer %s: %v", label, err)
	}
	return id
}

func ueModelKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, consumer, label string) string {
	t.Helper()
	sum := sha256.Sum256([]byte("ctxm_" + label))
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint) VALUES ($1::uuid, $2, 'ctxm_…ue00') RETURNING id::text`,
		consumer, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert model key %s: %v", label, err)
	}
	return id
}

// ueFlush drains the recorder; a failing Flush is the gate's red reason
// against the S1 stub.
func ueFlush(t *testing.T, r *usageevents.Recorder, gate string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ueFlushTimeout)
	defer cancel()
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("W06-18 gate (%s): Flush: %v", gate, err)
	}
}

// ueCount counts the consumer_usage rows of one consumer and action; extra is
// an additional SQL predicate ("" = none) whose parameters start at $3.
func ueCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, action, consumer, extra string, args ...any) int64 {
	t.Helper()
	q := `SELECT count(*) FROM context_settings_audit
	       WHERE entity_type = 'consumer_usage' AND action = $1 AND entity_key = $2`
	if extra != "" {
		q += " AND " + extra
	}
	var n int64
	if err := pool.QueryRow(ctx, q, append([]any{action, consumer}, args...)...).Scan(&n); err != nil {
		t.Fatalf("count %s rows of %s: %v", action, consumer, err)
	}
	return n
}

// ueRow is one consumer_usage row as stored.
type ueRow struct {
	scope                 string
	apiKeyNull, actorNull bool
	meta                  map[string]any
	rawMeta               string
}

// ueRows reads the consumer_usage rows of one consumer and action, oldest
// first; metadata numbers decode as json.Number.
func ueRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, action, consumer string) []ueRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT scope, api_key_id IS NULL, actor_label IS NULL, metadata::text
		   FROM context_settings_audit
		  WHERE entity_type = 'consumer_usage' AND action = $1 AND entity_key = $2
		  ORDER BY created_at, id`, action, consumer)
	if err != nil {
		t.Fatalf("read %s rows of %s: %v", action, consumer, err)
	}
	defer rows.Close()
	var out []ueRow
	for rows.Next() {
		var r ueRow
		if err := rows.Scan(&r.scope, &r.apiKeyNull, &r.actorNull, &r.rawMeta); err != nil {
			t.Fatalf("scan %s row of %s: %v", action, consumer, err)
		}
		d := json.NewDecoder(bytes.NewReader([]byte(r.rawMeta)))
		d.UseNumber()
		if err := d.Decode(&r.meta); err != nil {
			t.Fatalf("%s row of %s: metadata %s is not a JSON object: %v", action, consumer, r.rawMeta, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s rows of %s: %v", action, consumer, err)
	}
	return out
}

func ueMetaStr(r ueRow, key string) string {
	s, _ := r.meta[key].(string)
	return s
}

// ueMetaInt reads a numeric metadata field; ok is false when it is absent,
// null or not an integer.
func ueMetaInt(r ueRow, key string) (int64, bool) {
	n, ok := r.meta[key].(json.Number)
	if !ok {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil
}

// ueWantShape checks the row shape every consumer_usage row shares (package
// contract: scope '_global', api_key_id and actor_label NULL, metadata
// tenant_id, consumer_id, via 'gateway', dedupe = DedupeKey).
func ueWantShape(t *testing.T, gate string, r ueRow, e usageevents.Event) {
	t.Helper()
	if r.scope != "_global" || !r.apiKeyNull || !r.actorNull {
		t.Errorf("W06-18 gate (%s): scope %q api_key_id NULL %v actor_label NULL %v, want _global / true / true", gate, r.scope, r.apiKeyNull, r.actorNull)
	}
	for k, want := range map[string]string{
		"tenant_id": e.TenantID, "consumer_id": e.ConsumerID, "via": usageevents.Via,
	} {
		if got := ueMetaStr(r, k); got != want {
			t.Errorf("W06-18 gate (%s): metadata.%s %q, want %q (metadata %s)", gate, k, got, want, r.rawMeta)
		}
	}
	if key := usageevents.DedupeKey(e); key == "" || ueMetaStr(r, "dedupe") != key {
		t.Errorf("W06-18 gate (%s): metadata.dedupe %q, DedupeKey %q — want the same non-empty key", gate, ueMetaStr(r, "dedupe"), key)
	}
}

// ueWantLimitRow checks a soft_limit/hard_limit row against the event it
// records: op, dimension, level, window_start (RFC 3339, the window's UTC
// start) and — for the consumer level — used within [usedMin, usedMax] and
// limit.
func ueWantLimitRow(t *testing.T, gate string, r ueRow, e usageevents.Event, usedMin, usedMax int64) {
	t.Helper()
	ueWantShape(t, gate, r, e)
	for k, want := range map[string]string{"op": e.Op, "dimension": e.Dimension, "level": e.Level} {
		if got := ueMetaStr(r, k); got != want {
			t.Errorf("W06-18 gate (%s): metadata.%s %q, want %q (metadata %s)", gate, k, got, want, r.rawMeta)
		}
	}
	ws, err := time.Parse(time.RFC3339, ueMetaStr(r, "window_start"))
	if err != nil || !ws.Equal(e.WindowStart) {
		t.Errorf("W06-18 gate (%s): metadata.window_start %q, want %s (err %v)", gate, ueMetaStr(r, "window_start"), e.WindowStart.Format(time.RFC3339), err)
	}
	if e.Level != "consumer" {
		return
	}
	if used, ok := ueMetaInt(r, "used"); !ok || used < usedMin || used > usedMax {
		t.Errorf("W06-18 gate (%s): metadata.used %v, want an integer in [%d, %d] (metadata %s)", gate, r.meta["used"], usedMin, usedMax, r.rawMeta)
	}
	if limit, ok := ueMetaInt(r, "limit"); !ok || limit != e.Limit {
		t.Errorf("W06-18 gate (%s): metadata.limit %v, want %d (metadata %s)", gate, r.meta["limit"], e.Limit, r.rawMeta)
	}
}

// ueClock is a settable recorder clock.
type ueClock struct{ ns atomic.Int64 }

func newUEClock(at time.Time) *ueClock {
	c := &ueClock{}
	c.set(at)
	return c
}

func (c *ueClock) set(at time.Time) { c.ns.Store(at.UnixNano()) }
func (c *ueClock) now() time.Time   { return time.Unix(0, c.ns.Load()).UTC() }

func ueLimitEvent(kind usageevents.Kind, tenant, consumer, op, dim, level string, window time.Time, used, limit int64) usageevents.Event {
	return usageevents.Event{
		Kind: kind, ConsumerID: consumer, TenantID: tenant, Op: op, Dimension: dim,
		Level: level, WindowStart: window, Used: used, Limit: limit,
	}
}

func TestRecorderSoftLimitOncePerWindow(t *testing.T) {
	ctx, pool := ueSetup(t)
	tenant := ueTenant(ctx, t, pool, "ue-soft")
	consA := ueConsumer(ctx, t, pool, tenant, "ue-soft-a", "app")
	consB := ueConsumer(ctx, t, pool, tenant, "ue-soft-b", "app")
	day0 := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	day1 := day0.AddDate(0, 0, 1)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clk := newUEClock(day0.Add(12 * time.Hour))
	r := usageevents.NewRecorder(pool, clk.now)
	soft := func(consumer, dim string, window time.Time, used int64) usageevents.Event {
		return ueLimitEvent(usageevents.KindSoftLimit, tenant, consumer, "chat", dim, "consumer", window, used, 1000)
	}

	const gate = "soft_limit once per window"
	if k0, k49 := usageevents.DedupeKey(soft(consA, "daily_tokens", day0, 800)), usageevents.DedupeKey(soft(consA, "daily_tokens", day0, 849)); k0 == "" || k0 != k49 {
		t.Errorf("W06-18 gate (%s): DedupeKey %q vs %q for one consumer × op × dimension × window, want one non-empty key independent of the figures", gate, k0, k49)
	}
	for i := range 50 {
		r.Note(soft(consA, "daily_tokens", day0, 800+int64(i)))
	}
	ueFlush(t, r, gate)
	if n := ueCount(ctx, t, pool, "soft_limit", consA, ""); n != 1 {
		t.Fatalf("W06-18 gate (%s): %d consumer_usage rows after 50 notes, want 1", gate, n)
	}
	rows := ueRows(ctx, t, pool, "soft_limit", consA)
	ueWantLimitRow(t, gate, rows[0], soft(consA, "daily_tokens", day0, 800), 800, 849)
	if s := r.Stats(); s.Deduped < 49 {
		t.Errorf("W06-18 gate (%s): Stats %+v, want Deduped ≥ 49", gate, s)
	}

	// The next day: the same 50 notes write the second window's row.
	clk.set(day1.Add(12 * time.Hour))
	for i := range 50 {
		r.Note(soft(consA, "daily_tokens", day1, 800+int64(i)))
	}
	ueFlush(t, r, gate+", next day")
	if n := ueCount(ctx, t, pool, "soft_limit", consA, ""); n != 2 {
		t.Fatalf("W06-18 gate (soft_limit once per window, next day): %d rows of the consumer, want 2", n)
	}
	if n := ueCount(ctx, t, pool, "soft_limit", consA, "(metadata->>'window_start')::timestamptz = $3", day1); n != 1 {
		t.Errorf("W06-18 gate (soft_limit once per window, next day): %d rows with window_start %s, want 1", n, day1.Format(time.RFC3339))
	}
	if s := r.Stats(); s.Deduped < 98 {
		t.Errorf("W06-18 gate (%s, next day): Stats %+v, want Deduped ≥ 98", gate, s)
	}

	// The monthly dimension of the same consumer is its own key.
	r.Note(soft(consA, "monthly_tokens", month, 24000))
	r.Note(soft(consA, "monthly_tokens", month, 24001))
	ueFlush(t, r, gate+", monthly dimension")
	if n := ueCount(ctx, t, pool, "soft_limit", consA, "metadata->>'dimension' = 'monthly_tokens'"); n != 1 {
		t.Fatalf("W06-18 gate (soft_limit once per window, monthly dimension): %d monthly_tokens rows, want 1", n)
	}

	// Another consumer, same op × dimension × window: its own row.
	r.Note(soft(consB, "daily_tokens", day1, 900))
	ueFlush(t, r, gate+", other consumer")
	if n := ueCount(ctx, t, pool, "soft_limit", consB, ""); n != 1 {
		t.Fatalf("W06-18 gate (soft_limit once per window, other consumer): %d rows of consumer B, want 1", n)
	}
	if n := ueCount(ctx, t, pool, "soft_limit", consA, ""); n != 3 {
		t.Errorf("W06-18 gate (soft_limit once per window): %d rows of consumer A after all steps, want 3", n)
	}
	if s := r.Stats(); s.Written != 4 {
		t.Errorf("W06-18 gate (%s): Stats %+v, want Written 4 (two daily windows, one monthly, one other consumer)", gate, s)
	}
}

func TestRecorderHardLimitOncePerExceedance(t *testing.T) {
	ctx, pool := ueSetup(t)
	tenant := ueTenant(ctx, t, pool, "ue-hard")
	consA := ueConsumer(ctx, t, pool, tenant, "ue-hard-a", "app")
	consB := ueConsumer(ctx, t, pool, tenant, "ue-hard-b", "app")
	day := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	r := usageevents.NewRecorder(pool, newUEClock(day.Add(15*time.Hour)).now)

	const gate = "hard_limit once per exceedance"
	for i := range 20 {
		r.Note(ueLimitEvent(usageevents.KindHardLimit, tenant, consA, "chat", "daily_tokens", "consumer", day, 1000+int64(i), 1000))
	}
	ueFlush(t, r, gate)
	if n := ueCount(ctx, t, pool, "hard_limit", consA, ""); n != 1 {
		t.Fatalf("W06-18 gate (%s): %d consumer_usage rows after 20 notes, want 1", gate, n)
	}
	ueWantLimitRow(t, gate, ueRows(ctx, t, pool, "hard_limit", consA)[0],
		ueLimitEvent(usageevents.KindHardLimit, tenant, consA, "chat", "daily_tokens", "consumer", day, 1000, 1000), 1000, 1019)

	// Consumer B hits the tenant envelope. The caller hands figures on
	// purpose: the B6 doctrine is the writer's, not the caller's.
	const envGate = "envelope event carries no used/limit (B6)"
	env := ueLimitEvent(usageevents.KindHardLimit, tenant, consB, "chat", "daily_tokens", "tenant", day, 5000, 4000)
	for range 5 {
		r.Note(env)
	}
	ueFlush(t, r, envGate)
	if n := ueCount(ctx, t, pool, "hard_limit", consB, ""); n != 1 {
		t.Fatalf("W06-18 gate (%s): %d hard_limit rows of the envelope consumer, want 1", envGate, n)
	}
	row := ueRows(ctx, t, pool, "hard_limit", consB)[0]
	ueWantLimitRow(t, envGate, row, env, 0, 0)
	for _, k := range []string{"used", "limit"} {
		if v, ok := row.meta[k]; ok && v != nil {
			t.Errorf("W06-18 gate (%s): metadata.%s = %v on a level-tenant row, want absent or null (metadata %s)", envGate, k, v, row.rawMeta)
		}
	}
}

func TestRecorderFirstUseOncePerKey(t *testing.T) {
	ctx, pool := ueSetup(t)
	tenant := ueTenant(ctx, t, pool, "ue-first")
	cons := ueConsumer(ctx, t, pool, tenant, "ue-first-app", "app")
	k1 := ueModelKey(ctx, t, pool, cons, "ue-first-k1")
	k2 := ueModelKey(ctx, t, pool, cons, "ue-first-k2")
	r := usageevents.NewRecorder(pool, nil)
	ops := []string{"chat", "embed", "rerank"}
	first := func(key, op string) usageevents.Event {
		return usageevents.Event{Kind: usageevents.KindFirstUse, ConsumerID: cons, TenantID: tenant, Op: op, ModelKeyID: key}
	}

	// B-06-21: the key is the model key alone — nothing a request carries
	// (no Ctx-Hop, no op) may split it.
	const gate = "first_use once per key (B-06-21)"
	if kc, ke := usageevents.DedupeKey(first(k1, "chat")), usageevents.DedupeKey(first(k1, "embed")); kc == "" || kc != ke || kc == usageevents.DedupeKey(first(k2, "chat")) {
		t.Errorf("W06-18 gate (%s): DedupeKey chat %q embed %q K2 %q, want one non-empty key per model key, independent of the op", gate, kc, ke, usageevents.DedupeKey(first(k2, "chat")))
	}

	// 1 000 requests of one app consumer in a random order: each goroutine
	// yields a different number of times before it notes, over every op.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 1000 {
		wg.Go(func() {
			<-start
			for range i % 7 {
				runtime.Gosched()
			}
			r.Note(first(k1, ops[i%len(ops)]))
		})
	}
	close(start)
	wg.Wait()
	ueFlush(t, r, gate)
	if n := ueCount(ctx, t, pool, "first_use", cons, "metadata->>'model_key_id' = $3", k1); n != 1 {
		t.Fatalf("W06-18 gate (%s): %d first_use rows for K1 after 1 000 concurrent notes, want 1", gate, n)
	}
	rows := ueRows(ctx, t, pool, "first_use", cons)
	if len(rows) != 1 {
		t.Fatalf("W06-18 gate (%s): %d first_use rows of the consumer, want 1", gate, len(rows))
	}
	ueWantShape(t, gate, rows[0], first(k1, "chat"))

	// A second key of the same consumer: its own row.
	r.Note(first(k2, "embed"))
	ueFlush(t, r, gate+", second key")
	if n := ueCount(ctx, t, pool, "first_use", cons, "metadata->>'model_key_id' = $3", k2); n != 1 {
		t.Fatalf("W06-18 gate (first_use once per key, second key): %d first_use rows for K2, want 1", n)
	}

	// Restart: a new recorder has an empty in-memory set; the INSERT's NOT
	// EXISTS keeps K1 at one row.
	r2 := usageevents.NewRecorder(pool, nil)
	r2.Note(first(k1, "rerank"))
	ueFlush(t, r2, gate+", after restart")
	if n := ueCount(ctx, t, pool, "first_use", cons, "metadata->>'model_key_id' = $3", k1); n != 1 {
		t.Fatalf("W06-18 gate (first_use once per key, after restart): %d first_use rows for K1, want 1 (DB-side NOT EXISTS)", n)
	}
	if n := ueCount(ctx, t, pool, "first_use", cons, ""); n != 2 {
		t.Errorf("W06-18 gate (%s): %d first_use rows of the consumer at the end, want 2 (K1, K2)", gate, n)
	}
}

func TestRecorderPeerBoundWritesNoRow(t *testing.T) {
	ctx, pool := ueSetup(t)
	tenant := ueTenant(ctx, t, pool, "ue-peer")
	cons := ueConsumer(ctx, t, pool, tenant, "ue-peer-ctx", "ctx-peer")
	r := usageevents.NewRecorder(pool, nil)

	const gate = "peer_bound writes no row"
	r.Note(usageevents.Event{Kind: usageevents.KindPeerBound, ConsumerID: cons, TenantID: tenant, PeerInstanceID: uuid.NewString()})
	if s := r.Stats(); s.PeerBound != 1 {
		t.Errorf("W06-18 gate (%s): Stats %+v after one peer_bound note, want PeerBound 1", gate, s)
	}
	ueFlush(t, r, gate)
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_settings_audit WHERE entity_type = 'consumer_usage' AND entity_key = $1`, cons).Scan(&n); err != nil {
		t.Fatalf("count consumer_usage rows: %v", err)
	}
	if n != 0 {
		t.Errorf("W06-18 gate (%s): %d consumer_usage rows of the bound peer, want 0 (the binding is the model_consumer trigger's row)", gate, n)
	}
	if s := r.Stats(); s.PeerBound != 1 || s.Written != 0 {
		t.Errorf("W06-18 gate (%s): Stats %+v after the flush, want PeerBound 1, Written 0", gate, s)
	}
}

// ueRaceBuild reports a build with the race detector, read from the test
// binary's build settings (the package has no raceEnabled constant).
func ueRaceBuild() bool {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range bi.Settings {
		if s.Key == "-race" && s.Value == "true" {
			return true
		}
	}
	return false
}

// ueNoteBound is the wall-clock bound of QueueCap + 500 notes without a
// flush. The design target is 50 ms; the bound is ten times that so host
// load does not flake it, and under -race or coverage it is only logged.
const ueNoteBound = 500 * time.Millisecond

func TestRecorderNeverBlocks(t *testing.T) {
	_, pool := ueSetup(t)
	r := usageevents.NewRecorder(pool, nil)
	// Registered after SetupTestDB: runs before the pool closes.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ueFlushTimeout)
		defer cancel()
		if err := r.Flush(ctx); err != nil {
			t.Logf("cleanup flush: %v", err)
		}
	})

	n := usageevents.QueueCap + 500
	window := time.Now().UTC().Truncate(24 * time.Hour)
	tenant := uuid.NewString()
	evs := make([]usageevents.Event, n)
	for i := range evs {
		evs[i] = ueLimitEvent(usageevents.KindSoftLimit, tenant, uuid.NewString(), "chat", "daily_tokens", "consumer", window, 900, 1000)
	}

	const gate = "Note never blocks"
	began := time.Now()
	for _, e := range evs {
		r.Note(e)
	}
	took := time.Since(began)
	race, cover := ueRaceBuild(), testing.CoverMode()
	t.Logf("%d notes without a flush took %v (target 50ms, bound %v, race %v, cover %q)", n, took, ueNoteBound, race, cover)
	if !race && cover == "" && took > ueNoteBound {
		t.Errorf("W06-18 gate (%s): %d notes took %v, want ≤ %v", gate, n, took, ueNoteBound)
	}
	s := r.Stats()
	if s.Noted+s.Dropped != int64(n) || s.Dropped == 0 {
		t.Fatalf("W06-18 gate (%s, full queue drops): Stats %+v, want Noted + Dropped = %d with Dropped > 0", gate, s, n)
	}
}
