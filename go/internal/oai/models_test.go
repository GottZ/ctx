package oai_test

// W02-05: GET /v1/models and GET /v1/models/* over a fixture registry and
// fake request-time dependencies (oai.Deps). The real stack — enabled gate,
// door, LoopGuard, body cap, budget — is the integration half in cmd/ctxd
// (v1_mount_integration_test.go); here the handlers alone: the release
// predicate VisibleTo ∧ (AllModels ∨ Grant(alias.ID)) (design/01 §3.8,
// masterplan K11), the one 404 (B3), the ctx-peer rule (V-02-c), the
// invariant check before everything else (design/02 §4.2 stage 6) and the
// element form (§4.4.3).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// aliasRow is one context_model_aliases row as the registry's load query
// returns it.
type aliasRow struct {
	id, scope, name, kind, policy string
	enabled                       bool
}

var aliasColumns = []string{"id", "scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled", "created_at", "updated_at"}

// aliasCreated is the created_at of every fixture row (the model's
// "created", Unix seconds).
var aliasCreated = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// Fixture ids (UUIDs, as the table holds them).
const (
	idGlobalChat  = "00000000-0000-4000-8000-00000000c001"
	idGlobalEmbed = "00000000-0000-4000-8000-00000000e001"
	idGlobalRank  = "00000000-0000-4000-8000-00000000f001"
	idSlash       = "00000000-0000-4000-8000-00000000a001"
	idTenantChat  = "00000000-0000-4000-8000-00000000c00a"
	idForeign     = "00000000-0000-4000-8000-00000000b00b"
	idDisabled    = "00000000-0000-4000-8000-00000000d00d"
	idBroken      = "00000000-0000-4000-8000-00000000bad0"
	idRecreated   = "00000000-0000-4000-8000-00000000c0c0"
)

const (
	tenantScope  = "acme:main"
	foreignScope = "beta:main"
)

// fixtureRows: three _global aliases plus one with a slash in its name; a
// tenant alias "chat" that shadows the _global one for acme; a foreign
// tenant's alias; a disabled and a broken (policy above its ceiling) alias.
func fixtureRows() []aliasRow {
	return []aliasRow{
		{idGlobalChat, "_global", "chat", "chat", `{"context_length":262144,"max_output_tokens":8192}`, true},
		{idGlobalEmbed, "_global", "embed", "embedding", `{"embedding.native_dims":4096,"embedding.matryoshka_min":32,"embedding.max_inputs":32}`, true},
		{idGlobalRank, "_global", "rank", "rerank", `{}`, true},
		{idSlash, "_global", "qwen/x", "chat", `{"context_length":32768}`, true},
		{idTenantChat, tenantScope, "chat", "chat", `{"context_length":8192}`, true},
		{idForeign, foreignScope, "secret", "chat", `{}`, true},
		{idDisabled, "_global", "off", "chat", `{}`, false},
		{idBroken, "_global", "broken", "chat", `{"max_n":99}`, true},
	}
}

// fixtureRegistry loads rows into a registry through a pgxmock pool.
func fixtureRegistry(t *testing.T, rows []aliasRow) *modelalias.Registry {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	reg := modelalias.NewRegistry(mock)
	reloadRegistry(t, reg, mock, rows)
	return reg
}

func reloadRegistry(t *testing.T, reg *modelalias.Registry, mock pgxmock.PgxPoolIface, rows []aliasRow) {
	t.Helper()
	out := pgxmock.NewRows(aliasColumns)
	for _, r := range rows {
		out.AddRow(r.id, r.scope, r.name, r.kind, r.kind, "public", []byte(r.policy), "", r.enabled, aliasCreated, aliasCreated)
	}
	mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(out)
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatalf("reload fixture: %v", err)
	}
}

// fakeDeps is oai.Deps over fixed values, counting every call.
type fakeDeps struct {
	ca        *auth.ConsumerAuth
	knowledge bool
	reg       *modelalias.Registry
	grants    map[string]bool // alias id → granted
	grantErr  error

	consumerCalls, grantIDCalls, hasGrantCalls atomic.Int64
}

