// The in-process mesh of package fedtest (see doc.go).

package fedtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

// Options configure a mesh. Zero values take the defaults.
type Options struct {
	// MaxDepth is federation.max_depth of every instance (default 3, E-04-7).
	MaxDepth int
	// AttemptTimeout is the attempt timeout T of every walk (default 5 s).
	AttemptTimeout time.Duration
}

const (
	defaultMaxDepth       = 3
	defaultAttemptTimeout = 5 * time.Second
	// role is the one role the mesh walks.
	role = "chat"
	// errorBodyLimit bounds what an attempt reads of an error answer — the
	// 1 KiB of the production wire sites.
	errorBodyLimit = 1 << 10
)

// requestBody is the chat request every wire call of the mesh carries.
var requestBody = []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)

// Mesh is a set of instances and leaves that call each other in-process.
type Mesh struct {
	opt       Options
	transport *http.Transport
	client    *http.Client

	mu        sync.Mutex
	instances []*Instance
	leaves    []*Leaf
	captures  []Capture
	closed    sync.Once
}

// Capture is one request an instance or a leaf received.
type Capture struct {
	At     time.Time
	Target string // the instance or leaf name
	Path   string
	Header http.Header
}

// New builds an empty mesh; t's cleanup closes it.
func New(t testing.TB, opt Options) *Mesh {
	t.Helper()
	if opt.MaxDepth <= 0 {
		opt.MaxDepth = defaultMaxDepth
	}
	if opt.AttemptTimeout <= 0 {
		opt.AttemptTimeout = defaultAttemptTimeout
	}
	tr := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second}
	m := &Mesh{opt: opt, transport: tr, client: &http.Client{Transport: tr}}
	t.Cleanup(m.Close)
	return m
}

// Close closes every server, dispatcher and idle connection of the mesh;
// later calls do nothing.
func (m *Mesh) Close() {
	m.closed.Do(func() {
		m.transport.CloseIdleConnections()
		m.mu.Lock()
		insts, leaves := m.instances, m.leaves
		m.mu.Unlock()
		for _, i := range insts {
			i.srv.Close()
			i.disp.Close()
		}
		for _, l := range leaves {
			l.srv.Close()
		}
		m.transport.CloseIdleConnections()
	})
}

func (m *Mesh) capture(target string, r *http.Request) {
	m.mu.Lock()
	m.captures = append(m.captures, Capture{At: time.Now(), Target: target, Path: r.URL.Path, Header: r.Header.Clone()})
	m.mu.Unlock()
}

// Captures returns every request captured since the mesh was built or last reset.
func (m *Mesh) Captures() []Capture {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Capture(nil), m.captures...)
}

// Calls is the number of wire calls in the mesh: every request an instance
// or a leaf received.
func (m *Mesh) Calls() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, i := range m.instances {
		n += i.hits.Load()
	}
	for _, l := range m.leaves {
		n += l.hits.Load()
	}
	return n
}

// ResetCounts zeroes every counter and drops the captures.
func (m *Mesh) ResetCounts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range m.instances {
		i.hits.Store(0)
	}
	for _, l := range m.leaves {
		l.hits.Store(0)
	}
	m.captures = nil
}

// Leaf is a fake upstream: an engine or a capture endpoint.
type Leaf struct {
	Name, URL string
	srv       *httptest.Server
	hits      atomic.Int64
	status    atomic.Int32
	delay     atomic.Int64
}

// Leaf adds a leaf answering status (200: a chat completion; 429: the
// OpenAI rate-limit envelope; anything else: that status).
func (m *Mesh) Leaf(name string, status int) *Leaf {
	l := &Leaf{Name: name}
	l.status.Store(int32(status)) //nolint:gosec // G115: an HTTP status, three digits
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.hits.Add(1)
		m.capture(name, r)
		if d := time.Duration(l.delay.Load()); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		writeStatus(w, int(l.status.Load()))
	}))
	l.URL = l.srv.URL
	m.mu.Lock()
	m.leaves = append(m.leaves, l)
	m.mu.Unlock()
	return l
}

// SetStatus changes the leaf's answer.
func (l *Leaf) SetStatus(code int) { l.status.Store(int32(code)) } //nolint:gosec // G115: an HTTP status

// SetDelay makes the leaf answer after d (the generation time of an engine).
func (l *Leaf) SetDelay(d time.Duration) { l.delay.Store(int64(d)) }

// Hits is the number of requests the leaf received.
func (l *Leaf) Hits() int64 { return l.hits.Load() }

