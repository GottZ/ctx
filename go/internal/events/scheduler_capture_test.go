package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
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

// captureTestConfig builds a Validate-clean config generation whose dream
// pipeline points at the given hosts. Self-checked against config.Validate so
// the fixture cannot drift from the ERROR classes — generation B must pass
// the same Validate inside store.Replace, or the flip under test would be
// rejected instead of observed. Hosts use RFC-2606 documentation names where
// no wire traffic occurs (fixture hygiene rule, public repo).
func captureTestConfig(t *testing.T, dreamHost, dreamModel, embedHost string) *config.Config {
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
			Host: embedHost, Protocol: backends.ProtocolOllama, Model: "embed-model",
		},
		Dream: config.DreamConfig{
			Enabled: true, Host: dreamHost, Protocol: backends.ProtocolOllama,
			Model: dreamModel, IdleWait: 20 * time.Second, Parallelism: 1,
			Backoff: config.BackoffConfig{
				Mode: "exp", Factor: 1.6, Grace: 0,
				CapHours: config.Hours(1080), MinHours: config.Hours(12), InertOffset: 7,
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
	chatB   backends.Backend
	embedB  backends.Backend
	numCtx  int
	wireErr error
}

// TestDreamLoopSeesReplacedConfigNextCycle is the F1-W6 capture regression
// test (design/01-config-core.md §5): the dream loop derives its backend
// tuples from a fresh store snapshot at every loop-body start, so a
// store.Replace between cycles is fully visible to the next cycle.
//
// Mechanics: the dreamCycleFunc seam stands in for the DB-bound pipeline
// stages (pick/keywords/RRF) and fires ONE real llm.ChatJSON against the
// chatB tuple it received — the httptest fake backends therefore prove the
// snapshot values reach the wire (host hit + model in the request body), not
// merely the parameter list. The seam holds each cycle open on a channel so
// the test can Replace the store strictly between cycle 1 and cycle 2.
//
// Negatively probed (Pflicht-Reihenfolge „erst rot", §5): against the pre-W6
// shape — one snapshot hoisted above the loop, the boot-copy pattern this
// wave deletes — cycle 2 still hits the generation-A host with the
// generation-A model and the test fails on every cycle-2 assertion. With the
// per-cycle snapshot it is green.
func TestDreamLoopSeesReplacedConfigNextCycle(t *testing.T) {
	srvA := newFakeOllama(t)
	srvB := newFakeOllama(t)

	cfgA := captureTestConfig(t, srvA.srv.URL, "dream-model-a", "http://embed-a.example")
	cfgB := captureTestConfig(t, srvB.srv.URL, "dream-model-b", "http://embed-b.example")

	store := config.NewStore(cfgA)
	s := NewScheduler(deadPool(t), store, nil, StartupConfig{})

	got := make(chan cycleObs, 4)
	release := make(chan struct{})
	s.runCycle = func(ctx context.Context, _ *pgxpool.Pool, embedB, chatB backends.Backend, opts llm.Options, _ dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		_, err := llm.ChatJSON(ctx, chatB, "sys", "user", opts, 5*time.Second)
		got <- cycleObs{chatB: chatB, embedB: embedB, numCtx: opts.NumCtx, wireErr: err}
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

	// Cycle 1 runs on generation A.
	obs1 := waitObs("cycle 1")
	if obs1.wireErr != nil {
		t.Fatalf("cycle 1 wire call: %v", obs1.wireErr)
	}
	if obs1.chatB.Host != srvA.srv.URL || obs1.chatB.Model != "dream-model-a" {
		t.Errorf("cycle 1 chatB = (%s, %s), want (%s, dream-model-a)",
			obs1.chatB.Host, obs1.chatB.Model, srvA.srv.URL)
	}
	if obs1.embedB.Host != "http://embed-a.example" {
		t.Errorf("cycle 1 embedB.Host = %s, want http://embed-a.example", obs1.embedB.Host)
	}
	// Delta 1 at scheduler level: Dream.NumCtx unset → cycle opts inherit
	// Chat.NumCtx through cfg.DreamBackend().
	if obs1.numCtx != 4096 {
		t.Errorf("cycle 1 opts.NumCtx = %d, want 4096 (inherited from chat — Delta 1)", obs1.numCtx)
	}
	if hits := srvA.hits(); len(hits) != 1 || hits[0] != "dream-model-a" {
		t.Errorf("backend A hits = %v, want [dream-model-a]", hits)
	}

	// Flip the store to generation B strictly between the cycles, then let
	// cycle 1 return so the loop takes its next snapshot.
	if err := store.Replace(cfgB); err != nil {
		t.Fatalf("store.Replace(cfgB): %v", err)
	}
	release <- struct{}{}

	// Cycle 2 MUST run on generation B: new host hit, new model on the wire.
	obs2 := waitObs("cycle 2")
	if obs2.wireErr != nil {
		t.Fatalf("cycle 2 wire call: %v", obs2.wireErr)
	}
	if obs2.chatB.Host != srvB.srv.URL || obs2.chatB.Model != "dream-model-b" {
		t.Errorf("cycle 2 chatB = (%s, %s), want (%s, dream-model-b) — boot-copy capture would still see generation A",
			obs2.chatB.Host, obs2.chatB.Model, srvB.srv.URL)
	}
	if obs2.embedB.Host != "http://embed-b.example" {
		t.Errorf("cycle 2 embedB.Host = %s, want http://embed-b.example", obs2.embedB.Host)
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
