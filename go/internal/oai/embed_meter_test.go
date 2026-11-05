package oai_test

// The per-consumer meter at the embeddings route (W02-09 rebase round onto
// W06-10, part A; the chat route's rules, chat_meter_wire_test.go): a
// reservation goes back in full (Abort) exactly when no byte of the request
// was written to a backend in any try of any block; after wire contact it
// settles with the usage of the blocks that answered or, without one, keeps
// its charge. The usage sums saturate over the blocks. The successful answer
// carries the x-ratelimit-* headers of op embed, a refusal of the meter
// Retry-After and retry-after-ms. Every case runs over the rig's real
// planner, dispatcher and meter (endMeter, frozen clock), against
// deterministic transport faults (wire_sockets_linux_test.go).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// embedReservation is what the meter reserves for embedInputs(n) under op
// embed: the token estimate of its runes ("text 000" = 8 runes each).
func embedReservation(n int) int64 { return int64(promptguard.TokenEstimate(8 * n)) }

// embedMeter is the rig's real meter that names how each reservation ended
// and records the usage of every Settle.
type embedMeter struct {
	*endMeter
	mu      sync.Mutex
	settled []dispatch.Usage
}

func newEmbedMeter(lim keyquota.Limits) *embedMeter {
	return &embedMeter{endMeter: &endMeter{rigMeter: newRigMeter(lim, true)}}
}

func (m *embedMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := m.endMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return embedMeterReservation{LimitReservation: res, m: m}, nil
}

type embedMeterReservation struct {
	oai.LimitReservation
	m *embedMeter
}

func (r embedMeterReservation) Settle(u dispatch.Usage) {
	r.m.mu.Lock()
	r.m.settled = append(r.m.settled, u)
	r.m.mu.Unlock()
	r.LimitReservation.Settle(u)
}

// heldEmbed is what the op-embed bucket of the rig's consumer holds now.
func (m *embedMeter) heldEmbed(t *testing.T, rig *chatRig) int64 {
	t.Helper()
	ca := *rig.ca.Load()
	return m.lim.TPM - m.probe(t, &ca, keyquota.OpEmbed).RemainingTokens
}

// embedWireRow is an embedding row (OpenAI wire) on host.
func embedWireRow(name, host string, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: host, Protocol: backends.ProtocolOpenAI, Model: "EMBED-MODEL",
		Roles: []string{"embed"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"embed": 30},
	}
}

// embedMeterRig mounts the embeddings route over rows (E = 3) and a fresh
// embedMeter.
func embedMeterRig(t *testing.T, rows ...backends.Backend) (*chatRig, *embedMeter) {
	t.Helper()
	m := newEmbedMeter(keyquota.Limits{RPM: 1000, TPM: 60_000_000, CompletionReserve: 1024})
	return newEmbedRig(t, rows, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m })), m
}

