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
	"math"
	"net"
	"net/http"
	"net/http/httptest"
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

// TestEmbedMeterFailoverAfterTheWireBooksTheFirstRowsBlock (F4-1, every wire
// call of a block): emb1, eight inputs; the first row (prio 20) reads block
// 1 and answers 503, the walker fails over to the second row, which serves
// all eight blocks (two prompt tokens each). Block 1's wire call on the first
// row met a backend without a usage: settle 16 + 5 = 21 (before the rule:
// 16 — the first row's work went back in full).
func TestEmbedMeterFailoverAfterTheWireBooksTheFirstRowsBlock(t *testing.T) {
	first := newEmbedEngine(t, 8)  // prio 20: tried first
	second := newEmbedEngine(t, 8) // prio 10: the failover row
	first.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"engine busy"}}`)
		return true
	})
	rig, m := embedMeterRig(t, second.row("spark1", backends.ProtocolOpenAI, 10), first.row("spark2", backends.ProtocolOpenAI, 20))
	a := rig.postEmbed(t, embedBody("emb1", embedInputs(8)))
	held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
	want := 16 + embedReservation(1)
	t.Logf("first row 503 after the wire, failover served: answer %d; hits first %d, failover %d; ends %s; settled %+v; held %d (want settle %d, reservation %d)",
		a.status, first.hits.Load(), second.hits.Load(), ends, settled, held, want, embedReservation(8))
	if a.status != http.StatusOK || first.hits.Load() != 1 || second.hits.Load() != 8 {
		t.Fatalf("answer %d %.200s, hits %d/%d — want 200 after one wire call on the first row and eight on the second", a.status, a.body, first.hits.Load(), second.hits.Load())
	}
	if ends != "settle" || held != want || len(settled) != 1 || int64(settled[0].PromptTokens) != want {
		t.Errorf("ends %q, settled %+v, the bucket holds %d — want one Settle of %d", ends, settled, held, want)
	}
}

// TestEmbedMeterRateLimitHeadersBeforeTheFirstByte (Delta-Review 4 F4-8):
// the 200's x-ratelimit-* headers of op embed, read over a real socket —
// what a client sees, the headers as they stood when the status line left
// (an httptest.ResponseRecorder's Header() is the live map and also shows
// headers set after WriteHeader).
func TestEmbedMeterRateLimitHeadersBeforeTheFirstByte(t *testing.T) {
	e := newEmbedEngine(t, 8)
	m := newEmbedMeter(keyquota.Limits{RPM: 10, TPM: 60000, CompletionReserve: 1024})
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
// is booked as reported. A limiter that reports no capacity leaves the
// estimate as the ceiling: 2 + 445 = 447 < 449 settles 447 (named); 4 + 445
// = 449 reaches it ⇒ keep, and the bucket keeps what the admission charged
// (100) — a Settle of the ceiling would book 449.
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
		{"no capacity reported, 2 + 445 below the estimate", 2, 2, true, "settle", 447, false},
		{"no capacity reported, 4 + 445 reaches the estimate", 2, 4, true, "keep", 100, true},
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
			m := newEmbedMeter(keyquota.Limits{RPM: 1000, TPM: 100, CompletionReserve: 1024})
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
