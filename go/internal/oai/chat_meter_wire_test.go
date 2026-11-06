package oai_test

// The meter's wire contact at the chat route (W06-10 hardening 3, H3-1;
// Fremd-Review 4 F4-1, F4-2, F4-7): a reservation goes back in full (Abort)
// exactly when no byte of the request was written to a backend in any
// attempt, the first try of httpx.DoRetryOnce included; otherwise the usage
// settles it or, without one, it keeps its charge. Every case runs over the
// rig's real planner, dispatcher and meter, against transport faults that
// are deterministic (wire_sockets_linux_test.go) — HTTP/1.1 and HTTP/2.
// chatBody reserves 1 042 tokens (18 for the messages, 1 024 completion
// reserve); held is what the bucket holds after the request.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
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
)

const wireReservation = 1042

// endMeter is the rig's real meter that also names how each reservation
// ended (Abort and a Settle of 0 leave the same bucket).
type endMeter struct {
	*rigMeter
	mu   sync.Mutex
	ends []string
}

func newEndMeter() *endMeter {
	return &endMeter{rigMeter: newRigMeter(keyquota.Limits{Concurrency: noL1, RPM: 1000, TPM: 60_000_000, CompletionReserve: 1024}, true)}
}

func (m *endMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := m.rigMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return endReservation{LimitReservation: res, m: m}, nil
}

func (m *endMeter) end(kind string) {
	m.mu.Lock()
	m.ends = append(m.ends, kind)
	m.mu.Unlock()
}

// endsOf is the list of ends so far, comma-joined.
func (m *endMeter) endsOf() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.ends, ",")
}

// held is what the bucket of the rig's consumer holds now.
func (m *endMeter) held(t *testing.T, rig *chatRig) int64 {
	t.Helper()
	ca := *rig.ca.Load()
	return m.lim.TPM - m.probe(t, &ca, keyquota.OpChat).RemainingTokens
}

type endReservation struct {
	oai.LimitReservation
	m *endMeter
}

func (r endReservation) Settle(u dispatch.Usage) { r.m.end("settle"); r.LimitReservation.Settle(u) }
func (r endReservation) Keep()                   { r.m.end("keep"); r.LimitReservation.Keep() }
func (r endReservation) Abort()                  { r.m.end("abort"); r.LimitReservation.Abort() }
func (r endReservation) Reject(reason string, waited time.Duration) {
	r.m.end("reject")
	r.LimitReservation.Reject(reason, waited)
}

// wireRow is a chat row on host (the rig's row shape).
func wireRow(name, host string, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: host, Protocol: backends.ProtocolOpenAI, Model: "MODEL-CANARY",
		Roles: []string{"chat"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"chat": 30},
	}
}

// wireRig mounts rows over a fresh endMeter.
func wireRig(t *testing.T, rows ...backends.Backend) (*chatRig, *endMeter) {
	t.Helper()
	m := newEndMeter()
	return newChatRig(t, rows, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m })), m
}

// postTraced sends chatBody with trace on the inbound request context — the
// route composes its own trace with it — and survives a panic of the route.
func postTraced(t *testing.T, rig *chatRig, trace *httptrace.ClientTrace) chatAnswer {
	t.Helper()
	ctx := context.Background()
	if trace != nil {
		ctx = httptrace.WithClientTrace(ctx, trace)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("chat"))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		rig.router.ServeHTTP(w, req)
	}()
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// rawBackend is an HTTP/1.1 backend on a held listener; handle runs per
// accepted connection (n counts them from 1).
type rawBackend struct {
	l        *heldListener
	conns    atomic.Int64
	requests atomic.Int64 // complete requests read (headers and body)
	done     chan struct{}
}

func newRawBackend(t *testing.T, handle func(b *rawBackend, c net.Conn, br *bufio.Reader, n int64)) *rawBackend {
	t.Helper()
	b := &rawBackend{l: newHeldListener(t), done: make(chan struct{})}
	t.Cleanup(func() { close(b.done) })
	go func() {
		for {
			c, err := b.l.Accept()
			if err != nil {
				return
			}
			n := b.conns.Add(1)
			go func() {
				defer func() { _ = c.Close() }()
				handle(b, c, bufio.NewReader(c), n)
			}()
		}
	}()
	return b
}

