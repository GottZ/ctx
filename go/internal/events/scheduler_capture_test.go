package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeOllama is one httptest LLM backend: an /api/chat endpoint that records
// the model of every request it serves.
type fakeOllama struct {
	srv    *httptest.Server
	mu     sync.Mutex
	models []string
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.models = append(f.models, req.Model)
		f.mu.Unlock()
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"{}"},"eval_count":1,"prompt_eval_count":1}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// hits returns a copy of the models served so far.
func (f *fakeOllama) hits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.models))
	copy(out, f.models)
	return out
}

// captureTestConfig builds a Validate-clean config generation whose back-off
// policy carries the given MinHours marker — the config-snapshot axis of the
// capture test (backend tuples moved to the pool axis in G28). Self-checked
// against config.Validate so the fixture cannot drift from the ERROR classes
// — generation B must pass the same Validate inside store.Replace, or the
// flip under test would be rejected instead of observed. Hosts use RFC-2606
// documentation names — no wire traffic runs through the config tuples since
// G28 (fixture hygiene rule, public repo).
func captureTestConfig(t *testing.T, backoffMinHours float64) *config.Config {
	t.Helper()
	c := &config.Config{
		Server: config.ServerConfig{
			DB: "ctx", DBUser: "ctx", DBPass: "documentation-value",
			DBHost: "db.example", DBPort: 5432, DBSSL: "disable", ListenAddr: ":0",
		},
		Chat: config.ChatConfig{
			Host: "http://chat.example", Protocol: backends.ProtocolOllama,
			Model: "chat-model", NumCtx: 4096, Think: "false",
		},
		Fallback: config.FallbackConfig{Protocol: backends.ProtocolOpenAI},
		Embed: config.EmbedConfig{
			Host: "http://embed.example", Protocol: backends.ProtocolOllama, Model: "embed-model",
		},
		Dream: config.DreamConfig{
			Enabled: true, Host: "http://dream.example", Protocol: backends.ProtocolOllama,
			Model: "dream-model", IdleWait: 20 * time.Second, Parallelism: 1,
			Backoff: config.BackoffConfig{
				Mode: "exp", Factor: 1.6, Grace: 0,
				CapHours: config.Hours(1080), MinHours: config.Hours(backoffMinHours), InertOffset: 7,
			},
		},
		Graph: config.GraphConfig{HopDepth: 1},
		Query: config.QueryConfig{
			ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: "v5.2",
		},
		Scheduler: config.SchedulerConfig{
			ReadScopes: []string{"private"}, HomeScope: "private",
		},
	}
	if issues := config.Validate(c); config.HasErrors(issues) {
		t.Fatalf("capture fixture is not Validate-clean: %+v", issues)
	}
	return c
}

// dreamPoolRow builds one enabled full-trust pool row carrying the dream
// role, pointed at the given host/model — one generation of the pool axis.
func dreamPoolRow(host, model string) backends.Backend {
	return backends.Backend{
		ID: "row-" + model, Name: "row-" + model,
		Host: host, Protocol: backends.ProtocolOllama,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: model}},
		Priority: 100, Enabled: true,
	}
}

// deadPool returns a lazily-connecting pool aimed at a closed loopback port.
// The dream-loop's embed-backfill stage needs a non-nil pool; against this
// one it fails open instantly (ECONNREFUSED) and the loop proceeds to the
// dream cycle — the stage under test.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://ctx:documentation-value@127.0.0.1:1/ctx?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("deadPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// cycleObs is what the seam observed for one dream cycle.
type cycleObs struct {
	host     string
	model    string
	backoff  dream.BackoffConfig
	chainErr error
	wireErr  error
}

