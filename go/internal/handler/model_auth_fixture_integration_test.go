//go:build integration

package handler

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// madTag tags a request context; madTracer counts the statements issued
// under each tag, so "0 DB queries" is measured per request.
type madTag struct{}

type madProbe struct {
	tag string
	at  time.Time
}

// madTracer records every statement per tag and every ctx_model_auth call
// (with tag and start time) — the probe count of gate (c).
type madTracer struct {
	mu     sync.Mutex
	byTag  map[string][]string
	probes []madProbe
	live   atomic.Int64 // ctx_model_auth_live statements (W01-8 sweep cost)
}

func (m *madTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	tag, _ := ctx.Value(madTag{}).(string)
	m.mu.Lock()
	if tag != "" {
		m.byTag[tag] = append(m.byTag[tag], d.SQL)
	}
	if strings.Contains(d.SQL, "ctx_model_auth_live(") {
		m.live.Add(1)
	}
	if strings.Contains(d.SQL, "ctx_model_auth(") {
		m.probes = append(m.probes, madProbe{tag: tag, at: time.Now()})
	}
	m.mu.Unlock()
	return ctx
}

func (m *madTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (m *madTracer) of(tag string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.byTag[tag]...)
}

func (m *madTracer) probesOf(prefix string) []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []time.Time
	for _, p := range m.probes {
		if strings.HasPrefix(p.tag, prefix) {
			out = append(out, p.at)
		}
	}
	return out
}

// madSettings is a hot settings source: tests change the auth part while
// the door runs.
type madSettings struct {
	mu sync.Mutex
	s  oai.Settings
}

func (m *madSettings) get() oai.Settings { m.mu.Lock(); defer m.mu.Unlock(); return m.s }
func (m *madSettings) auth(f func(a *oai.AuthSettings)) {
	m.mu.Lock()
	f(&m.s.Auth)
	m.mu.Unlock()
}

// madUsage records Touch calls (store.ModelKeyUsage's batching is W01-3's).
type madUsage struct {
	mu   sync.Mutex
	keys []string
}

func (u *madUsage) Touch(id string) { u.mu.Lock(); u.keys = append(u.keys, id); u.mu.Unlock() }

var _ ModelKeyToucher = (*store.ModelKeyUsage)(nil)

// madLogs is a concurrency-safe log sink.
type madLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *madLogs) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.Write(p) }
func (l *madLogs) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

type madEnv struct {
	seed     *pgxpool.Pool
	dsn      string // the test database (a LISTEN connection of its own, W01-8)
	pool     *pgxpool.Pool // traced, production-sized (MaxConns 20)
	tracer   *madTracer
	settings *madSettings
	usage    *madUsage
	logs     *madLogs
	stamp    store.AuditStamp
	labels   atomic.Int64
}

func madSetup(t *testing.T) *madEnv {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, seed, "mad-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 20 // store/pool.go tunePool
	tr := &madTracer{byTag: map[string][]string{}}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &madEnv{
		seed: seed, dsn: dsn, pool: pool, tracer: tr,
		settings: &madSettings{s: config.Defaults().GatewaySettings()},
		usage:    &madUsage{}, logs: &madLogs{},
		stamp: store.APIStamp(admin.ID, "mad"),
	}
}

// madRecorder is the probe handler behind the door.
type madRecorder struct {
	mu      sync.Mutex
	reached int
	ca      *auth.ConsumerAuth
	ar      *auth.AuthResult
	ctx     context.Context
}

func (p *madRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.reached++
	p.ca = ConsumerAuthFromContext(r.Context())
	p.ar = AuthResultFromContext(r.Context())
	p.ctx = r.Context()
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (p *madRecorder) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.reached }

// madReject is the test renderer (the real one is W02-02's, K-02-1).
func madReject(w http.ResponseWriter, _ *http.Request, rej ModelAuthRejection) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rej.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": rej.Code}})
}

func (e *madEnv) door(t *testing.T, trustedProxy string) (*ModelDoor, http.Handler, *madRecorder) {
	t.Helper()
	d := ModelAuth(ModelAuthConfig{
		Pool: e.pool, Settings: e.settings.get, TrustedProxy: trustedProxy,
		Usage: e.usage, Reject: madReject, Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
	})
	rec := &madRecorder{}
	return d, d.Handler(rec), rec
}

// consumer creates a consumer of the default tenant with one key.
func (e *madEnv) consumer(t *testing.T, spec store.ModelConsumerSpec) (store.ModelConsumer, store.ModelKeyInfo, modelkey.Key) {
	t.Helper()
	if spec.TenantID == "" {
		spec.TenantID = store.DefaultTenantID
	}
	if spec.Label == "" {
		spec.Label = fmt.Sprintf("mad-%d", e.labels.Add(1))
	}
	c, k, key, err := store.CreateConsumerWithKey(context.Background(), e.seed, spec, nil, e.stamp)
	if err != nil {
		t.Fatalf("create consumer %s: %v", spec.Label, err)
	}
	return c, k, key
}

// madReq is one request through the door: TLS on unless plain, RemoteAddr
// from ip, tag on the context.
type madReq struct {
	ip     string
	bearer string
	plain  bool
	hdr    map[string]string
	lines  map[string][]string // header lines in arrival order (several lines of one name)
	tag    string
}

func madDo(h http.Handler, q madReq) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if q.ip == "" {
		q.ip = "192.0.2.1"
	}
	if strings.Contains(q.ip, ":") {
		r.RemoteAddr = "[" + q.ip + "]:40000"
	} else {
		r.RemoteAddr = q.ip + ":40000"
	}
	if !q.plain {
		r.TLS = &tls.ConnectionState{}
	} else {
		r.TLS = nil
	}
	if q.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+q.bearer)
	}
	for k, v := range q.hdr {
		r.Header.Set(k, v)
	}
	for k, vs := range q.lines {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	if q.tag != "" {
		r = r.WithContext(context.WithValue(r.Context(), madTag{}, q.tag))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func madCode(rec *httptest.ResponseRecorder) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Error.Code
}

// forged returns n distinct CRC-valid keys no database row holds.
func forged(t *testing.T, n int) []modelkey.Key {
	t.Helper()
	out := make([]modelkey.Key, n)
	for i := range out {
		k, err := modelkey.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		out[i] = k
	}
	return out
}

// parallel runs f(i) for i in [0, n) on w workers.
func parallel(n, w int, f func(i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range w {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				f(i)
			}
		}()
	}
	wg.Wait()
}

// httptestRequestWithContext is a TLS request carrying key as Bearer on ctx.
func httptestRequestWithContext(ctx context.Context, key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	r.RemoteAddr = "192.0.2.1:40000"
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

func newDiscard() *httptest.ResponseRecorder { return httptest.NewRecorder() }