func (b *rawBackend) host() string { return "http://" + b.l.Addr().String() }

// read reads one complete request from br.
func (b *rawBackend) read(br *bufio.Reader) bool {
	req, err := http.ReadRequest(br)
	if err != nil {
		return false
	}
	if _, err := io.ReadAll(req.Body); err != nil {
		return false
	}
	b.requests.Add(1)
	return true
}

// answerKeepAlive writes answerSGLang (usage 11 + 3) as a keep-alive response.
func answerKeepAlive(c net.Conn) {
	_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(answerSGLang), answerSGLang)
}

// reset closes c with an RST (linger 0) — a backend process that dies.
func reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}

// shortAttempts gives the rig's rows no role timeout and the attempts a
// deadline of 250 ms (gateway.request_timeout, the rows' fallback).
func shortAttempts(rig *chatRig) {
	rig.setSettings(func(s *oai.Settings) { s.RequestTimeout = 250 * time.Millisecond })
}

func noTimeouts(b backends.Backend) backends.Backend {
	b.Timeouts = nil
	return b
}

// TestMeterWireReadThenResetKeeps is gate (a) (F4-1): the backend reads the
// whole request and dies (RST); DoRetryOnce's retry finds the host refusing
// the connection. The first try wrote the request, so the reservation keeps
// its charge — the last try's error (a refused connect) does not decide.
func TestMeterWireReadThenResetKeeps(t *testing.T) {
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		if b.read(br) {
			b.l.stop()
			reset(c)
		}
	})
	rig, m := wireRig(t, wireRow("crash", b.host(), 10))
	a := rig.post(t, chatBody("chat"))
	held := m.held(t, rig)
	t.Logf("read, reset, retry refused: answer %d; backend conns %d, requests read %d; ends %s; the bucket holds %d tokens",
		a.status, b.conns.Load(), b.requests.Load(), m.endsOf(), held)
	if b.requests.Load() != 1 || b.conns.Load() != 1 {
		t.Fatalf("backend conns %d, requests %d — want one connection that read the request", b.conns.Load(), b.requests.Load())
	}
	if held != wireReservation || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a backend read the request, want the whole reservation %d kept",
			held, m.endsOf(), wireReservation)
	}
}

// TestMeterWireConnectTimeoutGivesBack is gate (c) (F4-2): the backend's
// host drops every SYN; the attempt ends by its deadline in the connect.
// No byte of the request left: Abort.
func TestMeterWireConnectTimeoutGivesBack(t *testing.T) {
	l := newHeldListener(t)
	l.dropSYNs()
	rig, m := wireRig(t, noTimeouts(wireRow("blackhole", "http://"+l.Addr().String(), 10)))
	shortAttempts(rig)
	start := time.Now()
	a := rig.post(t, chatBody("chat"))
	el := time.Since(start)
	held := m.held(t, rig)
	t.Logf("connect timeout: answer %d after %v; ends %s; the bucket holds %d tokens", a.status, el.Round(time.Millisecond), m.endsOf(), held)
	if el < 200*time.Millisecond {
		t.Fatalf("answered after %v — the connect did not hang until the attempt deadline", el)
	}
	if held != 0 || m.endsOf() != "abort" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a connect that timed out, want 0 by Abort", held, m.endsOf())
	}
}

