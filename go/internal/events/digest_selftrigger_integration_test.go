//go:build integration

// Issue #525, the background digest and the notifications of block writes:
// the write of its own topic map does not start it again, and a block written
// while a run is under way is not lost to that run's end.
//
// The notifications are real: PostgreSQL's NOTIFY of trg_block_write, read by
// a pgxlisten listener on its own connection and handed to the production
// WriteHandler — through a gate the test holds. Each notification waits at
// the gate until the test passes it on (or drops one of its own sentinels),
// so the order of notification and run state is set by the test, not by a
// sleep. PostgreSQL delivers the notifications of different transactions in
// commit order, so passing everything up to a sentinel block written last
// passes everything committed before it.
//
//	go test -tags=integration ./internal/events/ -run 'TestBackgroundDigest' -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgxlisten"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gateWait bounds every wait of these tests for a notification or a run. It
// is a failure bound, not a pacing: no step waits for it to pass.
const gateWait = 30 * time.Second

// writeGate stands between the listener and the production WriteHandler.
type writeGate struct {
	inner *WriteHandler
	ready chan struct{}
	once  sync.Once

	arrived   chan string // the id of a notification held at the gate
	decide    chan bool   // true: pass it on to the handler, false: drop it
	processed chan struct{}

	mu   sync.Mutex
	auto bool        // pass every notification on without holding it
	seen chan string // in auto mode: the id of each notification passed on
}

func (g *writeGate) HandleNotification(ctx context.Context, n *pgconn.Notification, conn *pgx.Conn) error {
	var p blockNotifyPayload
	_ = json.Unmarshal([]byte(n.Payload), &p)
	g.mu.Lock()
	auto := g.auto
	g.mu.Unlock()
	if auto {
		err := g.inner.HandleNotification(ctx, n, conn)
		select {
		case g.seen <- p.ID:
		default:
		}
		return err
	}
	select {
	case g.arrived <- p.ID:
	case <-ctx.Done():
		return nil
	}
	var pass bool
	select {
	case pass = <-g.decide:
	case <-ctx.Done():
		return nil
	}
	var err error
	if pass {
		err = g.inner.HandleNotification(ctx, n, conn)
	}
	select {
	case g.processed <- struct{}{}:
	case <-ctx.Done():
	}
	return err
}

// HandleBacklog is called once the listener LISTENs: the gate is open for
// business. The production backlog signal is not passed on; these tests are
// about single notifications.
func (g *writeGate) HandleBacklog(context.Context, string, *pgx.Conn) error {
	g.once.Do(func() { close(g.ready) })
	return nil
}

func (g *writeGate) setAuto(on bool) {
	g.mu.Lock()
	g.auto = on
	g.mu.Unlock()
}

// next returns the id of the next notification, held at the gate.
func (g *writeGate) next(t *testing.T) string {
	t.Helper()
	select {
	case id := <-g.arrived:
		return id
	case <-time.After(gateWait):
		t.Fatal("no notification reached the gate")
		return ""
	}
}

// settle passes the held notification on (or drops it) and waits until the
// handler is done with it.
func (g *writeGate) settle(t *testing.T, pass bool) {
	t.Helper()
	g.decide <- pass
	select {
	case <-g.processed:
	case <-time.After(gateWait):
		t.Fatal("the handler did not finish a notification")
	}
}

// selfFixture is a scheduler over a test database with a listener whose
// notifications pass the gate.
type selfFixture struct {
	s       *Scheduler
	pool    *pgxpool.Pool
	gate    *writeGate
	logs    *holdLog
	seq     atomic.Int64
	scope   string // the home scope of the default tenant
	tenants []backgroundTenant
}