func writeStatus(w http.ResponseWriter, code int) {
	switch code {
	case http.StatusOK:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	case http.StatusTooManyRequests:
		oai.Write(w, oai.Error{Status: code, Type: oai.TypeRateLimit, Code: oai.CodeRateLimitExceeded, Message: "Rate limit reached."})
	default:
		w.WriteHeader(code)
	}
}

// Instance is one ctx instance of the mesh.
type Instance struct {
	Name     string
	Identity fedhop.Identity
	URL      string
	// Pool holds the instance's learned peer instances (NotePeerInstance,
	// LearnedInstances); its snapshot stays empty — the chain is the
	// instance's own row list.
	Pool *backends.Pool

	m         *Mesh
	srv       *httptest.Server
	disp      *dispatch.Dispatcher
	hits      atomic.Int64
	queueWait atomic.Int64

	mu        sync.RWMutex
	rows      []backends.Backend
	transit   map[string]bool
	ctxRows   []CtxRow
	engine    *Leaf
	consumers map[string]*auth.ConsumerAuth
}

// CtxRow is a ctx row of an instance and the instance it leads to.
type CtxRow struct {
	ID string
	To *Instance
}

type consumerKey struct{}

func consumerFrom(ctx context.Context) *auth.ConsumerAuth {
	ca, _ := ctx.Value(consumerKey{}).(*auth.ConsumerAuth)
	return ca
}

// Instance adds an instance with a fresh identity and its server:
// capture → identity → door → LoopGuard → PeerIngress → handler.
func (m *Mesh) Instance(name string) *Instance {
	m.mu.Lock()
	n := len(m.instances) + 1
	m.mu.Unlock()
	id := "00000000-0000-4000-8000-" + leftPad(strconv.Itoa(n), 12)
	i := &Instance{
		Name:      name,
		Identity:  fedhop.NewIdentity(id, bytes.Repeat([]byte{byte(n)}, 32)),
		Pool:      backends.NewPool(nil, nil),
		m:         m,
		disp:      dispatch.New(nil, dispatch.DefaultSettings()),
		transit:   map[string]bool{},
		consumers: map[string]*auth.ConsumerAuth{},
	}
	guard := handler.LoopGuard(handler.LoopGuardConfig{MaxDepth: func() int { return m.opt.MaxDepth }, Envelope: handler.LoopGuardOpenAI})
	peer := federation.PeerIngress(federation.PeerIngressConfig{Consumer: consumerFrom})
	stack := fedhop.IdentityMiddleware(i.Identity)(i.door(guard(peer(http.HandlerFunc(i.serve)))))
	i.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.hits.Add(1)
		m.capture(name, r)
		stack.ServeHTTP(w, r)
	}))
	i.URL = i.srv.URL
	m.mu.Lock()
	m.instances = append(m.instances, i)
	m.mu.Unlock()
	return i
}

func leftPad(s string, n int) string { return strings.Repeat("0", max(n-len(s), 0)) + s }

// Hits is the number of requests the instance received.
func (i *Instance) Hits() int64 { return i.hits.Load() }

// EngineHits is the number of requests the instance's engine leaf received
// (0 without an engine).
func (i *Instance) EngineHits() int64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.engine == nil {
		return 0
	}
	return i.engine.Hits()
}

// CtxRows lists the instance's ctx rows in chain order.
func (i *Instance) CtxRows() []CtxRow {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]CtxRow(nil), i.ctxRows...)
}

// Consumer registers a model consumer token at the instance's door.
func (i *Instance) Consumer(token string, kind auth.ConsumerKind, sensitivity string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.consumers[token] = &auth.ConsumerAuth{ConsumerID: i.Name + "/" + token, Kind: kind, PromptSensitivity: sensitivity, IsValid: true}
}

// SetQueueWait makes the instance's first acquire of a request wait d (its
// admission queue): within the request's wait budget (Inbound.Wait) the
// lease follows, beyond it the request is refused as busy after the budget.
func (i *Instance) SetQueueWait(d time.Duration) { i.queueWait.Store(int64(d)) }

// Learn records that the row answered as instance (Pool.NotePeerInstance).
func (i *Instance) Learn(rowID, instance string) { i.Pool.NotePeerInstance(rowID, instance) }

func (i *Instance) addRow(b backends.Backend, transit bool) {
	b.Name = b.ID
	b.Protocol = backends.ProtocolOpenAI
	b.Roles = []string{role}
	b.ModelMap = map[string]backends.ModelSpec{role: {Model: "m"}}
	b.Trust = backends.TrustFull
	b.Enabled = true
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rows = append(i.rows, b)
	i.transit[b.ID] = transit
}