// TestMeterWireTLSHandshakeFailureGivesBack is gate (d) (F4-2): the TLS
// handshake fails — https against a plain HTTP server (only the ClientHello
// left), a certificate under an unknown CA — so no request byte was sent:
// Abort.
func TestMeterWireTLSHandshakeFailureGivesBack(t *testing.T) {
	t.Run("https against plain HTTP", func(t *testing.T) {
		b := newRawBackend(t, func(_ *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
			hello := make([]byte, 5)
			_, _ = io.ReadFull(br, hello)
			_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		})
		rig, m := wireRig(t, wireRow("plain", "https://"+b.l.Addr().String(), 10))
		a := rig.post(t, chatBody("chat"))
		held := m.held(t, rig)
		t.Logf("https against plain HTTP: answer %d; conns %d, requests %d; ends %s; the bucket holds %d tokens",
			a.status, b.conns.Load(), b.requests.Load(), m.endsOf(), held)
		if b.conns.Load() == 0 {
			t.Fatal("the backend saw no connection — the case needs a failed handshake")
		}
		if held != 0 || m.endsOf() != "abort" {
			t.Errorf("the bucket holds %d tokens (ends %s) after a failed TLS handshake, want 0 by Abort", held, m.endsOf())
		}
	})
	t.Run("unknown CA", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		t.Cleanup(srv.Close)
		rig, m := wireRig(t, wireRow("x509", srv.URL, 10))
		a := rig.post(t, chatBody("chat"))
		held := m.held(t, rig)
		t.Logf("unknown CA: answer %d; handler hits %d; ends %s; the bucket holds %d tokens", a.status, hits.Load(), m.endsOf(), held)
		if hits.Load() != 0 {
			t.Fatal("the handler ran — the certificate was trusted")
		}
		if held != 0 || m.endsOf() != "abort" {
			t.Errorf("the bucket holds %d tokens (ends %s) after a failed TLS handshake, want 0 by Abort", held, m.endsOf())
		}
	})
}

// TestMeterWireNoAnswerKeeps is gate (e): the connection stands, the backend
// reads the request and never answers, the attempt ends by its deadline —
// the backend has the request: Keep.
func TestMeterWireNoAnswerKeeps(t *testing.T) {
	b := newRawBackend(t, func(b *rawBackend, _ net.Conn, br *bufio.Reader, _ int64) {
		b.read(br)
		<-b.done
	})
	rig, m := wireRig(t, noTimeouts(wireRow("silent", b.host(), 10)))
	shortAttempts(rig)
	a := rig.post(t, chatBody("chat"))
	held := m.held(t, rig)
	t.Logf("no answer, attempt deadline: answer %d; requests read %d; ends %s; the bucket holds %d tokens", a.status, b.requests.Load(), m.endsOf(), held)
	if held != wireReservation || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a backend took the request and timed out, want %d kept",
			held, m.endsOf(), wireReservation)
	}
}

// reuseTrace counts the tries of one request and the reused connections it
// got (net/http's own transparent retry would show as a third GetConn).
type reuseTrace struct {
	getConns, gotConns, reused atomic.Int64
}

func (r *reuseTrace) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { r.getConns.Add(1) },
		GotConn: func(i httptrace.GotConnInfo) {
			r.gotConns.Add(1)
			if i.Reused {
				r.reused.Add(1)
			}
		},
	}
}