func newSelfFixture(t *testing.T, tenants ...backgroundTenant) *selfFixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	f := &selfFixture{pool: pool, scope: "private"}
	cfg := captureTestConfig(t, 12)
	// The default tenant reads its home scope; a second tenant, when a test
	// adds one, owns t525 and reads it from the same read_scopes.
	cfg.Scheduler.ReadScopes = []string{"private", "t525"}
	f.s = NewScheduler(pool, config.NewStore(cfg), backends.NewPool(nil, nil), StartupConfig{})
	f.s.SetBlocktypeRegistry(reg)
	f.tenants = append([]backgroundTenant{{scope: store.GlobalScope}}, tenants...)
	f.s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return f.tenants }

	f.logs = &holdLog{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f.gate = &writeGate{
		inner:     &WriteHandler{scheduler: f.s},
		ready:     make(chan struct{}),
		arrived:   make(chan string),
		decide:    make(chan bool),
		processed: make(chan struct{}),
		seen:      make(chan string, 4096),
	}
	l := &pgxlisten.Listener{
		Connect:        func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, dsn) },
		LogError:       func(ctx context.Context, err error) {},
		ReconnectDelay: time.Second,
	}
	l.Handle(channelBlockWrite, f.gate)
	lctx, lcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Listen(lctx)
	}()
	t.Cleanup(func() {
		lcancel()
		<-done
	})
	select {
	case <-f.gate.ready:
	case <-time.After(gateWait):
		t.Fatal("the listener did not LISTEN")
	}
	return f
}

// write writes a block through the production write path and returns its id.
func (f *selfFixture) write(t *testing.T, scope, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "decisions", title, "body of "+title, nil, nil, scope, true,
		store.SensitivityWrite{}, "")
	if err != nil {
		t.Errorf("write %q: %v", title, err)
		return ""
	}
	return b.ID
}

// flush writes a sentinel block outside every read window and passes every
// notification that arrives before the sentinel's on to the handler; the
// sentinel's own is dropped.
func (f *selfFixture) flush(t *testing.T) {
	t.Helper()
	sentinel := f.write(t, "sentinel-525", fmt.Sprintf("sentinel %d", f.seq.Add(1)))
	for {
		id := f.gate.next(t)
		if id == sentinel {
			f.gate.settle(t, false)
			return
		}
		f.gate.settle(t, true)
	}
}

// debouncePassed moves the last write back past the digest's debounce, as
// the passing of time would.
func (f *selfFixture) debouncePassed() {
	f.s.mu.Lock()
	if !f.s.lastWriteAt.IsZero() {
		f.s.lastWriteAt = time.Now().Add(-2 * digestDebounce)
	}
	f.s.mu.Unlock()
}

func (f *selfFixture) runs() int { return f.logs.count("scheduler: running digest") }

// mapText reads the topic map of scope.
func (f *selfFixture) mapText(t *testing.T, scope string) string {
	t.Helper()
	var text string
	if err := f.pool.QueryRow(context.Background(), `SELECT content FROM context_blocks
		WHERE category = 'index' AND title = $1 AND scope = $2 AND NOT is_archived`, "topic-map-"+scope, scope).Scan(&text); err != nil {
		t.Fatalf("read topic-map-%s: %v", scope, err)
	}
	return text
}

// TestBackgroundDigestIsNotStartedByItsOwnMap is the RED of the self-trigger
// (issue #525): one block written, then six debounce periods. In each, the
// ticker's step runs the digest when it is pending, and the notifications of
// the run's map write reach the scheduler after the run ended — the order in
// which, before the fix, the map's own notification made the digest pending
// again: six runs in six periods for one write. Now: one run.
func TestBackgroundDigestIsNotStartedByItsOwnMap(t *testing.T) {
	f := newSelfFixture(t)
	ctx := context.Background()

	f.write(t, f.scope, "a source block")
	f.flush(t)
	if !digestPending(f.s) {
		t.Fatal("the notification of a block write did not make the digest pending")
	}
	for period := 0; period < 6; period++ {
		f.debouncePassed()
		f.s.digestTick(ctx)
		f.flush(t) // the run's notifications, after its end
	}
	if got := f.runs(); got != 1 {
		t.Fatalf("one block write ran the background digest %d times in six debounce periods, want 1", got)
	}
	if digestPending(f.s) {
		t.Error("the digest is still pending after its run — the next period runs it again")
	}
	if !strings.Contains(f.mapText(t, f.scope), "a source block") {
		t.Error("the map does not list the written block")
	}
}

