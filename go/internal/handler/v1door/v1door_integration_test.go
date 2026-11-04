//go:build integration

// Self-test of the door gate (W01-6): green against a correct probe mount,
// red against a probe mount with an extra handler.Auth route and against
// handler.Auth in the group — each for /v1 and for /v2.
//
//	go test -tags=integration -p 1 -count=1 -run TestV1Door -v ./internal/handler/v1door/
package v1door

import (
	"context"
	"fmt"
	"log/slog"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// recTB records failures instead of failing the real test, so the
// self-test can require Assert to fail.
type recTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recTB) Helper() {}
func (r *recTB) Errorf(f string, a ...any) {
	r.mu.Lock()
	r.errs = append(r.errs, fmt.Sprintf(f, a...))
	r.mu.Unlock()
}
func (r *recTB) Fatalf(f string, a ...any) { r.Errorf(f, a...); runtime.Goexit() }

func runRecorded(t *testing.T, f func(tb testing.TB)) []string {
	rt := &recTB{TB: t}
	done := make(chan struct{})
	go func() { defer close(done); f(rt) }()
	<-done
	return rt.errs
}

type doorEnv struct {
	pool    *pgxpool.Pool
	tracer  *KnowledgeAuthTracer
	enabled atomic.Bool
	gate    func(http.Handler) http.Handler
	door    *handler.ModelDoor
	opts    Options
}

func setup(t *testing.T) *doorEnv {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, knowledge, err := store.CreateApiKey(ctx, seed, "v1door-knowledge", "private", nil, "")
	if err != nil {
		t.Fatalf("knowledge key: %v", err)
	}
	_, _, model, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "v1door"}, nil, store.APIStamp(admin.ID, "v1door"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	e := &doorEnv{tracer: &KnowledgeAuthTracer{}}
	cfg.ConnConfig.Tracer = e.tracer
	if e.pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(e.pool.Close)
	base := config.Defaults().GatewaySettings()
	get := func() oai.Settings { s := base; s.Enabled = e.enabled.Load(); return s }
	e.gate = oai.EnabledGate(get)
	e.door = handler.ModelAuth(handler.ModelAuthConfig{
		Pool: e.pool, Settings: get, Usage: nopUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	e.opts = Options{SetEnabled: e.enabled.Store, KnowledgeKey: knowledge, KnowledgeAuth: e.tracer, ModelKey: model.Plaintext()}
	return e
}

type nopUsage struct{}

func (nopUsage) Touch(string) {}

// group mounts prefix with the given door and the probe behind every route.
func group(r chi.Router, prefix string, gate, door func(http.Handler) http.Handler, probe http.Handler) {
	r.Route(prefix, func(g chi.Router) {
		g.Use(gate, door)
		if prefix == "/v2" {
			g.Method(http.MethodPost, "/rerank", probe)
			return
		}
		g.Method(http.MethodGet, "/models", probe)
		g.Method(http.MethodGet, "/models/{model}", probe)
		g.Method(http.MethodPost, "/chat/completions", probe)
	})
}

func TestV1DoorSelfTest_Integration(t *testing.T) {
	e := setup(t)
	knowledgeDoor := handler.Auth(e.pool)

	correct := func(r chi.Router, probe http.Handler) {
		group(r, "/v1", e.gate, e.door.Handler, probe)
		group(r, "/v2", e.gate, e.door.Handler, probe)
	}
	if errs := runRecorded(t, func(tb testing.TB) { Assert(tb, correct, e.opts) }); len(errs) != 0 {
		t.Fatalf("correct probe mount: %d failure(s):\n%s", len(errs), strings.Join(errs, "\n"))
	}
	t.Log("correct probe mount: green")

	for _, pfx := range []string{"/v1", "/v2"} {
		bad := map[string]func(r chi.Router, probe http.Handler){
			"extra handler.Auth route": func(r chi.Router, probe http.Handler) {
				correct(r, probe)
				r.With(knowledgeDoor).Get(pfx+"/extra", probe.ServeHTTP)
			},
			"handler.Auth in the group": func(r chi.Router, probe http.Handler) {
				for _, p := range []string{"/v1", "/v2"} {
					door := e.door.Handler
					if p == pfx {
						door = knowledgeDoor
					}
					group(r, p, e.gate, door, probe)
				}
			},
		}
		for name, mount := range bad {
			errs := runRecorded(t, func(tb testing.TB) { Assert(tb, mount, e.opts) })
			t.Logf("%s under %s: %d failure(s); first: %s", name, pfx, len(errs), first(errs))
			if len(errs) == 0 {
				t.Errorf("%s under %s: the door gate stayed green", name, pfx)
			}
		}
	}

	// A mount that claims a prefix without a route is refused, not passed.
	empty := func(r chi.Router, probe http.Handler) { group(r, "/v1", e.gate, e.door.Handler, probe) }
	if errs := runRecorded(t, func(tb testing.TB) { Assert(tb, empty, e.opts) }); len(errs) == 0 || !strings.Contains(errs[0], "no route under /v2") {
		t.Errorf("mount without /v2 routes: %v, want the empty-census refusal", errs)
	}
}

func first(errs []string) string {
	if len(errs) == 0 {
		return "—"
	}
	return errs[0]
}
