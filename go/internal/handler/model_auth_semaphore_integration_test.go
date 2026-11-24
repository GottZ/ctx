//go:build integration

// W01-6 gate (d) — the DB semaphore (B18, design/01 §6 "DB-Pool"): 1000
// parallel forgeries (probe bucket opened wide) while a DB-bound knowledge
// load runs on the same production-sized pool (MaxConns 20). The knowledge
// load is a stand-in with a real pgxpool.Acquire (/api/query needs models):
// 12 workers, each Acquire → SELECT pg_sleep(0.002) → Release.
//
// The promise: the door's share of the pool is gateway.auth_db_concurrency
// (ModelAuthConfig.Pool) — under the flood the door never holds more pool
// slots than the semaphore admits, so the knowledge load, fewer workers than
// the slots the door leaves, never meets a full pool. Judged by counted pool
// events, not by the host's clock (#369): semPoolMeter counts the slots each
// owner holds — the load marks its acquire context, every other acquire on
// this pool is the door's. Verdict: the door's acquires in flight reach the
// semaphore capacity (the flood filled it) and never exceed it, the door's
// slots — acquires in flight, connections it left closing, connections still
// being built for an acquire it gave up — never exceed it either, door and
// load together stay below MaxConns, the baseline phase has no door acquire,
// nothing is left held, and the door answers 401 or 503 auth_unavailable,
// never 500. The load's acquire quantiles, flood against baseline, are
// logged and not judged (#519 point 10: under -race at host load the old
// p99 gate also tripped on microsecond noise). This measurement closes the
// open measurement "Auth-Semaphor-Kapazität" (masterplan, Offene Messungen).
//
// A second flood runs against a database that does not answer the door: a
// lock on the key table holds every check past its deadline (issue #519).
// Before the fix a check its deadline cancelled destroyed its connection,
// whose close — and a build an abandoned acquire left — kept a pool slot
// after the semaphore released it, and the door held more slots than the
// semaphore admits; the same verdict applies to that flood. A third holds
// the checks past modelAuthCheckBound, where their statements are cancelled
// and their connections destroyed: a slot goes back only once its close is
// done (onSlot waits for CleanupDone), so the verdict holds there too.
//
//	go test -tags=integration -p 1 -count=1 -run TestModelAuthSemaphore -v ./internal/handler/
package handler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/oai"
)

const (
	semLoadWorkers = 12 // stays below the 16 connections the semaphore leaves
	semFlooders    = 1000
	semPhase       = 3 * time.Second
	// semStall is how long the stalled flood's lock holds the door's checks:
	// past modelAuthDBDeadline, so every check of that stretch runs into it.
	semStall = 2500 * time.Millisecond
	// semStallPastBound holds them past modelAuthCheckBound as well: the
	// checks' statements are cancelled there and their connections
	// destroyed, and the slot goes back only once the close is done.
	semStallPastBound = modelAuthCheckBound + 1500*time.Millisecond
)

func semLoadavg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "n/a"
	}
	return strings.TrimSpace(string(b))
}

func semQuantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(q*float64(len(s))+0.5) - 1
	return s[max(0, min(i, len(s)-1))]
}

// semLoadMark marks the knowledge load's acquire context.
type semLoadMark struct{}

type semOwner int

const (
	semDoor semOwner = iota
	semLoad
)

// semAcquire is one traced Acquire; semAcquireKey carries it from
// TraceAcquireStart into TraceAcquireEnd and, through the constructor's
// context (puddle keeps the acquire context's values), into
// TraceConnectStart/End.
type semAcquire struct {
	owner        semOwner
	constructing bool // a connection is being built for it
	gaveUp       bool // it ended with an error while the build went on
}

type semAcquireKey struct{}

// semCounts is one phase of the meter, per owner: acquires started,
// connections built, released connections the pool destroyed, the slots
// held now and their peak, the three kinds of slot with their peaks, and
// the peak of both owners together.
type semCounts struct {
	acquires, built, destroyed                  [2]int
	held, peak                                  [2]int
	inFlight, closing, orphans                  [2]int
	peakInFlight, peakClosing, peakOrphans      [2]int
	peakTotal, unknownReleases, unknownConnects int
}