// TestBackgroundDigestKeepsAWriteThatArrivesDuringTheRun is the counter-check
// (issue #525), the one a fix that drops the notifications of a run's window
// fails: a block written after the run's scan, whose notification reaches the
// scheduler while the run is still under way, keeps the digest pending, and
// the next run lists it. RED before the fix: the run's end cleared the digest
// after any success, and the block stayed out of the map until the next write.
// The run's own map notification arrives in the same window, before the run
// knows the map's id (the map's first write).
func TestBackgroundDigestKeepsAWriteThatArrivesDuringTheRun(t *testing.T) {
	f := newSelfFixture(t)
	ctx := context.Background()

	f.write(t, f.scope, "a source block")
	f.flush(t)
	f.s.runDigestFn = func(ctx context.Context, pool *pgxpool.Pool, reg *blocktype.Registry,
		mode, language, tenantScope, homeScope string, window []string) (digest.Result, error) {
		res, err := digest.RunDigest(ctx, pool, reg, mode, language, tenantScope, homeScope, window)
		f.write(t, f.scope, "written while the digest ran")
		f.flush(t) // the map's and the block's notifications, before the run ends
		return res, err
	}
	f.debouncePassed()
	f.s.digestTick(ctx)
	f.s.runDigestFn = nil
	if strings.Contains(f.mapText(t, f.scope), "written while the digest ran") {
		t.Fatal("fixture: the block written after the scan is in the run's map")
	}
	if !digestPending(f.s) {
		t.Fatal("a block written while the digest ran left the digest not pending — the map lacks it until the next write")
	}

	f.debouncePassed()
	f.s.digestTick(ctx)
	f.flush(t)
	if !strings.Contains(f.mapText(t, f.scope), "written while the digest ran") {
		t.Fatalf("the run after the write does not list the block:\n%s", f.mapText(t, f.scope))
	}
	f.debouncePassed()
	f.s.digestTick(ctx)
	if got := f.runs(); got != 2 {
		t.Errorf("%d runs, want 2: one for the first write, one for the write during it, none for the maps", got)
	}
}

// TestBackgroundDigestOwnMapDuringTheRunIsNoWrite: the notification of a
// run's own map — its first write, whose id the run reports only when the
// write returned — reaches the scheduler while the run is under way. It is
// the run's own write and leaves the digest settled; a fix that keeps the
// digest pending for every notification of a run's window runs it again.
// (Not red before the fix, which cleared the digest after every success.)
func TestBackgroundDigestOwnMapDuringTheRunIsNoWrite(t *testing.T) {
	f := newSelfFixture(t)
	ctx := context.Background()

	f.write(t, f.scope, "a source block")
	f.flush(t)
	f.s.runDigestFn = func(ctx context.Context, pool *pgxpool.Pool, reg *blocktype.Registry,
		mode, language, tenantScope, homeScope string, window []string) (digest.Result, error) {
		res, err := digest.RunDigest(ctx, pool, reg, mode, language, tenantScope, homeScope, window)
		f.flush(t) // the map's notifications, before the run ends
		return res, err
	}
	f.debouncePassed()
	f.s.digestTick(ctx)
	f.s.runDigestFn = nil
	if digestPending(f.s) {
		t.Fatal("the run's own map, notified while the run was under way, left the digest pending")
	}
	f.debouncePassed()
	f.s.digestTick(ctx)
	f.flush(t)
	if got := f.runs(); got != 1 {
		t.Errorf("%d runs for one block write, want 1", got)
	}
}