// postEmbedCtx posts body on ctx and survives a panic of the route.
func postEmbedCtx(t *testing.T, rig *chatRig, ctx context.Context, body string) chatAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		rig.router.ServeHTTP(w, req)
	}()
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// embedAnswerUsage is an OpenAI embeddings answer to inputs at dims with
// prompt_tokens tokens (math.MinInt writes no usage at all).
func embedAnswerUsage(inputs []string, dims, tokens int) string {
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for k, in := range inputs {
		if k > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"object":"embedding","index":%d,"embedding":`, k)
		writeVec(&b, engineVec(in, dims))
		b.WriteByte('}')
	}
	b.WriteString(`],"model":"X"`)
	if tokens != math.MinInt {
		fmt.Fprintf(&b, `,"usage":{"prompt_tokens":%d,"total_tokens":%d}`, tokens, tokens)
	}
	b.WriteString("}")
	return b.String()
}

// TestEmbedMeterReadThenResetKeeps: the backend reads the whole request of
// block one and dies (RST); DoRetryOnce's retry finds the host refusing the
// connection. The first try wrote the request: the reservation keeps its
// charge — the refused retry does not decide.
func TestEmbedMeterReadThenResetKeeps(t *testing.T) {
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		if b.read(br) {
			b.l.stop()
			reset(c)
		}
	})
	rig, m := embedMeterRig(t, embedWireRow("crash", b.host(), 10))
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	held := m.heldEmbed(t, rig)
	t.Logf("read, reset, retry refused: answer %d; conns %d, requests read %d; ends %s; the bucket holds %d tokens",
		a.status, b.conns.Load(), b.requests.Load(), m.endsOf(), held)
	if b.requests.Load() != 1 || b.conns.Load() != 1 {
		t.Fatalf("backend conns %d, requests %d — want one connection that read the request", b.conns.Load(), b.requests.Load())
	}
	if held != embedReservation(1) || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a backend read the request, want the whole reservation %d kept",
			held, m.endsOf(), embedReservation(1))
	}
}

// TestEmbedMeterConnectRefusedGivesBack: the row's host refuses every
// connect (both tries) — no byte of the request reached a backend: Abort.
func TestEmbedMeterConnectRefusedGivesBack(t *testing.T) {
	rig, m := embedMeterRig(t, embedWireRow("dead", "http://"+refusedAddr(t), 10))
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	code, _, _ := a.envelope(t)
	held := m.heldEmbed(t, rig)
	t.Logf("connect refused: answer %d %s; ends %s; the bucket holds %d tokens", a.status, code, m.endsOf(), held)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable {
		t.Fatalf("answer %d %s, want 503 upstream_unavailable", a.status, code)
	}
	if held != 0 || m.endsOf() != "abort" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a refused connect, want 0 by Abort", held, m.endsOf())
	}
}

// TestEmbedMeterConnectTimeoutGivesBack: the row's host drops every SYN; the
// attempt ends by its deadline inside the connect. No byte left: Abort.
func TestEmbedMeterConnectTimeoutGivesBack(t *testing.T) {
	l := newHeldListener(t)
	l.dropSYNs()
	row := embedWireRow("blackhole", "http://"+l.Addr().String(), 10)
	row.Timeouts = nil
	rig, m := embedMeterRig(t, row)
	shortAttempts(rig)
	start := time.Now()
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	el := time.Since(start)
	held := m.heldEmbed(t, rig)
	t.Logf("connect timeout: answer %d after %v; ends %s; the bucket holds %d tokens", a.status, el.Round(time.Millisecond), m.endsOf(), held)
	if el < 200*time.Millisecond {
		t.Fatalf("answered after %v — the connect did not hang until the attempt deadline", el)
	}
	if held != 0 || m.endsOf() != "abort" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a connect that timed out, want 0 by Abort", held, m.endsOf())
	}
}

// TestEmbedMeterWireContactNeverRefundsInFull (B-1, B-2; Fremd-Review 2
// probe): after wire contact without a usage — an upstream 400, an answer
// without usage, a panic after the wire (close()), the client gone during
// the wire call (the context rule) — the reservation stays booked.
func TestEmbedMeterWireContactNeverRefundsInFull(t *testing.T) {
	cases := []struct {
		name    string
		respond func(w http.ResponseWriter, r *http.Request, n int64, inputs []string) bool
		panicAt int64
		cancel  bool
		want    string
	}{
		{name: "upstream 400", want: "keep", respond: func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
			return true
		}},
		{name: "answer without usage", want: "keep", respond: func(w http.ResponseWriter, _ *http.Request, _ int64, inputs []string) bool {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, embedAnswerUsage(inputs, 8, math.MinInt))
			return true
		}},
		{name: "panic after the wire", panicAt: 2, want: "keep"},
		{name: "client gone during the wire call", cancel: true, want: "keep"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			m := newEmbedMeter(keyquota.Limits{RPM: 100, TPM: 60_000_000, CompletionReserve: 1024})
			var grants atomic.Int64
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
				d.Limiter = m
				inner := d.HasGrant
				d.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
					if grants.Add(1) == tc.panicAt {
						panic("embed meter gate: panic after the wire")
					}
					return inner(ctx, consumerID, aliasID)
				}
			}))
			rig.setGrant(idEmbOne, true)
			ca := *rig.ca.Load()
			ca.AllModels = false
			rig.ca.Store(&ca)
			switch {
			case tc.respond != nil:
				e.setRespond(tc.respond)
			case tc.cancel:
				e.setRespond(func(_ http.ResponseWriter, r *http.Request, _ int64, _ []string) bool {
					cancel()
					<-r.Context().Done()
					return true
				})
			}
			a := postEmbedCtx(t, rig, ctx, embedBody("emb1", embedInputs(1)))
			if e.hits.Load() != 1 {
				t.Fatalf("engine hits %d, want 1 (the case needs wire contact)", e.hits.Load())
			}
			held := m.heldEmbed(t, rig)
			t.Logf("%s: answer %d; ends %s; the bucket holds %d tokens, reservation %d", tc.name, a.status, m.endsOf(), held, embedReservation(1))
			if held != embedReservation(1) || m.endsOf() != tc.want {
				t.Errorf("%s: answer %d; the bucket holds %d tokens (ends %s), want the whole reservation %d by %s (no full refund after wire contact)",
					tc.name, a.status, held, m.endsOf(), embedReservation(1), tc.want)
			}
		})
	}
}

// TestEmbedMeterFailedBlockSettlesTheServedBlocks pins the end of a request
// whose block 3 of 8 fails on the wire after two blocks answered (two
// prompt tokens each): the reservation settles with the usage of the served
// blocks (4 tokens) — the backend reported them; blocks 4…8 never reached a
// backend, and the sticky stop ends the request at the failed block, so the
// one block whose usage is unknown is the failed one (named in the report).
func TestEmbedMeterFailedBlockSettlesTheServedBlocks(t *testing.T) {
	e := newEmbedEngine(t, 8)
	e.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, _ []string) bool {
		if n == 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
			return true
		}
		return false
	})
	rig, m := embedMeterRig(t, e.row("spark-embed", backends.ProtocolOpenAI, 10))
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(8)))
	held := m.heldEmbed(t, rig)
	t.Logf("8 blocks, block 3 failed: answer %d %.160s; engine hits %d; ends %s; settled %+v; the bucket holds %d tokens (reservation %d)",
		a.status, a.body, e.hits.Load(), m.endsOf(), m.settled, held, embedReservation(8))
	if e.hits.Load() != 3 {
		t.Fatalf("engine hits %d, want 3 (two served blocks, the failed third, nothing after the sticky stop)", e.hits.Load())
	}
	if held != 4 || m.endsOf() != "settle" || len(m.settled) != 1 || m.settled[0].PromptTokens != 4 {
		t.Errorf("the bucket holds %d tokens (ends %s, settled %+v), want the served blocks' usage 4 settled", held, m.endsOf(), m.settled)
	}
}

// TestEmbedMeterUsageSumSaturates (N-2): the usage of the blocks is summed
// as the meter counts it — a negative counter counts 0, the sum saturates at
// the int range. Two blocks of 2⁶² prompt tokens each overflow an int sum
// into a negative one, which the meter would read as 0: a full refund.
func TestEmbedMeterUsageSumSaturates(t *testing.T) {
	cases := []struct {
		name   string
		tokens [2]int
		want   int
	}{
		{"two blocks of 2^62", [2]int{1 << 62, 1 << 62}, math.MaxInt},
		{"a negative block", [2]int{-5, 2}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, inputs []string) bool {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, embedAnswerUsage(inputs, 8, tc.tokens[n-1]))
				return true
			})
			rig, m := embedMeterRig(t, e.row("spark-embed", backends.ProtocolOpenAI, 10))
			a := rig.postEmbed(t, embedBody("emb1", embedInputs(2)))
			if a.status != http.StatusOK {
				t.Fatalf("answer %d %.300s", a.status, a.body)
			}
			rec := <-rig.recs
			rows := rig.rows.all()
			t.Logf("%s: ends %s; settled %+v; access record prompt %d usage %q; rows %d", tc.name, m.endsOf(), m.settled, rec.PromptTokens, rec.Usage, len(rows))
			if m.endsOf() != "settle" || len(m.settled) != 1 || m.settled[0].PromptTokens != tc.want {
				t.Errorf("%s: ends %s, settled %+v — want one Settle of %d prompt tokens", tc.name, m.endsOf(), m.settled, tc.want)
			}
			if rec.PromptTokens != tc.want || rec.Usage != llmlog.UsageBackend {
				t.Errorf("%s: access record prompt %d usage %q, want %d and %q", tc.name, rec.PromptTokens, rec.Usage, tc.want, llmlog.UsageBackend)
			}
			if len(rows) != 1 || rows[0].Telemetry.PromptTokens != tc.want || rows[0].Gateway.Usage != llmlog.UsageBackend {
				t.Errorf("%s: rows %d (want 1 with prompt %d and usage %q)", tc.name, len(rows), tc.want, llmlog.UsageBackend)
			}
		})
	}
}

// TestEmbedMeterFailoverRefusalAfterTheWireKeeps (admissionRefused with
// wire contact): the first row answers 503 after the wire, the failover
// row's only external slot is held, the dispatcher refuses the failover
// acquire — the route answers through the admission writer and the
// reservation stays booked: the first row had the request.
func TestEmbedMeterFailoverRefusalAfterTheWireKeeps(t *testing.T) {
	first := newEmbedEngine(t, 8)  // prio 20: tried first
	second := newEmbedEngine(t, 8) // prio 10: the failover row
	first.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"engine busy"}}`)
		return true
	})
	m := newEmbedMeter(keyquota.Limits{RPM: 100, TPM: 60_000_000, CompletionReserve: 1024})
	rig := newEmbedRig(t, []backends.Backend{second.row("spark1", backends.ProtocolOpenAI, 10), first.row("spark2", backends.ProtocolOpenAI, 20)}, 2, 1,
		withDeps(func(d *oai.Deps) { d.Limiter = m }))
	hctx := context.WithValue(context.Background(), consumerKey{}, rig.ca.Load())
	hold, _, err := rig.disp.Acquire(hctx, dispatch.Request{Target: dispatch.Target{Origin: second.srv.URL}, Class: dispatch.ClassExternal, Role: "embed"})
	if err != nil {
		t.Fatalf("holding the failover row's external slot: %v", err)
	}
	defer hold.Release()
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	code, _, _ := a.envelope(t)
	held := m.heldEmbed(t, rig)
	t.Logf("first row 503 after the wire, failover row full: answer %d %s; hits first %d, failover %d; ends %s; the bucket holds %d tokens",
		a.status, code, first.hits.Load(), second.hits.Load(), m.endsOf(), held)
	if a.status != http.StatusTooManyRequests || code != "server_busy" || first.hits.Load() != 1 || second.hits.Load() != 0 {
		t.Fatalf("answer %d %s, hits %d/%d — want 429 server_busy after one wire call on the first row", a.status, code, first.hits.Load(), second.hits.Load())
	}
	if held != embedReservation(1) || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a failover refusal behind a wire call, want the whole reservation %d kept",
			held, m.endsOf(), embedReservation(1))
	}
}