// semPoolMeter counts the pool slots each owner holds, event by event. A
// slot of pgxpool's Pool is held
//   - by an Acquire from TraceAcquireStart (waiting included) to
//     TraceAcquireEnd with an error or to TraceRelease;
//   - by a released connection the pool destroys (closed, busy or inside a
//     transaction — Conn.Release's own test) until its close is done:
//     pgconn's close of a cancelled connection sends a cancel request and
//     drains, and the pool's destructor frees the slot only after
//     CleanupDone;
//   - by a connection still being built for an Acquire that already gave up
//     (puddle finishes the build in the background) until TraceConnectEnd.
//
// Not traced: pgxpool's health check takes idle slots for a moment, on the
// pool's one-minute ticker (longer than this test) or 500 ms after a
// destroyed connection. One mutex makes the total's peak a peak of
// simultaneous slots. The embedded madTracer keeps the fixture's statement
// trace.
type semPoolMeter struct {
	*madTracer
	mu      sync.Mutex
	owner   map[*pgx.Conn]semOwner // acquired connections, until their release
	closing []semClosing           // destroyed connections whose close may still run
	total   int
	c       semCounts
}

// semClosing is a destroyed connection of owner o until done (CleanupDone).
type semClosing struct {
	o    semOwner
	done <-chan struct{}
}

// endClosed ends the closing slots whose close is done; the caller holds
// m.mu. Every acquire runs it first, so a slot whose close ended before the
// acquire started is never counted beside it — the order onSlot keeps when it
// waits for CleanupDone before its semaphore slot goes back — while one whose
// close still runs is.
func (m *semPoolMeter) endClosed() {
	kept := m.closing[:0]
	for _, c := range m.closing {
		select {
		case <-c.done:
			m.add(&m.c.closing, &m.c.peakClosing, c.o, -1)
		default:
			kept = append(kept, c)
		}
	}
	m.closing = kept
}

// add moves owner o's slots of one kind by n and updates the peaks; the
// caller holds m.mu.
func (m *semPoolMeter) add(kind, peak *[2]int, o semOwner, n int) {
	kind[o] += n
	peak[o] = max(peak[o], kind[o])
	m.c.held[o] += n
	m.c.peak[o] = max(m.c.peak[o], m.c.held[o])
	m.total += n
	m.c.peakTotal = max(m.c.peakTotal, m.total)
}

// move hands one of owner o's slots from an acquire in flight to another
// kind (closing, or a build for a given-up acquire): the slot stays held,
// so neither the owner's nor the total's peak moves.
func (m *semPoolMeter) move(to, peak *[2]int, o semOwner) {
	m.c.inFlight[o]--
	to[o]++
	peak[o] = max(peak[o], to[o])
}

func (m *semPoolMeter) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	a := &semAcquire{owner: semDoor}
	if ctx.Value(semLoadMark{}) != nil {
		a.owner = semLoad
	}
	m.mu.Lock()
	m.endClosed()
	m.c.acquires[a.owner]++
	m.add(&m.c.inFlight, &m.c.peakInFlight, a.owner, 1)
	m.mu.Unlock()
	return context.WithValue(ctx, semAcquireKey{}, a)
}

func (m *semPoolMeter) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, d pgxpool.TraceAcquireEndData) {
	a := ctx.Value(semAcquireKey{}).(*semAcquire)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case d.Err == nil:
		m.owner[d.Conn] = a.owner // the slot stays with the acquire until TraceRelease
	case a.constructing:
		a.gaveUp = true
		m.move(&m.c.orphans, &m.c.peakOrphans, a.owner)
	default:
		m.add(&m.c.inFlight, &m.c.peakInFlight, a.owner, -1)
	}
}

func (m *semPoolMeter) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := ctx.Value(semAcquireKey{}).(*semAcquire); ok {
		a.constructing = true
	} else {
		m.c.unknownConnects++
	}
	return ctx
}

func (m *semPoolMeter) TraceConnectEnd(ctx context.Context, d pgx.TraceConnectEndData) {
	a, ok := ctx.Value(semAcquireKey{}).(*semAcquire)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a.constructing = false
	if d.Err == nil {
		m.c.built[a.owner]++
	}
	if a.gaveUp {
		m.add(&m.c.orphans, &m.c.peakOrphans, a.owner, -1)
	}
}

func (m *semPoolMeter) TraceRelease(_ *pgxpool.Pool, d pgxpool.TraceReleaseData) {
	pc := d.Conn.PgConn()
	destroy := d.Conn.IsClosed() || pc.IsBusy() || pc.TxStatus() != 'I'
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.owner[d.Conn]
	if !ok {
		m.c.unknownReleases++
		return
	}
	delete(m.owner, d.Conn)
	if !destroy {
		m.add(&m.c.inFlight, &m.c.peakInFlight, o, -1)
		return
	}
	m.c.destroyed[o]++
	m.move(&m.c.closing, &m.c.peakClosing, o)
	m.closing = append(m.closing, semClosing{o: o, done: pc.CleanupDone()})
	go func() {
		<-pc.CleanupDone()
		m.mu.Lock()
		m.endClosed()
		m.mu.Unlock()
	}()
}