// TestDreamLoopSeesReplacedConfigNextCycle is the capture regression test
// (F1-W6 origin, G28 shape): every dream cycle derives from FRESH state on
// both hot axes — the config-store snapshot (back-off policy, scopes) and
// the backend-pool snapshot (which backend the dream chain resolves to). A
// store.Replace or pool snapshot swap strictly between two cycles is fully
// visible to the second cycle.
//
// Mechanics: the dreamCycleFunc seam stands in for the DB-bound pipeline
// stages (pick/keywords/RRF). It resolves the dream chain through the
// router the scheduler handed it and fires ONE real llm.ChatJSON against the
// resolved row — the httptest fake backends therefore prove the pool
// generation reaches the wire (host hit + model in the request body), not
// merely a parameter list. The seam holds each cycle open on a channel so
// the test can flip both axes strictly between cycle 1 and cycle 2.
//
// Negatively probed (Pflicht-Reihenfolge „erst rot", §5): against a hoisted
// chain resolution or a boot-copied back-off, cycle 2 still observes the
// generation-A host/model/MinHours and fails on every cycle-2 assertion.
func TestDreamLoopSeesReplacedConfigNextCycle(t *testing.T) {
	srvA := newFakeOllama(t)
	srvB := newFakeOllama(t)

	cfgA := captureTestConfig(t, 12)
	cfgB := captureTestConfig(t, 24)

	store := config.NewStore(cfgA)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow(srvA.srv.URL, "dream-model-a")})
	s := NewScheduler(deadPool(t), store, bpool, StartupConfig{})

	got := make(chan cycleObs, 4)
	release := make(chan struct{})
	s.runCycle = func(ctx context.Context, _ *pgxpool.Pool, r *dream.Router, opts llm.Options, backoff dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		obs := cycleObs{backoff: backoff}
		chain, err := r.Pool.Chain(backends.RoleDream, backends.SensPublic, "")
		if err != nil {
			obs.chainErr = err
		} else {
			b := chain[0]
			b.Model = b.ModelFor(backends.RoleDream).Model
			obs.host, obs.model = b.Host, b.Model
			_, obs.wireErr = llm.ChatJSON(ctx, b, "sys", "user", opts, 5*time.Second)
		}
		got <- obs
		<-release // hold the cycle open until the test releases it
		return 1, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runDreamLoop(ctx)
		close(done)
	}()

	waitObs := func(stage string) cycleObs {
		t.Helper()
		select {
		case o := <-got:
			return o
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: no dream cycle within 15s", stage)
			return cycleObs{}
		}
	}

	// Cycle 1 runs on generation A of both axes.
	obs1 := waitObs("cycle 1")
	if obs1.chainErr != nil || obs1.wireErr != nil {
		t.Fatalf("cycle 1: chain=%v wire=%v", obs1.chainErr, obs1.wireErr)
	}
	if obs1.host != srvA.srv.URL || obs1.model != "dream-model-a" {
		t.Errorf("cycle 1 backend = (%s, %s), want (%s, dream-model-a)",
			obs1.host, obs1.model, srvA.srv.URL)
	}
	if obs1.backoff.MinHours != 12 {
		t.Errorf("cycle 1 backoff.MinHours = %v, want 12 (config generation A)", obs1.backoff.MinHours)
	}
	if hits := srvA.hits(); len(hits) != 1 || hits[0] != "dream-model-a" {
		t.Errorf("backend A hits = %v, want [dream-model-a]", hits)
	}

	// Flip BOTH axes strictly between the cycles, then let cycle 1 return so
	// the loop takes its next snapshot + router.
	if err := store.Replace(cfgB); err != nil {
		t.Fatalf("store.Replace(cfgB): %v", err)
	}
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow(srvB.srv.URL, "dream-model-b")})
	release <- struct{}{}

	// Cycle 2 MUST run on generation B: new host hit, new model on the wire,
	// new back-off policy.
	obs2 := waitObs("cycle 2")
	if obs2.chainErr != nil || obs2.wireErr != nil {
		t.Fatalf("cycle 2: chain=%v wire=%v", obs2.chainErr, obs2.wireErr)
	}
	if obs2.host != srvB.srv.URL || obs2.model != "dream-model-b" {
		t.Errorf("cycle 2 backend = (%s, %s), want (%s, dream-model-b) — a hoisted chain would still see generation A",
			obs2.host, obs2.model, srvB.srv.URL)
	}
	if obs2.backoff.MinHours != 24 {
		t.Errorf("cycle 2 backoff.MinHours = %v, want 24 (config generation B) — a boot copy would still see 12", obs2.backoff.MinHours)
	}
	if hits := srvB.hits(); len(hits) != 1 || hits[0] != "dream-model-b" {
		t.Errorf("backend B hits = %v, want [dream-model-b]", hits)
	}
	if hits := srvA.hits(); len(hits) != 1 {
		t.Errorf("backend A hits after flip = %v, want exactly the one cycle-1 hit", hits)
	}

	// Shut the loop down: cancel first, then release cycle 2 — the loop's
	// next shutdown check exits before any third cycle.
	cancel()
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}
}

// TestBackgroundTenantScopesFallsBackOnDBError pins the graceful-degradation
// contract: when the tenant register is unreachable — a pre-MT-schema deploy
// where context_tenants does not exist yet, OR a transient DB error — the
// background iterates exactly [_global] (the tenant-less single pass, where
// SnapshotForTenant(_global) returns base), never an empty list that would
// silently stall all background work. deadPool refuses every connection, so
// store.ListTenants errors and the fallback fires without a container.
func TestBackgroundTenantScopesFallsBackOnDBError(t *testing.T) {
	s := NewScheduler(deadPool(t), config.NewStore(captureTestConfig(t, 12)),
		backends.NewPool(nil, nil), StartupConfig{})
	got := s.backgroundTenantScopes(context.Background())
	if len(got) != 1 || got[0] != store.GlobalScope {
		t.Fatalf("backgroundTenantScopes on DB error = %v, want [%q]", got, store.GlobalScope)
	}
}

