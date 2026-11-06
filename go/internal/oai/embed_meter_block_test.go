package oai_test

// The meter's rule per block at the embeddings route (Delta-Review 4 F4-1):
// a block of which a byte reached a backend and that reported no usage — the
// client gone during it, an upstream error, an answer without usage — is
// booked with the meter's estimate of its inputs, whatever the cause; a
// block that reached no backend books nothing. Over the real route, the real
// dispatcher and the real meter, against real sockets.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// postEmbedOverSocket posts body to the rig's route over a real socket (an
// httptest.Server in front of the router): what a client sees on the wire.
// Canceling ctx closes the client's connection — the route sees the client
// gone.
func postEmbedOverSocket(t *testing.T, rig *chatRig, ctx context.Context, body string) (*http.Response, string, error) {
	t.Helper()
	srv := httptest.NewServer(rig.router)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

// waitEnded waits until the meter recorded an end: after a gone client the
// route ends the reservation asynchronously to the client's return.
func (m *embedMeter) waitEnded(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); m.endsOf() == ""; {
		if time.Now().After(deadline) {
			t.Fatalf("the reservation did not end within 10 s")
		}
		time.Sleep(time.Millisecond)
	}
}

// settledNow is the usage of every Settle so far.
func (m *embedMeter) settledNow() []dispatch.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]dispatch.Usage(nil), m.settled...)
}

// blockCharge is what the bucket holds after a request of eight one-input
// blocks (emb1, two prompt tokens each) whose block k met a backend without
// a usage: block one ⇒ the whole reservation kept (no block reported a
// usage); a later block ⇒ the usage of the k−1 served blocks plus the
// meter's estimate of block k's one input, never above the reservation.
func blockCharge(k int) (string, int64) {
	if k == 1 {
		return "keep", embedReservation(8)
	}
	return "settle", min(int64(2*(k-1))+embedReservation(1), embedReservation(8))
}

// TestEmbedMeterBlockContactBooksItsEstimate (F4-1): emb1 (sub_batch 1),
// eight inputs ⇒ eight blocks; block k meets the backend — the engine reads
// the whole request — and either answers 502 or the client closes its
// connection while the engine holds the block. The reservation (36) ends
// once: block 1 ⇒ keep 36; block 3 ⇒ settle 4 + 5 = 9; block 8 ⇒ settle
// 14 + 5 = 19. Before the rule the touched block went back in full (settle
// 4 / 14: a client paid for k−1 of the k blocks a backend had).
func TestEmbedMeterBlockContactBooksItsEstimate(t *testing.T) {
	cases := []struct {
		name  string
		block int64
		gone  bool
	}{
		{"upstream 502 in block 1 of 8", 1, false},
		{"upstream 502 in block 3 of 8", 3, false},
		{"upstream 502 in block 8 of 8", 8, false},
		{"client gone during block 1 of 8", 1, true},
		{"client gone during block 3 of 8", 3, true},
		{"client gone during block 8 of 8", 8, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var touched atomic.Bool
			e.setRespond(func(w http.ResponseWriter, r *http.Request, n int64, _ []string) bool {
				if n != tc.block {
					return false
				}
				touched.Store(true)
				if tc.gone {
					cancel()
					<-r.Context().Done()
					return true
				}
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
				return true
			})
			rig, m := embedMeterRig(t, e.row("spark-embed", backends.ProtocolOpenAI, 10))
			resp, body, err := postEmbedOverSocket(t, rig, ctx, embedBody("emb1", embedInputs(8)))
			m.waitEnded(t)
			held := m.heldEmbed(t, rig)
			ends, settled := m.endsOf(), m.settledNow()
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			wantEnd, want := blockCharge(int(tc.block))
			t.Logf("%s: answer %d err=%v %.80s; engine hits %d (block %d reached: %v); ends %s; settled %+v; held %d (want %s %d; reservation %d, per block %d)",
				tc.name, status, err, body, e.hits.Load(), tc.block, touched.Load(), ends, settled, held, wantEnd, want, embedReservation(8), embedReservation(1))
			if !touched.Load() || e.hits.Load() != tc.block {
				t.Fatalf("engine hits %d, block %d reached %v — want the engine to hold block %d and nothing after it", e.hits.Load(), tc.block, touched.Load(), tc.block)
			}
			if ends != wantEnd || held != want {
				t.Errorf("ends %q, the bucket holds %d — want one %s of %d", ends, held, wantEnd, want)
			}
			if wantEnd == "settle" && (len(settled) != 1 || int64(settled[0].PromptTokens) != want) {
				t.Errorf("settled %+v, want one Settle of %d prompt tokens", settled, want)
			}
		})
	}
}