// CtxRow appends a ctx row (provider class ctx, LAN) to the instance to, and
// registers this instance as a ctx-peer consumer (floor credentials) at to's
// door. transit false is the default of a ctx row: the stamp says
// Ctx-Egress: local.
func (i *Instance) CtxRow(to *Instance, transit bool) CtxRow {
	token := "peer:" + i.Name
	to.Consumer(token, auth.KindCtxPeer, string(backends.SensCredentials))
	r := CtxRow{ID: "ctx:" + i.Name + "->" + to.Name, To: to}
	i.addRow(backends.Backend{ID: r.ID, Host: to.URL, APIKey: token, ProviderClass: backends.ProviderCtx, Locality: backends.LocalityLAN}, transit)
	i.mu.Lock()
	i.ctxRows = append(i.ctxRows, r)
	i.mu.Unlock()
	return r
}

// Engine appends the instance's own engine (llamacpp, local) on leaf l.
func (i *Instance) Engine(l *Leaf) {
	i.addRow(backends.Backend{ID: "engine:" + i.Name + ":" + l.Name, Host: l.URL, ProviderClass: backends.ProviderLlamaCpp, Locality: backends.LocalityLocal}, false)
	i.mu.Lock()
	i.engine = l
	i.mu.Unlock()
}

// Row appends a row of any provider class and locality on leaf l.
func (i *Instance) Row(id, providerClass, locality string, l *Leaf) {
	i.addRow(backends.Backend{ID: id, Host: l.URL, ProviderClass: providerClass, Locality: locality}, false)
}

func (i *Instance) chain() []backends.Backend {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]backends.Backend(nil), i.rows...)
}

func (i *Instance) door(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		i.mu.RLock()
		ca, ok := i.consumers[token]
		i.mu.RUnlock()
		if !ok {
			oai.WriteInvalidAPIKey(w)
			return
		}
		c := *ca
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), consumerKey{}, &c)))
	})
}

// serve is the route handler of A: the filtered chain, one walk, the answer.
func (i *Instance) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pol := federation.PolicyFrom(ctx)
	chain, err := federation.FilterChain(i.chain(), fedhop.InboundFrom(ctx), pol, i.Pool.LearnedInstances())
	if err != nil {
		oai.Write(w, oai.Error{Status: http.StatusServiceUnavailable, Type: oai.TypeServiceUnavailable, Code: oai.CodeNoEligibleBackend, Message: "No backend can serve the request."})
		return
	}
	served, _, err := i.walk(ctx, chain, pol.Required, false)
	var se *httpx.StatusError
	switch {
	case served != nil:
		writeStatus(w, http.StatusOK)
	case llm.IsAdmissionError(err):
		oai.Write(w, oai.Error{Status: http.StatusTooManyRequests, Type: oai.TypeRateLimit, Code: oai.CodeServerBusy, Message: "The server is busy."})
	case errors.As(err, &se) && se.Code == http.StatusTooManyRequests:
		writeStatus(w, http.StatusTooManyRequests)
	default:
		oai.Write(w, oai.Error{Status: http.StatusServiceUnavailable, Type: oai.TypeServiceUnavailable, Code: oai.CodeUpstreamUnavailable, Message: "No upstream answered."})
	}
}

func (i *Instance) walk(ctx context.Context, chain []backends.Backend, required backends.Sensitivity, forceLocal bool) (*backends.Backend, []llm.ChainAttempt, error) {
	rules := llm.WalkRules{
		AllowedClasses: []dispatch.Class{dispatch.ClassBackground},
		Timeout:        func(*backends.Backend) time.Duration { return i.m.opt.AttemptTimeout },
	}
	return llm.WalkChain(ctx, chain, role, acquirer{i}, nil, rules, i.attempt(required, forceLocal))
}