func (f *fakeDeps) deps() oai.Deps {
	return oai.Deps{
		Consumer:      func(context.Context) *auth.ConsumerAuth { f.consumerCalls.Add(1); return f.ca },
		KnowledgeAuth: func(context.Context) bool { return f.knowledge },
		Aliases:       f.reg,
		GrantIDs: func(_ context.Context, consumerID string) ([]string, error) {
			f.grantIDCalls.Add(1)
			if f.grantErr != nil {
				return nil, f.grantErr
			}
			ids := []string{}
			for id, ok := range f.grants {
				if ok && consumerID == f.ca.ConsumerID {
					ids = append(ids, id)
				}
			}
			return ids, nil
		},
		HasGrant: func(_ context.Context, consumerID, aliasID string) (bool, error) {
			f.hasGrantCalls.Add(1)
			if f.grantErr != nil {
				return false, f.grantErr
			}
			return consumerID == f.ca.ConsumerID && f.grants[aliasID], nil
		},
	}
}

// appConsumer is a valid app consumer of acme without the wildcard.
func appConsumer() *auth.ConsumerAuth {
	return &auth.ConsumerAuth{ModelKeyID: "00000000-0000-4000-8000-0000000000k1", ConsumerID: "00000000-0000-4000-8000-0000000000c1",
		TenantID: "00000000-0000-4000-8000-0000000000t1", TenantScope: tenantScope, Kind: auth.KindApp, IsValid: true}
}