// TestEmbedMeterSuccessCarriesRateLimitHeaders (B-3, lead decision M2 (c)):
// the successful answer carries the x-ratelimit-* headers of op embed as
// the admission left them.
func TestEmbedMeterSuccessCarriesRateLimitHeaders(t *testing.T) {
	e := newEmbedEngine(t, 8)
	m := newEmbedMeter(keyquota.Limits{RPM: 10, TPM: 60000, CompletionReserve: 1024})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m }))
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	if a.status != http.StatusOK {
		t.Fatalf("answer %d %.300s", a.status, a.body)
	}
	want := map[string]string{
		"x-ratelimit-limit-requests": "10", "x-ratelimit-remaining-requests": "9",
		"x-ratelimit-limit-tokens": "60000", "x-ratelimit-remaining-tokens": fmt.Sprint(60000 - embedReservation(1)),
	}
	for k, v := range want {
		if got := a.header.Get(k); got != v {
			t.Errorf("embeddings 200: %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if a.header.Get(k) == "" {
			t.Errorf("embeddings 200: %s missing", k)
		}
	}
}

// TestEmbedMeterRefusalCarriesRetryAfterMs (B-4): the meter's refusal on
// the embeddings route carries Retry-After AND retry-after-ms, exact.
func TestEmbedMeterRefusalCarriesRetryAfterMs(t *testing.T) {
	e := newEmbedEngine(t, 8)
	m := newEmbedMeter(keyquota.Limits{RPM: 1, TPM: 60000, CompletionReserve: 1024})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m }))
	if a := rig.postEmbed(t, embedBody("emb1", embedInputs(1))); a.status != http.StatusOK {
		t.Fatalf("first: %d %.300s", a.status, a.body)
	}
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	code, _, _ := a.envelope(t)
	t.Logf("second: %d %s Retry-After %q retry-after-ms %q", a.status, code, a.header.Get("Retry-After"), a.header.Get("retry-after-ms"))
	if a.status != http.StatusTooManyRequests || code != oai.CodeRateLimitExceeded {
		t.Fatalf("second: %d %s, want 429 rate_limit_exceeded", a.status, a.body)
	}
	if a.header.Get("Retry-After") != "60" || a.header.Get("retry-after-ms") != "60000" {
		t.Errorf("second: Retry-After %q retry-after-ms %q, want 60 / 60000", a.header.Get("Retry-After"), a.header.Get("retry-after-ms"))
	}
	for _, k := range []string{"x-ratelimit-limit-requests", "x-ratelimit-remaining-tokens"} {
		if a.header.Get(k) != "" {
			t.Errorf("the 429 carries %s %q — the x-ratelimit-* headers belong to the successful answer only", k, a.header.Get(k))
		}
	}
}
