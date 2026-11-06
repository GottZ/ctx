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

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
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

// rawEmbedAnswer answers one OpenAI embeddings request read from br with two
// prompt tokens per input and Connection: close (one connection per block).
func rawEmbedAnswer(c net.Conn, br *bufio.Reader) bool {
	req, err := http.ReadRequest(br)
	if err != nil {
		return false
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return false
	}
	var in struct {
		Input []string `json:"input"`
	}
	_ = json.Unmarshal(raw, &in)
	ans := embedAnswerUsage(in.Input, 8, 2*len(in.Input))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(ans), ans)
	return true
}

// TestEmbedMeterBlockTransportFault (F4-1): block 3 of 8 meets a transport
// fault after two served blocks (raw backend, one connection per block).
// The connect refused ⇒ block 3 reached no backend: settle 4, the served
// blocks' usage exactly. The backend read block 3 and died (RST), the retry's
// connect refused ⇒ block 3 had wire contact: settle 4 + 5 = 9.
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
			var reqs atomic.Int64
			b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
				k := reqs.Add(1)
				if k == 3 && tc.contact {
					if b.read(br) {
						b.l.stop()
						reset(c)
					}
					return
				}
				if rawEmbedAnswer(c, br) && k == 2 && !tc.contact {
					b.l.stop()
				}
			})
			rig, m := embedMeterRig(t, embedWireRow("raw", b.host(), 10))
			a := rig.postEmbed(t, embedBody("emb1", embedInputs(8)))
			held, ends, settled := m.heldEmbed(t, rig), m.endsOf(), m.settledNow()
			want := int64(4)
			if tc.contact {
				want += embedReservation(1)
			}
			t.Logf("block 3 %s: answer %d %.80s; conns %d, requests read %d; ends %s; settled %+v; held %d (want settle %d)",
				tc.mode, a.status, a.body, b.conns.Load(), b.requests.Load(), ends, settled, held, want)
			if a.status == http.StatusOK || reqs.Load() < 2 {
				t.Fatalf("answer %d, requests %d — want block 3 to fail after two served blocks", a.status, reqs.Load())
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