// TestRunDigestRetainsPendingOnTenantFailure pins the 06-C6 per-tenant digest
// loop's partial-failure contract: when ANY iterated tenant's RunDigest fails,
// digestPending stays SET so the next debounce retries — the pre-T13 semantics
// preserved per multi-tenant run (one tenant's failure must not silently clear
// the pending flag for all). deadPool makes every RunDigest fail (no DB), and
// the two-scope backgroundTenantsFn proves the loop iterates more than one tenant.
func TestRunDigestRetainsPendingOnTenantFailure(t *testing.T) {
	s := NewScheduler(deadPool(t), config.NewStore(captureTestConfig(t, 12)),
		backends.NewPool(nil, nil), StartupConfig{})
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant {
		return []backgroundTenant{{scope: store.GlobalScope}, {scope: "tenant-x"}}
	}

	s.mu.Lock()
	s.digestPending = true
	s.mu.Unlock()

	s.runDigest(context.Background())

	s.mu.Lock()
	pending := s.digestPending
	s.mu.Unlock()
	if !pending {
		t.Fatal("digestPending cleared after a failing digest run — want retained so the next debounce retries")
	}
}

// TestDreamLoopResolvesPerTenantGenerationEachCycle is the 06-C6 tenant arm of
// the capture regression: the background takes ONE SnapshotForTenant per cycle
// over the authoritative tenant list (not a single process-global Snapshot), so
// each cycle derives from the iterated tenant's config generation — and a
// tenant-generation flip (InvalidateTenant, the NOTIFY-dispatch path C4 will
// drive) strictly between two cycles is fully visible to the next cycle.
//
// Mechanics: an overlay yields a tenant-specific generation for scope "tenant-a"
// (a distinct back-off MinHours marker), and the backgroundTenantsFn seam drives the
// loop over ["tenant-a"] without a database. Cycle 1 MUST observe the tenant
// generation (MinHours=99), NOT the base generation (MinHours=12) — that is the
// RED arm: against the pre-C6 `s.cfg.Snapshot()` the loop saw base. After the
// flip + InvalidateTenant, cycle 2 MUST observe the rebuilt tenant generation
// (MinHours=33) — a hoisted/cached snapshot would still see 99.
func TestDreamLoopResolvesPerTenantGenerationEachCycle(t *testing.T) {
	srv := newFakeOllama(t)

	cfgBase := captureTestConfig(t, 12) // base generation (what a plain Snapshot() returns)
	cfgT1 := captureTestConfig(t, 99)   // tenant-a generation, before the flip
	cfgT2 := captureTestConfig(t, 33)   // tenant-a generation, after the flip

	st := config.NewStore(cfgBase)
	var flipped atomic.Bool
	st.SetOverlay(func(_ context.Context, _ *config.Config, tenantScope string) (*config.Config, error) {
		if tenantScope != "tenant-a" {
			return nil, nil // any other scope falls back to base
		}
		if flipped.Load() {
			return cfgT2, nil
		}
		return cfgT1, nil
	})

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow(srv.srv.URL, "dream-model")})
	s := NewScheduler(deadPool(t), st, bpool, StartupConfig{})
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return []backgroundTenant{{scope: "tenant-a"}} }

	got := make(chan cycleObs, 4)
	release := make(chan struct{})
	s.runCycle = func(ctx context.Context, _ *pgxpool.Pool, r *dream.Router, opts llm.Options, backoff dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		obs := cycleObs{backoff: backoff}
		chain, err := r.Pool.Chain(backends.RoleDream, backends.SensPublic, "")
		if err != nil {
			obs.chainErr = err
		} else {
			b := chain[0]
			b.Model = b.ModelFor(backends.RoleDream).Model
			obs.host, obs.model = b.Host, b.Model
			_, obs.wireErr = llm.ChatJSON(ctx, b, "sys", "user", opts, 5*time.Second)
		}
		got <- obs
		<-release
		return 1, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runDreamLoop(ctx)
		close(done)
	}()

	waitObs := func(stage string) cycleObs {
		t.Helper()
		select {
		case o := <-got:
			return o
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: no dream cycle within 15s", stage)
			return cycleObs{}
		}
	}

	// Cycle 1 runs on tenant-a's generation, NOT the base generation.
	obs1 := waitObs("cycle 1")
	if obs1.chainErr != nil || obs1.wireErr != nil {
		t.Fatalf("cycle 1: chain=%v wire=%v", obs1.chainErr, obs1.wireErr)
	}
	if obs1.backoff.MinHours != 99 {
		t.Errorf("cycle 1 backoff.MinHours = %v, want 99 (tenant-a generation) — a process-global Snapshot() would see the base 12", obs1.backoff.MinHours)
	}

	// Flip tenant-a's generation and drop its cached entry (the NOTIFY-dispatch
	// path), then let cycle 1 return so the loop takes its next per-tenant snapshot.
	flipped.Store(true)
	st.InvalidateTenant("tenant-a")
	release <- struct{}{}

	// Cycle 2 MUST observe the rebuilt tenant-a generation.
	obs2 := waitObs("cycle 2")
	if obs2.chainErr != nil || obs2.wireErr != nil {
		t.Fatalf("cycle 2: chain=%v wire=%v", obs2.chainErr, obs2.wireErr)
	}
	if obs2.backoff.MinHours != 33 {
		t.Errorf("cycle 2 backoff.MinHours = %v, want 33 (rebuilt tenant-a generation) — a hoisted/cached snapshot would still see 99", obs2.backoff.MinHours)
	}

	cancel()
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}
}