// attempt is one wire call to row b: Tier F by hand for a ctx row (with the
// wire timeout of fedhop.WireTimeout), the row's own HopTarget otherwise.
func (i *Instance) attempt(required backends.Sensitivity, forceLocal bool) llm.AttemptFunc {
	return func(runCtx context.Context, b *backends.Backend, _ *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
		i.mu.RLock()
		transit := i.transit[b.ID]
		i.mu.RUnlock()
		h := http.Header{}
		wire := timeout
		if b.ProviderClass == backends.ProviderCtx {
			t := fedhop.Target{BackendID: b.ID, Tier: fedhop.TierFederation, Required: string(required), EgressLocal: forceLocal || !transit}
			fedhop.Stamp(runCtx, h, t)
			wire = fedhop.WireTimeout(runCtx, t, timeout)
		} else {
			fedhop.Stamp(runCtx, h, b.HopTarget(fedhop.WireChat))
		}
		cctx, cancel := context.WithTimeout(runCtx, wire)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, b.Host+"/v1/chat/completions", bytes.NewReader(requestBody))
		if err != nil {
			return dispatch.Usage{}, false, err
		}
		for k, v := range h {
			req.Header[k] = v
		}
		req.Header.Set("Content-Type", "application/json")
		if b.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+b.APIKey)
		}
		resp, err := i.m.client.Do(req)
		if err != nil {
			return dispatch.Usage{}, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		if resp.StatusCode != http.StatusOK {
			return dispatch.Usage{}, false, &httpx.StatusError{Code: resp.StatusCode, Body: string(body)}
		}
		return dispatch.Usage{}, true, nil
	}
}

// acquirer is the walk's Acquirer over the instance's pass-through
// dispatcher; First waits the instance's queue time first.
type acquirer struct{ i *Instance }

func (a acquirer) Class() dispatch.Class { return dispatch.ClassBackground }

func (a acquirer) request(b *backends.Backend, timeout time.Duration) dispatch.Request {
	return dispatch.Request{Target: dispatch.Target{Origin: b.Host}, Class: dispatch.ClassBackground, Role: role, DeadlineIn: timeout}
}

// errOverBudget is the admission refusal of a queue wait beyond the budget.
var errOverBudget = errors.New("fedtest: queue wait beyond the request's wait budget")

func (a acquirer) First(ctx context.Context, b *backends.Backend, _ string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	if qw := time.Duration(a.i.queueWait.Load()); qw > 0 {
		wait, refuse := qw, false
		if budget, ok := fedhop.InboundFrom(ctx).Wait(); ok && budget < qw {
			wait, refuse = budget, true
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, nil, ctx.Err()
		}
		if refuse {
			return nil, nil, errOverBudget
		}
	}
	return a.i.disp.Acquire(ctx, a.request(b, timeout))
}

func (a acquirer) Next(ctx context.Context, b *backends.Backend, _ string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	return a.i.disp.TryAcquire(ctx, a.request(b, timeout))
}

func (a acquirer) Continue(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	return nil, nil, llm.ErrNoContinuation
}

// Origin configures a request the mesh starts at an instance's own walk —
// B's internal pipeline, not B's ingress.
type Origin struct {
	// EgressLocal stamps Ctx-Egress: local on every ctx row of the walk
	// (otherwise the row's transit flag decides).
	EgressLocal bool
	// MaxWait > 0 sends Ctx-Max-Wait-Ms (the budget is carried as an
	// inbound one, so fedhop.WireTimeout extends B's wire as well).
	MaxWait time.Duration
	// Required is the chain's sensitivity (default credentials).
	Required backends.Sensitivity
}

// Result is the outcome of an originated request.
type Result struct {
	Served   string // the serving row's id, "" when none served
	Attempts []llm.ChainAttempt
	Err      error
	Elapsed  time.Duration
}

// Originate walks from's whole chain with from's identity, the way B's own
// pipelines do: no ingress and no chain filter on from itself.
func (m *Mesh) Originate(ctx context.Context, from *Instance, o Origin) Result {
	ctx = fedhop.WithIdentity(ctx, from.Identity)
	if o.MaxWait > 0 {
		ctx = fedhop.WithInbound(ctx, fedhop.Inbound{MaxWait: o.MaxWait, HasMaxWait: true, ReceivedAt: time.Now()})
	}
	required := o.Required
	if required == "" {
		required = backends.SensCredentials
	}
	start := time.Now()
	served, attempts, err := from.walk(ctx, from.chain(), required, o.EgressLocal)
	res := Result{Attempts: attempts, Err: err, Elapsed: time.Since(start)}
	if served != nil {
		res.Served = served.ID
	}
	return res
}

// Send is a client request straight to an instance's /v1/chat/completions
// with a consumer token and federation headers (canonical keys through
// http.Header.Set); it returns the status.
func (m *Mesh) Send(to *Instance, token string, hdr map[string]string) int {
	req, err := http.NewRequest(http.MethodPost, to.URL+"/v1/chat/completions", bytes.NewReader(requestBody)) //nolint:noctx // test helper; the server is in-process
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}