// phase returns the counts since the last call and starts a new phase; the
// slots held carry over.
func (m *semPoolMeter) phase() semCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.c
	m.c = semCounts{
		held: c.held, peak: c.held, inFlight: c.inFlight, closing: c.closing, orphans: c.orphans,
		peakInFlight: c.inFlight, peakClosing: c.closing, peakOrphans: c.orphans, peakTotal: m.total,
	}
	return c
}

// settled waits until no slot is held any more — closes and background
// builds done — within the destructor's own 15-s close budget, and returns
// the counts so far.
func (m *semPoolMeter) settled() semCounts {
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		m.mu.Lock()
		c := m.c
		m.mu.Unlock()
		if c.held == [2]int{} || time.Now().After(deadline) {
			return c
		}
	}
}

func TestModelAuthSemaphore_Integration(t *testing.T) {
	e := madSetup(t)
	e.settings.auth(func(a *oai.AuthSettings) { a.ProbeRate = 1 << 30 })
	ctx := context.Background()
	// The fixture's pool again, with the meter as its tracer.
	meter := &semPoolMeter{madTracer: e.tracer, owner: map[*pgx.Conn]semOwner{}}
	cfg := e.pool.Config()
	cfg.ConnConfig.Tracer = meter
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("metered pool: %v", err)
	}
	t.Cleanup(pool.Close)
	e.pool = pool
	capacity, maxConns := e.settings.get().Auth.DBConcurrency, int(cfg.MaxConns)
	if capacity+semLoadWorkers >= maxConns {
		t.Fatalf("fixture: semaphore capacity %d + %d load workers must stay below MaxConns %d", capacity, semLoadWorkers, maxConns)
	}
	d, h, _ := e.door(t, "")
	keys := forged(t, 20000)

	phase := func(flood bool, stall time.Duration) ([]time.Duration, map[string]int, int64) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var samples []time.Duration
		statuses := map[string]int{}
		var requests atomic.Int64
		if flood {
			for g := range semFlooders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ip := fmt.Sprintf("10.30.%d.%d", g>>8, g&255)
					for it := 0; ; it++ {
						select {
						case <-stop:
							return
						default:
						}
						k := keys[(g*7919+it)%len(keys)]
						r := madDo(h, madReq{ip: ip, bearer: k.Plaintext()})
						requests.Add(1)
						key := fmt.Sprintf("%d %s", r.Code, madCode(r))
						mu.Lock()
						statuses[key]++
						mu.Unlock()
					}
				}()
			}
			time.Sleep(200 * time.Millisecond) // let the flood reach steady state
		}
		if stall > 0 {
			// The lock holder is a connection of its own, outside the
			// metered pool.
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := pgx.Connect(ctx, e.dsn)
				if err != nil {
					t.Errorf("lock holder: %v", err)
					return
				}
				defer conn.Close(ctx)
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Errorf("lock holder: %v", err)
					return
				}
				if _, err := tx.Exec(ctx, `LOCK TABLE context_model_keys IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Errorf("lock holder: %v", err)
				}
				time.Sleep(stall)
				_ = tx.Rollback(ctx)
			}()
		}
		lctx := context.WithValue(ctx, semLoadMark{}, true)
		var lw sync.WaitGroup
		for range semLoadWorkers {
			lw.Add(1)
			go func() {
				defer lw.Done()
				var local []time.Duration
				for {
					select {
					case <-stop:
						mu.Lock()
						samples = append(samples, local...)
						mu.Unlock()
						return
					default:
					}
					t0 := time.Now()
					c, err := e.pool.Acquire(lctx)
					dt := time.Since(t0)
					if err != nil {
						t.Errorf("knowledge load acquire: %v", err)
						return
					}
					local = append(local, dt)
					if _, err := c.Exec(ctx, "SELECT pg_sleep(0.002)"); err != nil {
						t.Errorf("knowledge load query: %v", err)
					}
					c.Release()
				}
			}()
		}
		// The flood runs on until the stall has ended, so checks keep waiting
		// for the semaphore while those past the bound give their slots back.
		run := semPhase
		if stall+time.Second > run {
			run = stall + time.Second
		}
		time.Sleep(run)
		close(stop)
		lw.Wait()
		wg.Wait()
		return samples, statuses, requests.Load()
	}

	t.Logf("loadavg before: %s", semLoadavg())
	meter.phase()
	base, _, _ := phase(false, 0)
	bc := meter.phase()
	if bc.acquires[semDoor] != 0 {
		t.Errorf("baseline: %d pool acquires without the load's mark — the door's count would include another pool user", bc.acquires[semDoor])
	}
	t.Logf("pool slots, capacity %d, MaxConns %d — baseline: load peak %d (%d acquires), door acquires %d", capacity, maxConns,
		bc.peak[semLoad], bc.acquires[semLoad], bc.acquires[semDoor])
	unknown := bc.unknownReleases + bc.unknownConnects
	for _, fl := range []struct {
		name  string
		stall time.Duration
	}{{"flood", 0}, {"stalled flood", semStall}, {"flood stalled past the check bound", semStallPastBound}} {
		flood, statuses, reqs := phase(true, fl.stall)
		fc := meter.settled()
		meter.phase()
		st := d.Stats()
		b99, f99 := semQuantile(base, 0.99), semQuantile(flood, 0.99)
		t.Logf("%s: loadavg %s; load acquires n=%d p50=%v p99=%v max=%v (baseline p99 %v, ratio %.3f — logged, not judged); requests %d, statuses %v",
			fl.name, semLoadavg(), len(flood), semQuantile(flood, 0.5), f99, semQuantile(flood, 1), b99, float64(f99)/float64(b99), reqs, statuses)
		t.Logf("%s: door counters so far: semaphore waits %d, total wait %v, semaphore 503 %d, db errors %d, invalid %d",
			fl.name, st.SemaphoreWaits, st.SemaphoreWaitTotal, st.SemaphoreTimeouts, st.DBErrors, st.Invalid)
		t.Logf("%s: pool slots [door load]: peak %v of which acquires in flight %v, closing %v, built for a given-up acquire %v; "+
			"together %d; acquires %v, connections built %v, destroyed %v; left held %v",
			fl.name, fc.peak, fc.peakInFlight, fc.peakClosing, fc.peakOrphans, fc.peakTotal, fc.acquires, fc.built, fc.destroyed, fc.held)
		unknown += fc.unknownReleases + fc.unknownConnects
		for k := range statuses {
			if k != fmt.Sprintf("%d %s", http.StatusUnauthorized, oai.CodeInvalidAPIKey) &&
				k != fmt.Sprintf("%d %s", http.StatusServiceUnavailable, ModelAuthCodeUnavailable) {
				t.Errorf("%s: answer %q (%d×) — only 401 invalid_api_key or 503 %s are allowed, never 500", fl.name, k, statuses[k], ModelAuthCodeUnavailable)
			}
		}
		if reqs < semFlooders {
			t.Fatalf("%s ran only %d requests", fl.name, reqs)
		}
		if fc.held != [2]int{} {
			t.Errorf("%s: slots still held %v [door load] after it settled", fl.name, fc.held)
		}
		switch door := fc.peakInFlight[semDoor]; {
		case door < capacity:
			t.Errorf("%s never filled the semaphore: at most %d door acquires in flight, capacity %d — the bound was not exercised", fl.name, door, capacity)
		case door > capacity:
			t.Errorf("%s: the semaphore let %d door acquires into the pool at once, capacity %d", fl.name, door, capacity)
		}
		if fc.peak[semDoor] > capacity {
			t.Errorf("%s: the door held or queued for %d pool slots at once, capacity %d: up to %d acquires in flight, %d connections it left closing, "+
				"%d connections built for acquires it gave up (%d connections destroyed)",
				fl.name, fc.peak[semDoor], capacity, fc.peakInFlight[semDoor], fc.peakClosing[semDoor], fc.peakOrphans[semDoor], fc.destroyed[semDoor])
		}
		if fc.peakTotal >= maxConns {
			t.Errorf("%s: door and knowledge load held or queued for %d pool slots at once — the load met a full pool (MaxConns %d)", fl.name, fc.peakTotal, maxConns)
		}
		if fl.stall > 0 && st.DBErrors == 0 && st.SemaphoreTimeouts == 0 {
			t.Errorf("%s: no check ran into its deadline — the stall was not exercised", fl.name)
		}
		if fl.stall > modelAuthCheckBound && fc.destroyed[semDoor] == 0 {
			t.Errorf("%s: no door connection was destroyed — no check reached modelAuthCheckBound", fl.name)
		}
	}
	if unknown != 0 {
		t.Errorf("meter unbalanced: %d untraced releases or connects", unknown)
	}
}