// TestMeterWireReusedConnection is gate (f): a keep-alive connection of an
// earlier request, closed by the backend.
//
//   - Closed while idle (FIN, and the client's read loop has seen it):
//     net/http drops the connection from its pool and dials afresh; the
//     dial is refused — no byte of the second request left: Abort.
//   - Closed when the request arrives (the backend reads it on the reused
//     connection and dies): net/http does not retry a POST whose bytes it
//     wrote (not replayable), DoRetryOnce retries once on a fresh connection,
//     which is refused — the first try wrote the request: Keep.
func TestMeterWireReusedConnection(t *testing.T) {
	t.Run("closed while idle", func(t *testing.T) {
		dropped := make(chan struct{})
		b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
			if !b.read(br) {
				return
			}
			answerKeepAlive(c)
			_ = c.(*net.TCPConn).CloseWrite()
			_, _ = io.Copy(io.Discard, br) // until the client closes its end
			close(dropped)
		})
		rig, m := wireRig(t, wireRow("idle", b.host(), 10))
		if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
			t.Fatalf("first request: %d %s", a.status, a.body)
		}
		select {
		case <-dropped:
		case <-time.After(10 * time.Second):
			t.Fatal("the client never closed the connection the backend closed")
		}
		b.l.stop()
		before := m.held(t, rig)
		rt := &reuseTrace{}
		a := postTraced(t, rig, rt.trace())
		after := m.held(t, rig)
		t.Logf("idle connection closed: answer %d; tries %d, connections got %d (reused %d); backend conns %d; ends %s; the second request holds %d tokens",
			a.status, rt.getConns.Load(), rt.gotConns.Load(), rt.reused.Load(), b.conns.Load(), m.endsOf(), after-before)
		if rt.gotConns.Load() != 0 || b.conns.Load() != 1 {
			t.Fatalf("connections got %d, backend conns %d — want the closed connection dropped and the fresh dial refused", rt.gotConns.Load(), b.conns.Load())
		}
		if after-before != 0 || m.endsOf() != "settle,abort" {
			t.Errorf("the second request holds %d tokens (ends %s), want 0 by Abort", after-before, m.endsOf())
		}
	})
	t.Run("closed when the request arrives", func(t *testing.T) {
		b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
			if !b.read(br) {
				return
			}
			answerKeepAlive(c)
			if b.read(br) {
				b.l.stop()
				reset(c)
			}
		})
		rig, m := wireRig(t, wireRow("reuse", b.host(), 10))
		if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
			t.Fatalf("first request: %d %s", a.status, a.body)
		}
		before := m.held(t, rig)
		rt := &reuseTrace{}
		a := postTraced(t, rig, rt.trace())
		after := m.held(t, rig)
		t.Logf("closed on arrival: answer %d; tries %d, connections got %d (reused %d); backend conns %d, requests read %d; ends %s; the second request holds %d tokens",
			a.status, rt.getConns.Load(), rt.gotConns.Load(), rt.reused.Load(), b.conns.Load(), b.requests.Load(), m.endsOf(), after-before)
		if rt.reused.Load() != 1 || rt.getConns.Load() != 2 || b.conns.Load() != 1 || b.requests.Load() != 2 {
			t.Fatalf("tries %d, reused %d, backend conns %d, requests %d — want the request read on the reused connection, one retry by DoRetryOnce (refused), no retry by net/http",
				rt.getConns.Load(), rt.reused.Load(), b.conns.Load(), b.requests.Load())
		}
		if after-before != wireReservation || m.endsOf() != "settle,keep" {
			t.Errorf("the second request holds %d tokens (ends %s), want its whole reservation %d kept", after-before, m.endsOf(), wireReservation)
		}
	})
}