// modelsRouter mounts the two handlers the way the /v1 group does:
// GET /v1/models and the wildcard GET /v1/models/* (alias names may hold a
// slash, design/02 §3.1).
func modelsRouter(d oai.Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(g chi.Router) {
		g.Method(http.MethodGet, "/models", oai.ListModels(d))
		g.Method(http.MethodGet, "/models/*", oai.GetModel(d))
	})
	return r
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func get(t *testing.T, h http.Handler, path string) answer {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return answer{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

type listBody struct {
	Object string            `json:"object"`
	Data   []json.RawMessage `json:"data"`
}

type modelBody struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	OwnedBy string         `json:"owned_by"`
	Ctx     map[string]any `json:"ctx"`
}

// listIDs is GET /v1/models as the sorted ids it lists.
func listIDs(t *testing.T, h http.Handler) []string {
	t.Helper()
	a := get(t, h, "/v1/models")
	if a.status != http.StatusOK {
		t.Fatalf("GET /v1/models = %d %s, want 200", a.status, a.body)
	}
	var l listBody
	if err := json.Unmarshal(a.body, &l); err != nil || l.Object != "list" || l.Data == nil {
		t.Fatalf("list body %s: object=%q data=%v err=%v", a.body, l.Object, l.Data, err)
	}
	ids := make([]string, 0, len(l.Data))
	for _, raw := range l.Data {
		var m modelBody
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("element %s: %v", raw, err)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

// TestModelNotFoundIndistinguishable is B3 (design/02 §5): an alias that
// does not exist, one of a foreign tenant, one not granted (AllModels off,
// no grant), a disabled one and one whose policy failed validation answer
// byte-identically — status, headers (without X-Request-ID) and body:
// 404 model_not_found, one fixed text that names no model.
func TestModelNotFoundIndistinguishable(t *testing.T) {
	f := &fakeDeps{ca: appConsumer(), reg: fixtureRegistry(t, fixtureRows()), grants: map[string]bool{
		idForeign: true, idDisabled: true, idBroken: true, // grants held on aliases the consumer cannot see
	}}
	h := modelsRouter(f.deps())
	cases := map[string]string{
		"does not exist": "/v1/models/nope",
		"foreign tenant": "/v1/models/secret",
		"not granted":    "/v1/models/embed",
		"disabled":       "/v1/models/off",
		"broken policy":  "/v1/models/broken",
	}
	var ref *answer
	refName := ""
	for name, path := range cases {
		probes := f.hasGrantCalls.Load()
		a := get(t, h, path)
		// The same database work for every 404 (Fremd-Review 1 L-1): one
		// grant probe, whether the name is an alias or not — no timing
		// oracle beside the identical bytes.
		if n := f.hasGrantCalls.Load() - probes; n != 1 {
			t.Errorf("%s: %d grant probes, want 1 for every 404", name, n)
		}
		a.header.Del("X-Request-ID")
		var e struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    string  `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(a.body, &e); err != nil || a.status != http.StatusNotFound || e.Error.Code != oai.CodeModelNotFound ||
			e.Error.Type != oai.TypeInvalidRequest || e.Error.Param != nil {
			t.Errorf("%s: %d %s, want 404 model_not_found (invalid_request_error, param null)", name, a.status, a.body)
		}
		for _, word := range []string{"nope", "secret", "embed", "off", "broken"} {
			if strings.Contains(string(a.body), word) {
				t.Errorf("%s: body %s names %q — the 404 reflects request input", name, a.body, word)
			}
		}
		if ref == nil {
			ref, refName = &a, name
			continue
		}
		if a.status != ref.status || !bytes.Equal(a.body, ref.body) || !headersEqual(a.header, ref.header) {
			t.Errorf("%s differs from %s:\n  %d %v %s\n  %d %v %s", name, refName, a.status, a.header, a.body, ref.status, ref.header, ref.body)
		}
	}
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w := b[k]
		if len(v) != len(w) {
			return false
		}
		for i := range v {
			if v[i] != w[i] {
				return false
			}
		}
	}
	return true
}

// TestGrantsBindAliasIDs is K11 at request time (design/01 §3.8, B28): a
// grant names the alias ID. Alias "embed" granted, then deleted and created
// again under the same name (a new id) ⇒ the list no longer has it and
// GET /v1/models/embed is a 404 — the old grant does not carry over to the
// new alias.
func TestGrantsBindAliasIDs(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	reg := modelalias.NewRegistry(mock)
	reloadRegistry(t, reg, mock, fixtureRows())
	// The grant as an id-keyed store holds it and as a name-keyed one would:
	// only the id may count, so a release by name is caught after the
	// recreation below.
	f := &fakeDeps{ca: appConsumer(), reg: reg, grants: map[string]bool{idGlobalEmbed: true, "embed": true}}
	h := modelsRouter(f.deps())
	if got := strings.Join(listIDs(t, h), ","); got != "embed" {
		t.Fatalf("before: list %q, want embed", got)
	}
	if a := get(t, h, "/v1/models/embed"); a.status != http.StatusOK {
		t.Fatalf("before: GET embed = %d %s", a.status, a.body)
	}

	rows := fixtureRows()
	for i := range rows {
		if rows[i].id == idGlobalEmbed {
			rows[i].id = idRecreated
		}
	}
	reloadRegistry(t, reg, mock, rows)
	if got := listIDs(t, h); len(got) != 0 {
		t.Errorf("after delete + recreate: list %v, want empty — the grant of the old id released the new alias", got)
	}
	if a := get(t, h, "/v1/models/embed"); a.status != http.StatusNotFound {
		t.Errorf("after delete + recreate: GET embed = %d %s, want 404", a.status, a.body)
	}
}

// TestModelsPeerRuleAndShadowing is V-02-c plus the shadowing rule: for an
// app consumer of acme the tenant alias "chat" replaces the _global one;
// a ctx-peer of the same tenant resolves _global only and sees the _global
// row. A slash id resolves; AllModels off without a grant is an empty list.
func TestModelsPeerRuleAndShadowing(t *testing.T) {
	reg := fixtureRegistry(t, fixtureRows())
	ctxOf := func(h http.Handler, path string) map[string]any {
		t.Helper()
		a := get(t, h, path)
		var m modelBody
		if a.status != http.StatusOK || json.Unmarshal(a.body, &m) != nil {
			t.Fatalf("GET %s = %d %s", path, a.status, a.body)
		}
		return m.Ctx
	}

	app := &fakeDeps{ca: appConsumer(), reg: reg}
	app.ca.AllModels = true
	happ := modelsRouter(app.deps())
	if got := ctxOf(happ, "/v1/models/chat")["context_length"]; got != float64(8192) {
		t.Errorf("app consumer of acme: chat context_length %v, want 8192 (the tenant alias shadows _global)", got)
	}
	if got, want := strings.Join(listIDs(t, happ), ","), "chat,embed,qwen/x,rank"; got != want {
		t.Errorf("app consumer, all models: list %q, want %q (sorted by id, tenant chat once, no foreign, disabled or broken alias)", got, want)
	}

	peer := &fakeDeps{ca: appConsumer(), reg: reg}
	peer.ca.AllModels, peer.ca.Kind = true, auth.KindCtxPeer
	hpeer := modelsRouter(peer.deps())
	if got := ctxOf(hpeer, "/v1/models/chat")["context_length"]; got != float64(262144) {
		t.Errorf("ctx-peer of acme: chat context_length %v, want 262144 (a peer resolves _global only, V-02-c)", got)
	}

	// The peer's grant on the tenant alias releases nothing: it resolves the
	// _global "chat", whose id it does not hold.
	peerGrant := &fakeDeps{ca: appConsumer(), reg: reg, grants: map[string]bool{idTenantChat: true}}
	peerGrant.ca.Kind = auth.KindCtxPeer
	if a := get(t, modelsRouter(peerGrant.deps()), "/v1/models/chat"); a.status != http.StatusNotFound {
		t.Errorf("ctx-peer with a grant on the tenant alias: GET chat = %d %s, want 404", a.status, a.body)
	}

	for _, path := range []string{"/v1/models/qwen/x", "/v1/models/qwen%2Fx"} {
		a := get(t, happ, path)
		var m modelBody
		if a.status != http.StatusOK || json.Unmarshal(a.body, &m) != nil || m.ID != "qwen/x" {
			t.Errorf("GET %s = %d %s, want the alias qwen/x", path, a.status, a.body)
		}
	}

	none := &fakeDeps{ca: appConsumer(), reg: reg}
	a := get(t, modelsRouter(none.deps()), "/v1/models")
	if a.status != http.StatusOK || string(bytes.TrimSpace(a.body)) != `{"object":"list","data":[]}` {
		t.Errorf("AllModels off, no grant: %d %s, want {\"object\":\"list\",\"data\":[]}", a.status, a.body)
	}
	if none.grantIDCalls.Load() != 1 {
		t.Errorf("AllModels off: %d GrantIDs calls for one list, want 1 (one index range per list)", none.grantIDCalls.Load())
	}
}

// TestModelsDisabledAliasGrantReleasesNothing is the request-time half of
// W01-9 review 2 N-2: a grant can land on an alias deactivated between the
// admin's snapshot and the insert (the foreign key checks existence only);
// at request time such an alias is neither listed nor resolvable.
func TestModelsDisabledAliasGrantReleasesNothing(t *testing.T) {
	f := &fakeDeps{ca: appConsumer(), reg: fixtureRegistry(t, fixtureRows()), grants: map[string]bool{idDisabled: true, idBroken: true}}
	h := modelsRouter(f.deps())
	if got := listIDs(t, h); len(got) != 0 {
		t.Errorf("grants on a disabled and a broken alias: list %v, want empty", got)
	}
	for _, name := range []string{"off", "broken"} {
		if a := get(t, h, "/v1/models/"+name); a.status != http.StatusNotFound {
			t.Errorf("GET %s with a grant = %d %s, want 404", name, a.status, a.body)
		}
	}
}

// TestModelElementForm is design/02 §4.4.3: id = alias name, object model,
// created = created_at in Unix seconds, owned_by ctx, the ctx extension per
// kind with the policy's values — unset ones left out — and nothing of the
// backend (no role, no host, no backend model name).
func TestModelElementForm(t *testing.T) {
	f := &fakeDeps{ca: appConsumer(), reg: fixtureRegistry(t, fixtureRows())}
	f.ca.AllModels, f.ca.TenantScope = true, ""
	h := modelsRouter(f.deps())
	want := map[string]string{
		"chat":   `{"id":"chat","object":"model","created":1793707200,"owned_by":"ctx","ctx":{"kind":"chat","context_length":262144,"max_output_tokens":8192}}`,
		"embed":  `{"id":"embed","object":"model","created":1793707200,"owned_by":"ctx","ctx":{"kind":"embedding","dimensions":4096,"matryoshka_min":32,"max_inputs":32}}`,
		"rank":   `{"id":"rank","object":"model","created":1793707200,"owned_by":"ctx","ctx":{"kind":"rerank"}}`,
		"qwen/x": `{"id":"qwen/x","object":"model","created":1793707200,"owned_by":"ctx","ctx":{"kind":"chat","context_length":32768}}`,
	}
	if aliasCreated.Unix() != 1793707200 {
		t.Fatalf("fixture created_at = %d", aliasCreated.Unix())
	}
	a := get(t, h, "/v1/models")
	var l listBody
	if err := json.Unmarshal(a.body, &l); err != nil || len(l.Data) != len(want) {
		t.Fatalf("list %s: %v", a.body, err)
	}
	for _, raw := range l.Data {
		var m modelBody
		_ = json.Unmarshal(raw, &m)
		if string(raw) != want[m.ID] {
			t.Errorf("list element %s, want %s", raw, want[m.ID])
		}
		one := get(t, h, "/v1/models/"+m.ID)
		if string(bytes.TrimSpace(one.body)) != want[m.ID] || one.status != http.StatusOK {
			t.Errorf("GET %s = %d %s, want %s", m.ID, one.status, one.body, want[m.ID])
		}
		if ct := one.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type %q", m.ID, ct)
		}
	}
}

// logSink captures slog records (level and message).
type logSink struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (s *logSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *logSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	s.recs = append(s.recs, r)
	s.mu.Unlock()
	return nil
}
func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSink) WithGroup(string) slog.Handler      { return s }

func (s *logSink) errors() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.recs {
		if r.Level == slog.LevelError {
			n++
		}
	}
	return n
}

func captureLog(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return sink
}

// TestModelsInternalFaults is the handler half of G9 (masterplan K5/K29,
// design/02 §4.2 stage 6, L11): a broken invariant — no consumer, an
// invalid one, one without tenant, a knowledge AuthResult in the context —,
// a missing dependency and a failing grant read each answer 503
// internal_error (service_unavailable_error) with x-should-retry: false in
// the envelope, log an ERROR, and resolve nothing.
func TestModelsInternalFaults(t *testing.T) {
	reg := fixtureRegistry(t, fixtureRows())
	noTenant := appConsumer()
	noTenant.TenantID = ""
	invalid := appConsumer()
	invalid.IsValid = false
	dbErr := errors.New("grant read: connection refused")
	cases := map[string]func() (oai.Deps, *fakeDeps){
		"no consumer":      func() (oai.Deps, *fakeDeps) { f := &fakeDeps{reg: reg}; return f.deps(), f },
		"invalid consumer": func() (oai.Deps, *fakeDeps) { f := &fakeDeps{ca: invalid, reg: reg}; return f.deps(), f },
		"empty tenant":     func() (oai.Deps, *fakeDeps) { f := &fakeDeps{ca: noTenant, reg: reg}; return f.deps(), f },
		"knowledge context": func() (oai.Deps, *fakeDeps) {
			f := &fakeDeps{ca: appConsumer(), reg: reg, knowledge: true}
			return f.deps(), f
		},
		"grants db error": func() (oai.Deps, *fakeDeps) {
			f := &fakeDeps{ca: appConsumer(), reg: reg, grantErr: dbErr}
			return f.deps(), f
		},
		"no registry": func() (oai.Deps, *fakeDeps) {
			f := &fakeDeps{ca: appConsumer()}
			d := f.deps()
			d.Aliases = nil
			return d, f
		},
		"no grant probe": func() (oai.Deps, *fakeDeps) {
			f := &fakeDeps{ca: appConsumer(), reg: reg}
			d := f.deps()
			d.HasGrant, d.GrantIDs = nil, nil
			return d, f
		},
	}
	for name, mk := range cases {
		for _, path := range []string{"/v1/models", "/v1/models/embed"} {
			sink := captureLog(t)
			d, _ := mk()
			a := get(t, modelsRouter(d), path)
			var e struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(a.body, &e)
			if a.status != http.StatusServiceUnavailable || e.Error.Code != oai.CodeInternalError || e.Error.Type != oai.TypeServiceUnavailable ||
				a.header.Get("x-should-retry") != "false" {
				t.Errorf("%s, %s: %d x-should-retry=%q %s, want 503 internal_error, x-should-retry false", name, path, a.status, a.header.Get("x-should-retry"), a.body)
			}
			if sink.errors() == 0 {
				t.Errorf("%s, %s: no ERROR logged", name, path)
			}
		}
	}
}