// readEmbedInputs reads one OpenAI embeddings request from br and returns
// its inputs.
func readEmbedInputs(br *bufio.Reader) ([]string, bool) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, false
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, false
	}
	var in struct {
		Input []string `json:"input"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Input) == 0 {
		return nil, false
	}
	return in.Input, true
}

// rawEmbedAnswer answers inputs with two prompt tokens per input and
// Connection: close (one connection per block).
func rawEmbedAnswer(c net.Conn, inputs []string) {
	ans := embedAnswerUsage(inputs, 8, 2*len(inputs))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(ans), ans)
}

// TestEmbedMeterBlockTransportFault (F4-1): block 3 of 8 meets a transport
// fault after two served blocks (raw backend, one connection per block).
// The connect refused ⇒ block 3 reached no backend: settle 4, the served
// blocks' usage exactly. The backend read block 3 and died (RST), the retry's
// connect refused ⇒ block 3 had wire contact: settle 4 + 5 = 9. The blocks
// are told apart by their input, not by the order of the connections, and
// the listener stops before block 2's answer leaves — the route dials block 3
// only after it read that answer (under -race the stop after the answer lost
// the race 9 times in 200).
func TestEmbedMeterBlockTransportFault(t *testing.T) {
	cases := []struct {
		mode    string
		contact bool
	}{
		{"connect refused", false},
		{"read then reset, retry refused", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			var reqs, served atomic.Int64
			inputs := embedInputs(8)
			b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
				in, ok := readEmbedInputs(br)
				if !ok {
					return
				}
				reqs.Add(1)
				switch {
				case tc.contact && in[0] == inputs[2]:
					b.requests.Add(1)
					b.l.stop()
					reset(c)
					return
				case !tc.contact && in[0] == inputs[1]:
					b.l.stop()
				}
				served.Add(1)
				rawEmbedAnswer(c, in)
			})
			rig, m := embedMeterRig(t, embedWireRow("raw", b.host(), 10))
			a := rig.postEmbed(t, embedBody("emb1", embedInputs(8)))
			held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
			want := int64(4)
			if tc.contact {
				want += embedReservation(1)
			}
			t.Logf("block 3 %s: answer %d %.80s; conns %d, requests %d, served %d, block 3 read %d; ends %s; settled %+v; held %d (want settle %d)",
				tc.mode, a.status, a.body, b.conns.Load(), reqs.Load(), served.Load(), b.requests.Load(), ends, settled, held, want)
			if a.status == http.StatusOK || served.Load() != 2 {
				t.Fatalf("answer %d, served %d — want block 3 to fail after two served blocks", a.status, served.Load())
			}
			if tc.contact && b.requests.Load() != 1 {
				t.Fatalf("the backend read %d requests of block 3, want 1", b.requests.Load())
			}
			if ends != "settle" || held != want || len(settled) != 1 || int64(settled[0].PromptTokens) != want {
				t.Errorf("ends %q, settled %+v, the bucket holds %d — want one Settle of %d", ends, settled, held, want)
			}
		})
	}
}

// TestEmbedMeterBlockWithoutUsageBooksItsEstimate (F4-1, the third cause):
// emb1, two blocks; block 1 answers with two prompt tokens, block 2 answers
// its vectors without a usage object ⇒ 200, settle 2 + 5 = 7 (before: 2 —
// the block the backend computed went back in full). No block with a usage
// ⇒ keep the whole reservation, as before.
func TestEmbedMeterBlockWithoutUsageBooksItsEstimate(t *testing.T) {
	cases := []struct {
		name   string
		tokens [2]int
		end    string
		want   int64
	}{
		{"block 2 without usage", [2]int{2, math.MinInt}, "settle", 2 + embedReservation(1)},
		{"no block with usage", [2]int{math.MinInt, math.MinInt}, "keep", embedReservation(2)},
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
			held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
			t.Logf("%s: answer %d; hits %d; ends %s; settled %+v; held %d (want %s %d)", tc.name, a.status, e.hits.Load(), ends, settled, held, tc.end, tc.want)
			if a.status != http.StatusOK || e.hits.Load() != 2 {
				t.Fatalf("answer %d %.200s, hits %d — want 200 after two blocks", a.status, a.body, e.hits.Load())
			}
			if ends != tc.end || held != tc.want {
				t.Errorf("ends %q, the bucket holds %d — want %s %d", ends, held, tc.end, tc.want)
			}
			if tc.end == "settle" && (len(settled) != 1 || int64(settled[0].PromptTokens) != tc.want) {
				t.Errorf("settled %+v, want one Settle of %d", settled, tc.want)
			}
		})
	}
}

// failoverCase is one case of TestEmbedMeterFailoverBooksEachInputOnce: the
// first row (prio 20) is an engine that refuses every block it reads; the
// second (prio 10) is built by second (it reports two prompt tokens per
// input); firstE, when set, is the first row's external capacity E (the
// rig's is 3).
type failoverCase struct {
	name     string
	model    string
	inputs   []string
	tpm      int64
	refusal  int // the first row's answer: 503 (gateway) or 400 (the window, num_ctx 64 against 4096)
	firstE   int
	second   func(t *testing.T, cancel func(), inputs []string) (backends.Backend, func() int64)
	status   int // 0: any failure
	firstHit int64
	hits     int64 // the second row's
	end      string
	want     int64
}

// servingEngine is a second row that serves every block; failAt > 0 makes
// its block failAt answer 502 (gone: the client goes away while the engine
// holds it).
func servingEngine(failAt int64, gone bool) func(*testing.T, func(), []string) (backends.Backend, func() int64) {
	return func(t *testing.T, cancel func(), _ []string) (backends.Backend, func() int64) {
		e := newEmbedEngine(t, 8)
		e.setRespond(func(w http.ResponseWriter, r *http.Request, n int64, _ []string) bool {
			if n != failAt {
				return false
			}
			if gone {
				cancel()
				<-r.Context().Done()
				return true
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
			return true
		})
		return e.row("serving", backends.ProtocolOpenAI, 10), e.hits.Load
	}
}

// TestEmbedMeterFailoverBooksEachInputOnce (Delta-Review 5, the chat route's
// failover rule): an input a later row served with a usage books that usage
// once — the failed try on the row the walk left books nothing for it; an
// input no block with a usage served by the end of the request books its
// estimate once, also when only part of a block was served (the rows cut
// the request into different blocks: the inputs decide, not the block
// numbers). Engine two prompt tokens per input, 8 runes per input.
//
//   - emb1 ×8, 503 on block 1, the failover serves all: settle 16 (before 21)
//   - emb ×6 (blocks of 3): 12 (before 26); emb ×3 (one block): 6 (before 14)
//   - the first row refuses block 1 with a window 400 (num_ctx 64), the
//     failover row (4096) serves, emb ×3: 6 (before 14) — a refusal the
//     client can provoke
//   - emb1 ×8, failover, then the failover row's block 2 answers 502 or the
//     client goes away during it: 2 + 5 = 7 (before 12)
//   - emb ×12, the first row cut into blocks of 6 (E 6) refuses block 1; the
//     failover row (E 3) serves inputs 1–3, its block 2 (inputs 4–6) answers
//     502: 6 + the estimate of inputs 4–6 once = 20 (before 47); the same with
//     block 2's connect refused (no contact on the failover row): 6 + the
//     first row's unserved rest = 20 (before 33)
//   - TPM 100, emb ×3 of 100 runes (estimate 167 above the bucket), failover
//     served: 6 (before 167)
func TestEmbedMeterFailoverBooksEachInputOnce(t *testing.T) {
	long := []string{strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100)}
	cases := []failoverCase{
		{"emb1 x8, 503 in block 1, the failover serves", "emb1", embedInputs(8), 0, 503, 0, servingEngine(0, false), 200, 1, 8, "settle", 16},
		{"emb x6 in blocks of 3, 503 in block 1, the failover serves", "emb", embedInputs(6), 0, 503, 0, servingEngine(0, false), 200, 1, 2, "settle", 12},
		{"emb x3 in one block, 503, the failover serves", "emb", embedInputs(3), 0, 503, 0, servingEngine(0, false), 200, 1, 1, "settle", 6},
		{"emb x3, a window 400 on the first row, the failover serves", "emb", embedInputs(3), 0, 400, 0, servingEngine(0, false), 200, 1, 1, "settle", 6},
		{"emb1 x8, failover, its block 2 answers 502", "emb1", embedInputs(8), 0, 503, 0, servingEngine(2, false), 0, 1, 2, "settle", 2 + embedReservation(1)},
		{"emb1 x8, failover, the client gone in its block 2", "emb1", embedInputs(8), 0, 503, 0, servingEngine(2, true), 0, 1, 2, "settle", 2 + embedReservation(1)},
		{"emb x12, first row E 6, failover E 3 serves block 1, its block 2 answers 502", "emb", embedInputs(12), 0, 503, 6, servingEngine(2, false), 0, 1, 2, "settle", 6 + embedReservation(3)},
		{"emb x12, first row E 6, failover E 3 serves block 1, its block 2 connect refused", "emb", embedInputs(12), 0, 503, 6, refusingAfterBlock1, 0, 1, 1, "settle", 6 + embedReservation(3)},
		{"TPM 100, emb x3 of 100 runes above the bucket, 503, the failover serves", "emb", long, 100, 503, 0, servingEngine(0, false), 200, 1, 1, "settle", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := newEmbedEngine(t, 8)
			var firstInputs atomic.Int64
			first.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, in []string) bool {
				firstInputs.Store(int64(len(in)))
				w.WriteHeader(tc.refusal)
				if tc.refusal == http.StatusBadRequest {
					_, _ = io.WriteString(w, `{"error":{"message":"input is too long for the context window","type":"invalid_request_error"}}`)
					return true
				}
				_, _ = io.WriteString(w, `{"error":{"message":"engine busy"}}`)
				return true
			})
			r1 := first.row("first", backends.ProtocolOpenAI, 20)
			r2, hits := tc.second(t, cancel, tc.inputs)
			if tc.refusal == http.StatusBadRequest {
				r1.NumCtx, r2.NumCtx = 64, 4096
			}
			tpm := tc.tpm
			if tpm == 0 {
				tpm = 60_000_000
			}
			m := newEmbedMeter(keyquota.Limits{RPM: 1000, TPM: tpm, CompletionReserve: 1024, Concurrency: noL1})
			rig := newEmbedRig(t, []backends.Backend{r2, r1}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m }))
			if tc.firstE > 0 {
				rig.disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
					r1.Host: {Slots: tc.firstE + 1, InternalReserved: 1}, r2.Host: {Slots: 4, InternalReserved: 1},
				}})
			}
			status, body, err := postEmbedAndWait(t, rig, ctx, embedBody(tc.model, tc.inputs))
			ends, settled := m.endsOf(), m.settledNow()
			t.Logf("%s: answer %d err=%v %.80s; hits first %d (%d inputs), failover %d; ends %s; settled %+v (want %s %d, reservation %d)",
				tc.name, status, err, body, first.hits.Load(), firstInputs.Load(), hits(), ends, settled, tc.end, tc.want, embedReservationOf(tc.inputs))
			if first.hits.Load() != tc.firstHit || hits() != tc.hits || (tc.status != 0 && status != tc.status) || (tc.status == 0 && status == http.StatusOK) {
				t.Fatalf("answer %d, hits first %d failover %d — want %d/%d and status %d (0: a failure)", status, first.hits.Load(), hits(), tc.firstHit, tc.hits, tc.status)
			}
			if tc.firstE > 0 && firstInputs.Load() != int64(tc.firstE) {
				t.Fatalf("the first row's block carried %d inputs, want %d — the rows must be cut differently", firstInputs.Load(), tc.firstE)
			}
			if ends != tc.end || len(settled) != 1 || int64(settled[0].PromptTokens) != tc.want {
				t.Errorf("ends %q, settled %+v — want one %s of %d", ends, settled, tc.end, tc.want)
			}
		})
	}
}

// refusingAfterBlock1 is a second row (raw backend, E 3) that serves block 1
// and stops listening before its answer leaves: block 2's connect is refused,
// no byte of it reaches a backend.
func refusingAfterBlock1(t *testing.T, _ func(), inputs []string) (backends.Backend, func() int64) {
	var served atomic.Int64
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		in, ok := readEmbedInputs(br)
		if !ok {
			return
		}
		if in[0] == inputs[0] {
			b.l.stop()
		}
		served.Add(1)
		rawEmbedAnswer(c, in)
	})
	return embedWireRow("raw", b.host(), 10), served.Load
}

// embedReservationOf is the meter's estimate of inputs.
func embedReservationOf(inputs []string) int64 {
	runes := 0
	for _, in := range inputs {
		runes += len([]rune(in))
	}
	return int64(promptguard.TokenEstimate(runes))
}

// TestEmbedMeterRateLimitHeadersBeforeTheFirstByte (Delta-Review 4 F4-8):
// the 200's x-ratelimit-* headers of op embed, read over a real socket —
// what a client sees, the headers as they stood when the status line left
// (an httptest.ResponseRecorder's Header() is the live map and also shows
// headers set after WriteHeader).
func TestEmbedMeterRateLimitHeadersBeforeTheFirstByte(t *testing.T) {
	e := newEmbedEngine(t, 8)
	m := newEmbedMeter(keyquota.Limits{RPM: 10, TPM: 60000, CompletionReserve: 1024, Concurrency: noL1})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m }))
	resp, body, err := postEmbedOverSocket(t, rig, context.Background(), embedBody("emb1", embedInputs(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("on the wire: %d; limit-requests %q remaining-requests %q limit-tokens %q remaining-tokens %q reset-requests %q reset-tokens %q",
		resp.StatusCode, resp.Header.Get("x-ratelimit-limit-requests"), resp.Header.Get("x-ratelimit-remaining-requests"),
		resp.Header.Get("x-ratelimit-limit-tokens"), resp.Header.Get("x-ratelimit-remaining-tokens"),
		resp.Header.Get("x-ratelimit-reset-requests"), resp.Header.Get("x-ratelimit-reset-tokens"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer %d %.300s", resp.StatusCode, body)
	}
	want := map[string]string{
		"x-ratelimit-limit-requests": "10", "x-ratelimit-remaining-requests": "9",
		"x-ratelimit-limit-tokens": "60000", "x-ratelimit-remaining-tokens": fmt.Sprint(60000 - embedReservation(1)),
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("the 200 on the wire: %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if resp.Header.Get(k) == "" {
			t.Errorf("the 200 on the wire: %s missing", k)
		}
	}
}

// TestEmbedMeterRetryAfterAFailedHandshakeSettles (Delta-Review 4 F4-7) is
// the chat pin TestMeterWireRetryAfterAFailedHandshakeSettles over the
// embeddings route: HTTP/2, the first try's handshake is reset (no byte of
// the request left), DoRetryOnce's retry reaches the backend, which answers
// with usage 7 — the retry's wire contact counts: one Settle of 7, not the
// full refund of a served answer.
func TestEmbedMeterRetryAfterAFailedHandshakeSettles(t *testing.T) {
	rl := &resetFirst{Listener: newHeldListener(t)}
	var hits atomic.Int64
	url := serveH2(t, rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, embedAnswerUsage(in.Input, 8, 7))
	}))
	rig, m := embedMeterRig(t, embedWireRow("flaky", url, 10))
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(1)))
	held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
	t.Logf("handshake reset, retry served: answer %d; accepted %d; hits %d; ends %s; settled %+v; held %d", a.status, rl.n.Load(), hits.Load(), ends, settled, held)
	if a.status != http.StatusOK || rl.n.Load() != 2 || hits.Load() != 1 {
		t.Fatalf("answer %d %.200s, accepted %d, hits %d — want the retry served", a.status, a.body, rl.n.Load(), hits.Load())
	}
	if ends != "settle" || held != 7 || len(settled) != 1 || settled[0].PromptTokens != 7 {
		t.Errorf("ends %q, settled %+v, the bucket holds %d — want the retry's usage 7 settled", ends, settled, held)
	}
}

// TestEmbedMeterBlockEstimateStopsAtTheReservation (F4-1, Delta-Review 5
// F5-1): emb1, eight blocks, block 8 answers 502 after seven served blocks
// (reservation 36, block estimate 5). Five tokens each ⇒ 35 + 5 = 40 ≥ 36
// while the reported 35 is below it: the reservation ends with Keep, the
// bucket keeps 36 — never a Settle of more than it holds. 25 + six × 1 = 31
// ⇒ 31 + 5 = 36 reaches it exactly: keep as well. Six each ⇒ the reported
// 42 is above the reservation already: booked as reported, settle 42, no
// estimate on top.
func TestEmbedMeterBlockEstimateStopsAtTheReservation(t *testing.T) {
	cases := []struct {
		name     string
		perBlock func(n int64) int
		end      string
		want     int64
	}{
		{"served 35, estimate past the reservation", func(int64) int { return 5 }, "keep", embedReservation(8)},
		{"served 31, estimate up to the reservation", func(n int64) int {
			if n == 1 {
				return 25
			}
			return 1
		}, "keep", embedReservation(8)},
		{"served 42 above the reservation", func(int64) int { return 6 }, "settle", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, inputs []string) bool {
				if n == 8 {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
					return true
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, embedAnswerUsage(inputs, 8, tc.perBlock(n)))
				return true
			})
			rig, m := embedMeterRig(t, e.row("spark-embed", backends.ProtocolOpenAI, 10))
			a := rig.postEmbed(t, embedBody("emb1", embedInputs(8)))
			held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
			t.Logf("%s: answer %d; hits %d; ends %s; settled %+v; held %d (want %s %d, reservation %d)", tc.name, a.status, e.hits.Load(), ends, settled, held, tc.end, tc.want, embedReservation(8))
			if a.status == http.StatusOK || e.hits.Load() != 8 {
				t.Fatalf("answer %d, hits %d — want block 8 to fail after seven served", a.status, e.hits.Load())
			}
			if ends != tc.end || held != tc.want {
				t.Errorf("ends %q, the bucket holds %d — want %s %d", ends, held, tc.end, tc.want)
			}
			switch {
			case tc.end == "settle" && (len(settled) != 1 || int64(settled[0].PromptTokens) != tc.want):
				t.Errorf("settled %+v, want one Settle of %d", settled, tc.want)
			case tc.end == "keep" && len(settled) != 0:
				t.Errorf("settled %+v, want none — the reservation keeps its charge", settled)
			}
		})
	}
}

// capacityHidden is the rig's real meter behind a limiter whose admission
// reports no bucket capacity (no x-ratelimit-limit-tokens).
type capacityHidden struct{ *embedMeter }

func (m capacityHidden) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := m.embedMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return capacityHiddenReservation{res}, nil
}

type capacityHiddenReservation struct{ oai.LimitReservation }

func (r capacityHiddenReservation) Headers() http.Header {
	h := r.LimitReservation.Headers()
	h.Del("x-ratelimit-limit-tokens")
	return h
}

// nextEmbedAdmission is one zero-token admission of the rig's consumer under
// op embed (it counts a request): refused exactly when the token bucket holds
// more than its capacity — debt the next request waits off.
func (m *embedMeter) nextEmbedAdmission(rig *chatRig) keyquota.Decision {
	ca := *rig.ca.Load()
	_, d := m.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpEmbed}, m.lim, keyquota.Estimate{ClientMax: true})
	return d
}

// postEmbedAndWait is postEmbedOverSocket that returns only after the route's
// handler returned (httptest.Server.Close waits for it): the reservation has
// ended, also behind a client that went away.
func postEmbedAndWait(t *testing.T, rig *chatRig, ctx context.Context, body string) (int, string, error) {
	t.Helper()
	srv := httptest.NewServer(rig.router)
	defer srv.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	srv.Close() // idempotent; the deferred one covers the error path
	return resp.StatusCode, string(b), nil
}

// TestEmbedMeterEstimateNeverAboveKeep (Delta-Review 5 F5-1): the estimate
// of the blocks that met a backend without a usage never charges more than
// the reservation holds — the estimate of all inputs, at most the bucket's
// capacity the admission reports (keyquota charges a reservation above it at
// the capacity, B-06-28). TPM 100, emb1, inputs of 8 and 800 runes: estimate
// 449 (block 2: 445), charged 100. Block 1 reports u tokens, the client goes
// away while the engine holds block `gone`. u + 445 ≥ 100 with u below it ⇒
// keep: the bucket keeps 100 and the next admission passes (before: settle
// 447 and 3 min 28 s of debt). A reported usage above the reservation (300)
// is booked as reported. A limiter that reports no capacity in its headers
// changes nothing: the route reads what the admission charged from the
// reservation (LimitReservation.Held, W02-09 rebase round 2, part C) — 2 +
// 445 and 4 + 445 both reach 100 ⇒ keep 100, the next admission passes
// (before, the estimate stood in as the ceiling: 2 + 445 = 447 < 449
// settled 447, 3 min 28 s of debt, against a charge of 100).
func TestEmbedMeterEstimateNeverAboveKeep(t *testing.T) {
	inputs := []string{"text 000", strings.Repeat("x", 800)}
	cases := []struct {
		name   string
		gone   int64
		u      int
		hidden bool
		end    string
		want   int64 // the one Settle, or what the bucket keeps
		next   bool  // a zero-token admission right after passes
	}{
		{"client gone in block 1", 1, 2, false, "keep", 100, true},
		{"client gone in block 2 after 2 reported", 2, 2, false, "keep", 100, true},
		{"block 1 reports 300 above the reservation, gone in block 2", 2, 300, false, "settle", 300, false},
		{"no capacity reported, 2 + 445 above what the admission charged", 2, 2, true, "keep", 100, true},
		{"no capacity reported, 4 + 445 above what the admission charged", 2, 4, true, "keep", 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.setRespond(func(w http.ResponseWriter, r *http.Request, n int64, in []string) bool {
				if n == tc.gone {
					cancel()
					<-r.Context().Done()
					return true
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, embedAnswerUsage(in, 8, tc.u))
				return true
			})
			m := newEmbedMeter(keyquota.Limits{RPM: 1000, TPM: 100, CompletionReserve: 1024, Concurrency: noL1})
			var lim oai.Limiter = m
			if tc.hidden {
				lim = capacityHidden{m}
			}
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = lim }))
			status, body, err := postEmbedAndWait(t, rig, ctx, embedBody("emb1", inputs))
			ends, settled := m.endsOf(), m.settledNow()
			next := m.nextEmbedAdmission(rig)
			t.Logf("%s: answer %d err=%v %.80s; engine hits %d; ends %s; settled %+v; next admission allowed %v retry-after %v remaining %d (want %s %d, next %v)",
				tc.name, status, err, body, e.hits.Load(), ends, settled, next.Allowed, next.RetryAfter, next.Headers.RemainingTokens, tc.end, tc.want, tc.next)
			if e.hits.Load() != tc.gone {
				t.Fatalf("engine hits %d — want the client gone during block %d", e.hits.Load(), tc.gone)
			}
			if ends != tc.end {
				t.Errorf("ends %q, want %s", ends, tc.end)
			}
			switch {
			case tc.end == "settle" && (len(settled) != 1 || int64(settled[0].PromptTokens) != tc.want):
				t.Errorf("settled %+v, want one Settle of %d", settled, tc.want)
			case tc.end == "keep" && len(settled) != 0:
				t.Errorf("settled %+v, want none — the reservation keeps its charge", settled)
			}
			if next.Allowed != tc.next {
				t.Errorf("the next admission allowed %v (retry-after %v), want %v", next.Allowed, next.RetryAfter, tc.next)
			}
			if tc.next && m.lim.TPM-next.Headers.RemainingTokens != tc.want {
				t.Errorf("the bucket holds %d, want %d", m.lim.TPM-next.Headers.RemainingTokens, tc.want)
			}
		})
	}
}

// TestEmbedMeterContextRuleMarksTheBlock (Delta-Review 5 F5-3, the probe
// TestR5ContextRuleInBlock2): the context rule (a block that ended by its
// context with a connection in hand) marks the block's own contact, not only
// the request's. HTTP/2, emb1 × 2: block 1 is served (two tokens), block 2's
// try stalls in GotConn past its 1 s attempt deadline — no write hook fires,
// the h2 writer could still write: block 2 books its estimate, settle
// 2 + 5 = 7 (with the rule marking only the request: settle 2).
func TestEmbedMeterContextRuleMarksTheBlock(t *testing.T) {
	var hits atomic.Int64
	url := serveH2(t, newHeldListener(t), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, embedAnswerUsage(in.Input, 8, 2*len(in.Input)))
	}))
	row := embedWireRow("h2", url, 10)
	row.Timeouts = nil
	rig, m := embedMeterRig(t, row)
	const attemptDeadline = time.Second
	rig.setSettings(func(s *oai.Settings) { s.RequestTimeout = attemptDeadline })
	pt := &h2Trace{}
	tr := pt.trace()
	record := tr.GotConn
	// Block 2 is the first connection asked for after the handler served
	// block 1 — not the second GetConn: net/http can ask twice for block 1's
	// connection (1 of 200 under -race: tries 2, hits 0).
	var gets, start atomic.Int64
	var armed atomic.Bool
	tr.GetConn = func(string) {
		gets.Add(1)
		if hits.Load() >= 1 && armed.CompareAndSwap(false, true) {
			start.Store(time.Now().UnixNano())
		}
	}
	tr.GotConn = func(i httptrace.GotConnInfo) {
		record(i)
		if armed.Load() {
			time.Sleep(time.Until(time.Unix(0, start.Load()).Add(attemptDeadline + 200*time.Millisecond)))
		}
	}
	a := postEmbedCtx(t, rig, httptrace.WithClientTrace(context.Background(), tr), embedBody("emb1", embedInputs(2)))
	held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
	want := 2 + embedReservation(1)
	t.Logf("context rule in block 2: answer %d %.60s; tries %d; connections %s; handler hits %d; ends %s; settled %+v; held %d (want settle %d)",
		a.status, a.body, gets.Load(), pt, hits.Load(), ends, settled, held, want)
	if !armed.Load() || hits.Load() != 1 || !strings.HasPrefix(pt.String(), "h2") || a.status == http.StatusOK {
		t.Fatalf("tries %d, handler hits %d, connections %q, answer %d — want block 1 served over h2 and block 2 ended at its deadline", gets.Load(), hits.Load(), pt, a.status)
	}
	if ends != "settle" || held != want || len(settled) != 1 || int64(settled[0].PromptTokens) != want {
		t.Errorf("ends %q, settled %+v, the bucket holds %d — want one Settle of %d", ends, settled, held, want)
	}
}

// resetAt resets the raw connections a listener accepts whose number (from
// 1) reset selects — before the TLS handshake: no byte of a request leaves.
type resetAt struct {
	net.Listener
	n     atomic.Int64
	reset func(k int64) bool
}

func (l *resetAt) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.reset(l.n.Add(1)) {
			reset(c)
			continue
		}
		return c, nil
	}
}

// serveTLSH1 serves h over TLS with the test CA the rig trusts, HTTP/1.1
// only; with Connection: close on every answer each block has its own
// connection and handshake.
func serveTLSH1(t *testing.T, l net.Listener, h http.Handler) string {
	t.Helper()
	h1 := new(http.Protocols)
	h1.SetHTTP1(true)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Protocols: h1}
	go func() { _ = srv.ServeTLS(l, wireCertFile, wireKeyFile) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + l.Addr().String()
}

// TestEmbedMeterRetryContactMarksTheBlock (Delta-Review 5 F5-4, the probe
// TestR5TLSHandshakeInBlock3 "retry 502"): the block's own trace marks in
// every try of httpx.DoRetryOnce, not only in the first. TLS, HTTP/1.1, one
// connection per block, emb1 × 8: blocks 1–2 are served (two tokens each);
// block 3's first handshake is reset (no byte leaves), its retry reaches the
// backend, which answers 502 — the retry's contact books block 3's
// estimate: settle 4 + 5 = 9 (with the block trace marking only the first
// try: settle 4).
func TestEmbedMeterRetryContactMarksTheBlock(t *testing.T) {
	rl := &resetAt{Listener: newHeldListener(t), reset: func(k int64) bool { return k == 3 }}
	inputs := embedInputs(8)
	var hits atomic.Int64
	url := serveTLSH1(t, rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &in)
		w.Header().Set("Connection", "close")
		if len(in.Input) > 0 && in.Input[0] == inputs[2] {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, embedAnswerUsage(in.Input, 8, 2*len(in.Input)))
	}))
	rig, m := embedMeterRig(t, embedWireRow("tls", url, 10))
	a := rig.postEmbed(t, embedBody("emb1", inputs))
	held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
	want := 4 + embedReservation(1)
	t.Logf("block 3 handshake reset, retry 502: answer %d %.60s; accepted %d; handler hits %d; ends %s; settled %+v; held %d (want settle %d)",
		a.status, a.body, rl.n.Load(), hits.Load(), ends, settled, held, want)
	if rl.n.Load() != 4 || hits.Load() != 3 || a.status == http.StatusOK {
		t.Fatalf("accepted %d, handler hits %d, answer %d — want blocks 1–2 served, block 3's first handshake reset and its retry answered 502", rl.n.Load(), hits.Load(), a.status)
	}
	if ends != "settle" || held != want || len(settled) != 1 || int64(settled[0].PromptTokens) != want {
		t.Errorf("ends %q, settled %+v, the bucket holds %d — want one Settle of %d", ends, settled, held, want)
	}
}