// TestMeterWireMixedAttempts is gate (g): two rows, the first tried first.
// One attempt with wire contact decides; a refused connect adds nothing.
func TestMeterWireMixedAttempts(t *testing.T) {
	busy := func(t *testing.T) *upstream {
		u := newUpstream(t, answerSGLang)
		u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"busy"}}`)
		})
		return u
	}
	cases := []struct {
		name string
		rows func(t *testing.T) []backends.Backend
		held int64
		ends string
	}{
		{"refused, then 503", func(t *testing.T) []backends.Backend {
			return []backends.Backend{busy(t).row("b503", backends.TrustFull, 10), wireRow("dead", "http://"+refusedAddr(t), 20)}
		}, wireReservation, "keep"},
		{"503, then refused", func(t *testing.T) []backends.Backend {
			return []backends.Backend{wireRow("dead", "http://"+refusedAddr(t), 10), busy(t).row("a503", backends.TrustFull, 20)}
		}, wireReservation, "keep"},
		{"refused, then 200 with usage", func(t *testing.T) []backends.Backend {
			return []backends.Backend{newUpstream(t, answerSGLang).row("b200", backends.TrustFull, 10), wireRow("dead", "http://"+refusedAddr(t), 20)}
		}, 14, "settle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, m := wireRig(t, tc.rows(t)...)
			a := rig.post(t, chatBody("chat"))
			held := m.held(t, rig)
			t.Logf("%s: answer %d; ends %s; the bucket holds %d tokens", tc.name, a.status, m.endsOf(), held)
			if held != tc.held || m.endsOf() != tc.ends {
				t.Errorf("%s: the bucket holds %d tokens (ends %s), want %d (%s)", tc.name, held, m.endsOf(), tc.held, tc.ends)
			}
		})
	}
}

// h2Trace records the protocol of the connections a request got.
type h2Trace struct {
	mu     sync.Mutex
	protos []string
}

func (h *h2Trace) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		p := negotiatedProtocol(i.Conn)
		if p == "" {
			p = "plain"
		}
		h.mu.Lock()
		h.protos = append(h.protos, p)
		h.mu.Unlock()
	}}
}

func (h *h2Trace) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.protos, ",")
}

// TestMeterWireHTTP2 is gate (h): the pool clients negotiate HTTP/2 over
// TLS (the guarded rows force it); the wire trace fires there as well. Each
// case checks that the connection spoke h2.
func TestMeterWireHTTP2(t *testing.T) {
	cases := []struct {
		name    string
		handler func(l *heldListener, raw *connLog) http.HandlerFunc
		extra   map[string]string
		hits    int64 // handler calls the case needs
		held    int64
		ends    string
	}{
		{"answer with usage", func(*heldListener, *connLog) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, answerSGLang)
			}
		}, nil, 1, 14, "settle"},
		{"request read, connection reset, retry refused", func(l *heldListener, raw *connLog) http.HandlerFunc {
			return func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				l.stop()
				reset(<-raw.conns) // the one TCP connection under the TLS one
			}
		}, nil, 1, wireReservation, "keep"},
		{"request read, stream reset", func(*heldListener, *connLog) http.HandlerFunc {
			return func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				panic(http.ErrAbortHandler)
			}
		}, nil, 1, wireReservation, "keep"},
		{"a header HTTP/2 refuses: a connection, no request byte", func(*heldListener, *connLog) http.HandlerFunc {
			return func(http.ResponseWriter, *http.Request) {}
		}, map[string]string{"Upgrade": "websocket"}, 0, 0, "abort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newHeldListener(t)
			raw := newConnLog(l)
			var hits, h2 atomic.Int64
			h := tc.handler(l, raw)
			url := serveH2(t, raw, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.ProtoMajor == 2 {
					h2.Add(1)
				}
				h(w, r)
			}))
			row := wireRow("h2", url, 10)
			row.ExtraHeaders = tc.extra
			rig, m := wireRig(t, row)
			pt := &h2Trace{}
			a := postTraced(t, rig, pt.trace())
			held := m.held(t, rig)
			t.Logf("%s: answer %d; connections %s; handler hits %d (h2 %d); ends %s; the bucket holds %d tokens",
				tc.name, a.status, pt, hits.Load(), h2.Load(), m.endsOf(), held)
			if !strings.HasPrefix(pt.String(), "h2") || hits.Load() != tc.hits || h2.Load() != tc.hits {
				t.Fatalf("connections %q, handler hits %d (h2 %d) — want an h2 connection and %d handler call(s) over h2", pt, hits.Load(), h2.Load(), tc.hits)
			}
			if held != tc.held || m.endsOf() != tc.ends {
				t.Errorf("%s: the bucket holds %d tokens (ends %s), want %d (%s)", tc.name, held, m.endsOf(), tc.held, tc.ends)
			}
		})
	}
	// A named limit, pinned (the conservative side): an HTTP/2 attempt whose
	// deadline ends while it holds a connection, before its headers are
	// written. net/http's HTTP/2 client returns on the context without
	// waiting for the stream's writer, which may still write; the meter
	// keeps. Here the request goroutine stalls in GotConn past the deadline.
	t.Run("deadline with a connection in hand, before the headers", func(t *testing.T) {
		var hits atomic.Int64
		url := serveH2(t, newHeldListener(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		rig, m := wireRig(t, noTimeouts(wireRow("h2slow", url, 10)))
		shortAttempts(rig)
		pt := &h2Trace{}
		stall := pt.trace()
		record := stall.GotConn
		stall.GotConn = func(i httptrace.GotConnInfo) {
			record(i)
			time.Sleep(400 * time.Millisecond)
		}
		a := postTraced(t, rig, stall)
		held := m.held(t, rig)
		t.Logf("h2 deadline before the headers: answer %d; connections %s; handler hits %d; ends %s; the bucket holds %d tokens",
			a.status, pt, hits.Load(), m.endsOf(), held)
		if pt.String() != "h2" || hits.Load() != 0 {
			t.Fatalf("connections %q, handler hits %d — want one h2 connection and no request", pt, hits.Load())
		}
		if held != wireReservation || m.endsOf() != "keep" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the reservation %d kept (the writer may still write)", held, m.endsOf(), wireReservation)
		}
	})
}

// TestMeterWirePanicInsideTheWireCallKeeps is gate (i) (F4-7): the first try
// wrote the request (the backend read it and died), and the request panics
// inside ChatRaw on DoRetryOnce's retry (the composed test trace panics in
// the retry's GetConn) — close() ends the reservation; it keeps its charge.
func TestMeterWirePanicInsideTheWireCallKeeps(t *testing.T) {
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		if b.read(br) {
			b.l.stop()
			reset(c)
		}
	})
	rig, m := wireRig(t, wireRow("crash", b.host(), 10))
	var gets atomic.Int64
	a := postTraced(t, rig, &httptrace.ClientTrace{GetConn: func(string) {
		if gets.Add(1) == 2 {
			panic("h3: panic inside the wire call, after the request was written")
		}
	}})
	held := m.held(t, rig)
	t.Logf("panic in the retry: answer %d; tries %d; requests read %d; ends %s; the bucket holds %d tokens",
		a.status, gets.Load(), b.requests.Load(), m.endsOf(), held)
	if gets.Load() != 2 || b.requests.Load() != 1 {
		t.Fatalf("tries %d, requests read %d — want the panic in the retry after one written request", gets.Load(), b.requests.Load())
	}
	if held != wireReservation || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a panic behind a written request, want %d kept", held, m.endsOf(), wireReservation)
	}
}

// resetFirst resets the first accepted connection (the TLS handshake fails,
// a transient error) and hands on the rest.
type resetFirst struct {
	net.Listener
	n atomic.Int64
}

func (l *resetFirst) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.n.Add(1) == 1 {
			reset(c)
			continue
		}
		return c, nil
	}
}

// TestMeterWireRetryAfterAFailedHandshakeSettles pins the retry's report:
// the first try's handshake is reset (no connection, no request byte),
// DoRetryOnce's retry reaches the backend, which answers with usage 11 + 3
// — the wire contact of the retry counts: Settle 14.
func TestMeterWireRetryAfterAFailedHandshakeSettles(t *testing.T) {
	rl := &resetFirst{Listener: newHeldListener(t)}
	var hits atomic.Int64
	url := serveH2(t, rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	}))
	rig, m := wireRig(t, wireRow("flaky", url, 10))
	a := rig.post(t, chatBody("chat"))
	held := m.held(t, rig)
	t.Logf("handshake reset, retry served: answer %d; accepted %d; handler hits %d; ends %s; the bucket holds %d tokens",
		a.status, rl.n.Load(), hits.Load(), m.endsOf(), held)
	if a.status != http.StatusOK || rl.n.Load() != 2 || hits.Load() != 1 {
		t.Fatalf("answer %d, accepted %d, hits %d — want the retry served", a.status, rl.n.Load(), hits.Load())
	}
	if held != 14 || m.endsOf() != "settle" {
		t.Errorf("the bucket holds %d tokens (ends %s), want the retry's usage 14 settled", held, m.endsOf())
	}
}

// TestMeterUsageMarkerSaturates is H3-5 (Fremd-Review 4 F4-6): the usage
// marker of the access record and of the llmlog row reads the saturated sum
// of the counters — prompt_tokens MaxInt with completion_tokens 1 is a
// reported usage, not an overflowed negative sum.
func TestMeterUsageMarkerSaturates(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Replace(answerSGLang,
			`"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3`,
			fmt.Sprintf(`"usage":{"prompt_tokens":%d,"total_tokens":1,"completion_tokens":1`, math.MaxInt), 1))
	})
	rig, _ := wireRig(t, u.row("spark", backends.TrustFull, 10))
	if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Fatalf("answer %d %s", a.status, a.body)
	}
	rec := <-rig.recs
	rows := rig.rows.all()
	if len(rows) != 1 {
		t.Fatalf("%d llmlog rows, want 1", len(rows))
	}
	t.Logf("usage MaxInt + 1: access record usage %q (prompt %d, completion %d); row usage %q", rec.Usage, rec.PromptTokens, rec.CompletionTokens, rows[0].Gateway.Usage)
	if rec.Usage != llmlog.UsageBackend || rows[0].Gateway.Usage != llmlog.UsageBackend {
		t.Errorf("access record usage %q, row usage %q — want %q on both (the saturated sum is > 0)", rec.Usage, rows[0].Gateway.Usage, llmlog.UsageBackend)
	}
}