// TestBackgroundDigestHeldTenantIsNotRunByAnotherTenantsMap: with two
// tenants, one held (its map refused with 54000, issue #518) and one whose
// map is written, the written map's notification is no block write: the held
// tenant stays held. RED before the fix: every late map notification moved
// the write generation, so the held tenant ran along each period.
func TestBackgroundDigestHeldTenantIsNotRunByAnotherTenantsMap(t *testing.T) {
	f := newSelfFixture(t, backgroundTenant{scope: "t525", owned: []string{"t525"}})
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		CREATE SEQUENCE digest_attempts;
		CREATE FUNCTION digest_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM nextval('digest_attempts');
			RAISE EXCEPTION 'injected refusal of the topic map' USING ERRCODE = '54000';
		END $$;
		CREATE TRIGGER trg_digest_refuse BEFORE INSERT OR UPDATE ON context_blocks
			FOR EACH ROW WHEN (NEW.title = 'topic-map-t525') EXECUTE FUNCTION digest_refuse();`); err != nil {
		t.Fatalf("inject: %v", err)
	}
	attempts := func() int64 {
		var last int64
		var called bool
		if err := f.pool.QueryRow(ctx, `SELECT last_value, is_called FROM digest_attempts`).Scan(&last, &called); err != nil {
			t.Fatalf("read attempts: %v", err)
		}
		if !called {
			return 0
		}
		return last
	}

	f.write(t, f.scope, "a source block")
	f.write(t, "t525", "a block of the held tenant")
	f.flush(t)
	for period := 0; period < 6; period++ {
		f.debouncePassed()
		f.s.digestTick(ctx)
		f.flush(t)
	}
	if got := attempts(); got != 1 {
		t.Fatalf("the held tenant's map was attempted %d times in six periods, want 1 — another tenant's map write ran it along", got)
	}
	if got := f.logs.count("scheduler: running digest", "scope=private"); got != 1 {
		t.Errorf("%d runs of the default tenant for one write, want 1", got)
	}
}

// TestBackgroundDigestSettlesUnderConcurrentWrites (stress; run it with
// -race): blocks written from four goroutines while the ticker's step runs
// the digest and the listener hands every notification on at once. Once the
// writes and their notifications are through, the digest settles within the
// runs the last writes call for, its map lists every block, and nothing runs
// it again.
func TestBackgroundDigestSettlesUnderConcurrentWrites(t *testing.T) {
	f := newSelfFixture(t)
	ctx := context.Background()
	f.gate.setAuto(true)

	const writers, each = 4, 25
	ids := make(chan string, writers*each)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ids <- f.write(t, f.scope, fmt.Sprintf("stress %d-%02d", w, i))
			}
		}(w)
	}
	stop := make(chan struct{})
	ticking := make(chan struct{})
	go func() {
		defer close(ticking)
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.debouncePassed()
			f.s.digestTick(ctx)
		}
	}()
	wg.Wait()
	close(stop)
	<-ticking
	close(ids)

	want := map[string]bool{}
	for id := range ids {
		want[id] = true
	}
	deadline := time.After(gateWait)
	for len(want) > 0 {
		select {
		case id := <-f.gate.seen:
			delete(want, id)
		case <-deadline:
			t.Fatalf("%d written blocks were never notified", len(want))
		}
	}
	f.gate.setAuto(false)
	f.flush(t)
	for i := 0; i < 3 && digestPending(f.s); i++ {
		f.debouncePassed()
		f.s.digestTick(ctx)
		f.flush(t)
	}
	if digestPending(f.s) {
		t.Fatal("the digest is still pending three runs after the last write")
	}
	text := f.mapText(t, f.scope)
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			if title := fmt.Sprintf("stress %d-%02d", w, i); !strings.Contains(text, title) {
				t.Fatalf("the settled map lacks %q", title)
			}
		}
	}
	runs := f.runs()
	f.debouncePassed()
	f.s.digestTick(ctx)
	f.flush(t)
	if got := f.runs(); got != runs {
		t.Errorf("the settled digest ran again (%d runs, then %d) without a write", runs, got)
	}
}