// TestMeterWireAnswerBeforeTheRead pins the response side of the wire trace
// (Fremd-Review 5 F5-3): an HTTP/1.1 backend that answers the moment it
// accepts, before it reads a byte (an early 503 of a proxy or an overloaded
// server), and reads what comes afterwards. Two outcomes are right, and the
// timing picks one per run:
//
//   - net/http takes the answer for the request: the route answers with the
//     backend's 503. It can hand the response back before its write loop
//     reaches the first write hook, so only GotFirstResponseByte marks the
//     wire contact in time — the reservation is kept. Without that hook the
//     meter would give it back and the request would go onto the connection
//     after the decision.
//   - net/http sees the answer before it has a request on the connection and
//     drops the connection (both tries): nothing of the request was written,
//     the route answers 500 and the reservation goes back.
//
// So: the backend's answer reached the client ⇒ keep, and abort ⇒ the
// backend read no byte of the request. Without the hook the first rule
// fails once in a few hundred runs on an idle host (about one run in eight
// under load); the loop makes that certain.
func TestMeterWireAnswerBeforeTheRead(t *testing.T) {
	const runs = 2000
	const early = "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nContent-Length: 28\r\nConnection: close\r\n\r\n{\"error\":{\"message\":\"busy\"}}"
	seen := map[string]int{}
	for i := 0; i < runs; i++ {
		var got, finished, dials atomic.Int64
		b := newRawBackend(t, func(_ *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
			defer finished.Add(1)
			_, _ = io.WriteString(c, early)
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := io.Copy(io.Discard, br)
			got.Add(n)
		})
		rig, m := wireRig(t, wireRow("early", b.host(), 10))
		a := postTraced(t, rig, &httptrace.ClientTrace{ConnectDone: func(_, _ string, err error) {
			if err == nil {
				dials.Add(1)
			}
		}})
		end, held := m.endsOf(), m.held(t, rig)
		// Every connection the client opened is read to its end before the
		// backend's byte count is taken (the client closed its side).
		for deadline := time.Now().Add(5 * time.Second); finished.Load() < dials.Load(); {
			if time.Now().After(deadline) {
				t.Fatalf("run %d: %d connections dialed, %d read to the end", i, dials.Load(), finished.Load())
			}
			time.Sleep(50 * time.Microsecond)
		}
		seen[fmt.Sprintf("%d/%s/backend read %v", a.status, end, got.Load() > 0)]++
		if a.status == http.StatusServiceUnavailable && (end != "keep" || held != wireReservation) {
			t.Fatalf("run %d: the backend's answer came through (%d), ends %s, the bucket holds %d tokens — an answer is wire contact, want keep and the whole reservation %d",
				i, a.status, end, held, wireReservation)
		}
		if end == "abort" && got.Load() > 0 {
			t.Fatalf("run %d: answer %d, the reservation went back although the backend read %d bytes of the request", i, a.status, got.Load())
		}
		if end != "keep" && end != "abort" {
			t.Fatalf("run %d: answer %d, ends %q, want exactly one of keep and abort", i, a.status, end)
		}
	}
	t.Logf("answer before the read, %d runs: status/end/backend %v", runs, seen)
}
