//go:build !race

package rerankwire_test

// The wire-call term of the rerank routes (W02-10): the heap bytes one
// rerank.Score wire call allocates (runtime.MemStats TotalAlloc and
// Mallocs) — TestRerankWireCallReserveIsTheMeasuredOne is the pin of the
// booked term (rerankWireCall), the rest is the measurement it came from
// (bau report W02-10 §Wire-Term), which runs with CTX_RERANK_WIRE_MEASURE=1.
// Production build only — race instrumentation changes what allocates.
//
// The package holds tests only and lives beside internal/oai, not in it:
// internal/oai's X13 census (wirecensus_test.go) admits no wire call
// outside a WalkChain attempt in any of its files, tests included, and this
// pin calls rerank.Score directly — as FOLGE-E's wire pins live in
// internal/embed. It reads the route's term from oai's exported constants.
//
// Two fake upstreams answer the same prepared bytes:
//   - wireHTTPUpstream: an httptest.Server. Its server-side allocation runs
//     in this process and lands in TotalAlloc; it is measured separately
//     with a plain client (op Phttp) so it can be subtracted.
//   - wireRawUpstream: a raw TCP responder that reads the request head with
//     one per-connection bufio.Reader, discards the body and writes a
//     prebuilt response — next to no server allocation. Score against it
//     (op Traw) is the client-side cost directly; the plain client against
//     it (op Praw) is the bare transport. Phttp − Praw is the httptest
//     server's share S.
//
// Ops per cell: T (Score → httptest), Traw (Score → raw), Phttp/Praw (plain
// http.Client, prebuilt body, answer read into a preallocated sink), M
// (json.Marshal of the body alone), R (io.ReadAll over the 1 MiB
// LimitReader + json.Unmarshal into rerank.go's response shape alone),
// idle (the settle window with no call: the noise floor). "cold" runs two
// runtime.GC() before each measurement (empties every sync.Pool, among them
// the encoder/decoder state of encoding/json); "warm" runs the same op once
// unmeasured first.
//
// The measurement runs as CTX_RERANK_WIRE_MEASURE=1 go test -count=1 -run
// 'TestRerankWireMeasure' -v ./internal/oai/rerankwire/ (the pin always).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/rerank"
)

const (
	wireRuns = 5
	// wireLimit is rerank.go's io.LimitReader ceiling on the answer body.
	wireLimit = 1 << 20
	// wireSettle lets the server and transport goroutines finish their
	// trailing work before MemStats is read.
	wireSettle = 5 * time.Millisecond
)

// wireReq, wireResult and wireResp mirror rerank.go's rerankRequest,
// rerankResult and rerankResponse field for field (tags included).
type wireReq struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type wireResult struct {
	Index          int      `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
}

type wireResp struct {
	Results []wireResult    `json:"results"`
	Data    json.RawMessage `json:"data"`
	Usage   struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// wirePayload is one prepared answer: the body for httptest, the full
// response (head + body) for the raw responder.
type wirePayload struct {
	body []byte
	clen string
	raw  []byte
}

func newWirePayload(body []byte) *wirePayload {
	head := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nDate: Sat, 07 Nov 2026 10:00:00 GMT\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n"
	raw := make([]byte, 0, len(head)+len(body))
	raw = append(append(raw, head...), body...)
	return &wirePayload{body: body, clen: strconv.Itoa(len(body)), raw: raw}
}

type wireHTTPUpstream struct {
	srv *httptest.Server
	cur atomic.Pointer[wirePayload]
}

func newWireHTTPUpstream(t *testing.T) *wireHTTPUpstream {
	t.Helper()
	u := &wireHTTPUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p := u.cur.Load()
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", p.clen)
		_, _ = w.Write(p.body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

type wireRawUpstream struct {
	ln    net.Listener
	url   string
	cur   atomic.Pointer[wirePayload]
	mu    sync.Mutex
	conns []net.Conn
	// free holds the connections' read buffers between connections: a call
	// that refuses its answer unread closes its connection (FOLGE-R: an
	// answer past the read cap), and the next call's new connection must not
	// allocate a buffer in this process — the responder's share would land
	// in the client's TotalAlloc. Filled before any measurement.
	free []*bufio.Reader
}

// wireRawBuffers is how many read buffers the raw responder holds before a
// measurement: more than the connections a run keeps open at once (one per
// call, the one before still closing).
const wireRawBuffers = 4

func newWireRawUpstream(t *testing.T) *wireRawUpstream {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("raw upstream listen: %v", err)
	}
	u := &wireRawUpstream{ln: ln, url: "http://" + ln.Addr().String()}
	for range wireRawBuffers {
		u.free = append(u.free, bufio.NewReaderSize(nil, 64<<10))
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u.mu.Lock()
			u.conns = append(u.conns, c)
			u.mu.Unlock()
			go u.serve(c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		u.mu.Lock()
		defer u.mu.Unlock()
		for _, c := range u.conns {
			_ = c.Close()
		}
	})
	return u
}

var wireContentLength = []byte("content-length")

// serve answers keep-alive requests on c: head line by line from the
// connection's own buffer, body discarded in place, prebuilt answer written.
func (u *wireRawUpstream) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	u.mu.Lock()
	var br *bufio.Reader
	if k := len(u.free); k > 0 {
		br, u.free = u.free[k-1], u.free[:k-1]
		br.Reset(c)
	} else {
		br = bufio.NewReaderSize(c, 64<<10)
	}
	u.mu.Unlock()
	defer func() {
		br.Reset(nil)
		u.mu.Lock()
		u.free = append(u.free, br)
		u.mu.Unlock()
	}()
	for {
		cl, ok := wireReadHead(br)
		if !ok {
			return
		}
		if _, err := br.Discard(cl); err != nil {
			return
		}
		if _, err := c.Write(u.cur.Load().raw); err != nil {
			return
		}
	}
}

// wireReadHead consumes one request head and returns its Content-Length.
func wireReadHead(br *bufio.Reader) (int, bool) {
	cl := -1
	for first := true; ; first = false {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return 0, false
		}
		if first {
			continue
		}
		if len(line) <= 2 {
			return cl, cl >= 0
		}
		n := len(wireContentLength)
		if len(line) > n && line[n] == ':' && bytes.EqualFold(line[:n], wireContentLength) {
			cl = 0
			for _, ch := range bytes.TrimSpace(line[n+1:]) {
				cl = cl*10 + int(ch-'0')
			}
		}
	}
}

type wireCase struct {
	name    string
	n, size int
	docs    []string
	body    []byte // exactly json.Marshal(rerankRequest{...}) — B
	resp    []byte
	hostile bool
}

func (c *wireCase) d() int {
	s := 0
	for _, d := range c.docs {
		s += len(d)
	}
	return s
}

type wireStat struct {
	minB, maxB, minM, maxM uint64
	set                    bool
}

func (s *wireStat) add(b, m uint64) {
	if !s.set {
		*s = wireStat{minB: b, maxB: b, minM: m, maxM: m, set: true}
		return
	}
	s.minB, s.maxB = min(s.minB, b), max(s.maxB, b)
	s.minM, s.maxM = min(s.minM, m), max(s.maxM, m)
}

var wireOps = []string{"T", "Traw", "Phttp", "Praw", "M", "R", "idle"}

type wireHarness struct {
	t      *testing.T
	ctx    context.Context
	httpUp *wireHTTPUpstream
	rawUp  *wireRawUpstream
	bHTTP  backends.Backend
	bRaw   backends.Backend
	plain  *http.Client
	sink   []byte
	query  string
}

func (h *wireHarness) op(name string, c *wireCase) error {
	switch name {
	case "T":
		_, _, err := rerank.Score(h.ctx, h.bHTTP, "M", h.query, c.docs, rerank.DomainAuto)
		return err
	case "Traw":
		_, _, err := rerank.Score(h.ctx, h.bRaw, "M", h.query, c.docs, rerank.DomainAuto)
		return err
	case "Phttp":
		return wirePlainCall(h.ctx, h.plain, h.httpUp.srv.URL, c.body, h.sink)
	case "Praw":
		return wirePlainCall(h.ctx, h.plain, h.rawUp.url, c.body, h.sink)
	case "M":
		_, err := json.Marshal(wireReq{Model: "M", Query: h.query, Documents: c.docs})
		return err
	case "R":
		return wireDecode(c.resp)
	}
	return nil
}

// wirePlainCall is the bare round trip: prebuilt body, answer read into a
// preallocated sink.
func wirePlainCall(ctx context.Context, c *http.Client, url string, body, sink []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	for {
		_, err := resp.Body.Read(sink)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// wireDecode is rerank.Score's answer decode alone: the buffered read under
// the 1 MiB ceiling, the response decode, and resolveEntries' second decode
// of a "data" container.
func wireDecode(resp []byte) error {
	raw, err := io.ReadAll(io.LimitReader(bytes.NewReader(resp), wireLimit))
	if err != nil {
		return err
	}
	var r wireResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if len(r.Results) == 0 && len(r.Data) > 0 {
		var entries []wireResult
		return json.Unmarshal(r.Data, &entries)
	}
	return nil
}

func (h *wireHarness) measureOnce(name string, c *wireCase, warm bool) (uint64, uint64, error) {
	if warm {
		_ = h.op(name, c)
	} else {
		runtime.GC()
		runtime.GC()
	}
	time.Sleep(wireSettle)
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	err := h.op(name, c)
	time.Sleep(wireSettle)
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc, b.Mallocs - a.Mallocs, err
}

// run measures every op of c wireRuns times per mode, interleaved run by
// run so host-load drift spreads over all ops alike.
func (h *wireHarness) run(c *wireCase) [2]map[string]*wireStat {
	h.httpUp.cur.Store(newWirePayload(c.resp))
	h.rawUp.cur.Store(newWirePayload(c.resp))
	var out [2]map[string]*wireStat
	errs := map[string]string{}
	for mode := range 2 {
		out[mode] = map[string]*wireStat{}
		for _, op := range wireOps {
			out[mode][op] = &wireStat{}
		}
		for range wireRuns {
			for _, op := range wireOps {
				b, m, err := h.measureOnce(op, c, mode == 1)
				if err != nil {
					hostileOp := c.hostile && (op == "T" || op == "Traw" || op == "R")
					if !hostileOp {
						h.t.Fatalf("%s op %s: %v", c.name, op, err)
					}
					if _, seen := errs[op]; !seen {
						errs[op] = err.Error()
					}
				}
				out[mode][op].add(b, m)
			}
		}
	}
	for _, op := range wireOps {
		if e, ok := errs[op]; ok {
			h.t.Logf("W0210 wire: %s op=%s error (expected, hostile): %.160s", c.name, op, e)
		}
	}
	return out
}

func wireText(size int) string {
	const pat = "the quick brown fox jumps over the lazy dog 0123456789 "
	return strings.Repeat(pat, size/len(pat)+1)[:size]
}

// wireLlamaAnswer is llama.cpp's rerank answer for n documents: results
// sorted descending by score, scores float32 logits from 9.75 down to
// -3.25 rendered at float64 precision, indices a permutation of [0,n).
func wireLlamaAnswer(n, promptTokens int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"model":"m","object":"list","usage":{"prompt_tokens":%d,"total_tokens":%d},"results":[`, promptTokens, promptTokens)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		s := float64(float32(9.75 - 13*float64(i)/float64(max(n-1, 1))))
		fmt.Fprintf(&b, `{"index":%d,"relevance_score":%s}`, (i*7919)%n, strconv.FormatFloat(s, 'g', -1, 64))
	}
	b.WriteString("]}")
	return b.Bytes()
}

// wireHostileAnswer fills the 1 MiB answer ceiling (one byte short, so the
// body is read to EOF and the connection stays reusable) with elem repeated
// inside prefix…suffix.
func wireHostileAnswer(prefix, elem, suffix string) []byte {
	room := wireLimit - 1 - len(prefix) - len(suffix)
	count := (room + 1) / (len(elem) + 1)
	var b bytes.Buffer
	b.Grow(wireLimit)
	b.WriteString(prefix)
	for i := range count {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(elem)
	}
	b.WriteString(suffix)
	return b.Bytes()
}

func newWireCase(t *testing.T, query, name string, n, size int, doc string, resp []byte, hostile bool) *wireCase {
	t.Helper()
	docs := make([]string, n)
	for i := range docs {
		docs[i] = doc
	}
	body, err := json.Marshal(wireReq{Model: "M", Query: query, Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		resp = wireLlamaAnswer(n, (n*size)/4+n*16)
	}
	return &wireCase{name: name, n: n, size: size, docs: docs, body: body, resp: resp, hostile: hostile}
}

// newWireHarness builds both upstreams and the plain client, and runs every
// op once on a small case: the keep-alive connections and the per-type json
// caches exist before any measurement.
func newWireHarness(t *testing.T) *wireHarness {
	t.Helper()
	identity := fedhop.NewIdentity("0193e0b5-7c1d-7000-8000-00000000a001", bytes.Repeat([]byte{7}, 32))
	h := &wireHarness{
		t:      t,
		ctx:    fedhop.WithIdentity(context.Background(), identity),
		httpUp: newWireHTTPUpstream(t),
		rawUp:  newWireRawUpstream(t),
		plain:  httpx.PooledClient(),
		sink:   make([]byte, 64<<10),
		query:  wireText(64),
	}
	t.Cleanup(h.plain.CloseIdleConnections)
	h.bHTTP = backends.Backend{Host: h.httpUp.srv.URL, Protocol: backends.ProtocolOpenAI, Name: "r", Model: "M", Enabled: true}
	h.bRaw = backends.Backend{Host: h.rawUp.url, Protocol: backends.ProtocolOpenAI, Name: "r", Model: "M", Enabled: true}
	if h.bHTTP.EgressGuarded() || h.bRaw.EgressGuarded() {
		t.Fatal("operator rows must use the plain pool (no ssrfguard on loopback)")
	}
	warm := newWireCase(t, h.query, "warm-up", 1, 64, wireText(64), nil, false)
	h.httpUp.cur.Store(newWirePayload(warm.resp))
	h.rawUp.cur.Store(newWirePayload(warm.resp))
	for _, op := range wireOps {
		if err := h.op(op, warm); err != nil {
			t.Fatalf("warm-up %s: %v", op, err)
		}
	}
	return h
}

func wireHostileCases(t *testing.T, query string) []*wireCase {
	t.Helper()
	var cases []*wireCase
	for _, hc := range []struct{ name, prefix, elem, suffix string }{
		{"hostile-llama", `{"results":[`, `{"index":0,"relevance_score":0}`, `]}`},
		{"hostile-empty-obj", `{"results":[`, `{}`, `]}`},
		{"hostile-data-obj", `{"data":[`, `{}`, `]}`},
		{"hostile-number", `{"results":[`, `0`, `]}`},
		{"hostile-array", `{"results":[`, `[]`, `]}`},
	} {
		cases = append(cases, newWireCase(t, query, hc.name, 1, 64, wireText(64), wireHostileAnswer(hc.prefix, hc.elem, hc.suffix), true))
	}
	return cases
}

// wireMeasure skips a measurement without CTX_RERANK_WIRE_MEASURE=1: the
// matrix allocates gigabytes and runs minutes, the pin below is the gate.
func wireMeasure(t *testing.T) {
	t.Helper()
	if os.Getenv("CTX_RERANK_WIRE_MEASURE") != "1" {
		t.Skip("measurement, not a gate: set CTX_RERANK_WIRE_MEASURE=1")
	}
}

// TestRerankWireCallReserveIsTheMeasuredOne is the pin of the wire-call
// term: for every form of the matrix — 1, 128 and 1000 documents of 64 B,
// 4 KiB and 64 KiB (1000 × 64 KiB outside -short), documents of "<" and of
// control characters that json.Marshal writes six times as long — the body
// what the call allocates on the client side (Score against an upstream that allocates
// next to nothing, pools cold, the largest of three) is at most what the
// route books for it (rerankWireCall, the retry's share and the answer's
// room for an echo left out: no call here is retried, no answer here
// echoes — TestFolgeRAnswerReadIsBooked books the echo) and, at the largest
// form, more than 85 % of it — the term is neither short nor loose.
func TestRerankWireCallReserveIsTheMeasuredOne(t *testing.T) {
	h := newWireHarness(t)
	type cell struct {
		name    string
		n, size int
		doc     string
	}
	var cells []cell
	for _, n := range []int{1, 128, 1000} {
		for _, size := range []int{64, 4 << 10, 64 << 10} {
			if n == 1000 && size == 64<<10 && testing.Short() {
				continue
			}
			cells = append(cells, cell{fmt.Sprintf("n%d/%dB", n, size), n, size, wireText(size)})
		}
	}
	cells = append(cells,
		cell{"n128/4KiB-lt", 128, 4 << 10, strings.Repeat("<", 4<<10)},
		cell{"n128/4KiB-ctl", 128, 4 << 10, strings.Repeat("\x01", 4<<10)},
	)
	best := 0.0
	for _, cl := range cells {
		c := newWireCase(t, h.query, cl.name, cl.n, cl.size, cl.doc, nil, false)
		h.rawUp.cur.Store(newWirePayload(c.resp))
		var most uint64
		for range 3 {
			b, _, err := h.measureOnce("Traw", c, false)
			if err != nil {
				t.Fatalf("%s: %v", cl.name, err)
			}
			most = max(most, b)
		}
		// The body the route books: the wire's own length (rerank.RequestBodyLen,
		// FOLGE-R — c.body is json.Marshal's, which the wire no longer sends).
		// The retry's share and the answer's room for an echo left out: no call
		// here is retried, no answer here echoes.
		booked := wireCallBooking(rerank.RequestBodyLen("M", h.query, c.docs), cl.n)
		ratio := float64(most) / float64(booked)
		best = max(best, ratio)
		t.Logf("W0210 wire pin: %-14s B=%d allocated=%d booked=%d (%.3f)", cl.name, len(c.body), most, booked, ratio)
		if int64(most) > booked {
			t.Errorf("%s: a wire call allocates %d B, the route books %d B", cl.name, most, booked)
		}
	}
	if best <= 0.85 {
		t.Errorf("the largest form uses %.3f of its booking, want more than 0.85 — the term books for nothing", best)
	}
}

// wireCallBooking is the route's term for one call of a body of body bytes
// over docs documents without the retry's share and without the answer's
// room for an echo (rerankWireCall in internal/oai/rerank_wire.go, from its
// exported constants): the body's build and its size class, the
// transport's copy, the documents' answer and the call.
func wireCallBooking(body, docs int) int64 {
	b := int64(body)
	return (oai.RerankWireBodyNum*b+oai.RerankWireBodyDen-1)/oai.RerankWireBodyDen + oai.RerankWireBodySlack +
		min(b, oai.RerankWireCopyReserve) + int64(docs)*oai.RerankWireDocReserve + oai.RerankWireCallReserve
}

func TestRerankWireMeasure(t *testing.T) {
	wireMeasure(t)
	h := newWireHarness(t)
	query := h.query

	var cases []*wireCase
	for _, n := range []int{1, 128, 1000} {
		for _, size := range []int{64, 4 << 10, 64 << 10} {
			cases = append(cases, newWireCase(t, query, fmt.Sprintf("n%d/%dB", n, size), n, size, wireText(size), nil, false))
		}
	}
	cases = append(cases,
		newWireCase(t, query, "n128/4KiB-lt", 128, 4<<10, strings.Repeat("<", 4<<10), nil, false),
		newWireCase(t, query, "n128/4KiB-ctl", 128, 4<<10, strings.Repeat("\x01", 4<<10), nil, false),
	)
	cases = append(cases, wireHostileCases(t, query)...)

	t.Logf("W0210 wire: GOMAXPROCS=%d runs=%d settle=%s go=%s", runtime.GOMAXPROCS(0), wireRuns, wireSettle, runtime.Version())
	t.Logf("W0210 wire: csv,cell,mode,n,size,D,B,A,Tmin,Tmax,TmallocMin,Trawmin,Trawmax,Phttpmin,Prawmin,Mmin,Mmax,Rmin,Rmax,idleMax,S,TmaxMinusS,ratioD,ratioB")
	for _, c := range cases {
		start := time.Now()
		st := h.run(c)
		for mode, name := range []string{"cold", "warm"} {
			wireReport(t, c, name, st[mode])
		}
		t.Logf("W0210 wire: %s wall=%s", c.name, time.Since(start).Round(time.Millisecond))
	}
}

func wireReport(t *testing.T, c *wireCase, mode string, st map[string]*wireStat) {
	t.Helper()
	for _, op := range wireOps {
		s := st[op]
		t.Logf("W0210 wire: op %-22s %s %-5s bytes=%d..%d mallocs=%d..%d", c.name, mode, op, s.minB, s.maxB, s.minM, s.maxM)
	}
	d, b, a := c.d(), len(c.body), len(c.resp)
	s := int64(st["Phttp"].minB) - int64(st["Praw"].minB)
	tms := int64(st["T"].maxB) - s
	t.Logf("W0210 wire: csv,%s,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%.3f,%.3f",
		c.name, mode, c.n, c.size, d, b, a,
		st["T"].minB, st["T"].maxB, st["T"].minM,
		st["Traw"].minB, st["Traw"].maxB,
		st["Phttp"].minB, st["Praw"].minB,
		st["M"].minB, st["M"].maxB, st["R"].minB, st["R"].maxB,
		st["idle"].maxB, s, tms,
		float64(tms)/float64(max(d, 1)), float64(tms)/float64(b))
}

// TestRerankWireMeasureSweep walks B in small steps between the matrix
// points: the body buffer grows by append's factor, so bytes/B is a saw
// tooth whose peak the three matrix sizes need not hit. Cold only, client
// side only (Traw — the raw responder allocates next to nothing), max of
// two runs per point.
func TestRerankWireMeasureSweep(t *testing.T) {
	wireMeasure(t)
	h := newWireHarness(t)
	t.Logf("W0210 wire: sweep,n,size,B,A,TrawMax,Mmax,Traw/B,M/B")
	for _, sw := range []struct{ n, from, to, step int }{
		{128, 4096, 8192, 64},
		{1000, 4096, 8192, 256},
	} {
		var maxT, maxM float64
		minT, minM := 1e9, 1e9
		var maxTAt, maxMAt int
		for size := sw.from; size <= sw.to; size += sw.step {
			c := newWireCase(t, h.query, "sweep", sw.n, size, wireText(size), nil, false)
			h.rawUp.cur.Store(newWirePayload(c.resp))
			var tb, mb uint64
			for range 2 {
				b, _, err := h.measureOnce("Traw", c, false)
				if err != nil {
					t.Fatalf("sweep n=%d size=%d: %v", sw.n, size, err)
				}
				tb = max(tb, b)
				b, _, _ = h.measureOnce("M", c, false)
				mb = max(mb, b)
			}
			bl := float64(len(c.body))
			rt, rm := float64(tb)/bl, float64(mb)/bl
			if rt > maxT {
				maxT, maxTAt = rt, len(c.body)
			}
			if rm > maxM {
				maxM, maxMAt = rm, len(c.body)
			}
			minT, minM = min(minT, rt), min(minM, rm)
			t.Logf("W0210 wire: sweep,%d,%d,%d,%d,%d,%d,%.3f,%.3f", sw.n, size, len(c.body), len(c.resp), tb, mb, rt, rm)
		}
		t.Logf("W0210 wire: sweep-summary n=%d Traw/B=%.3f..%.3f (max at B=%d) M/B=%.3f..%.3f (max at B=%d)",
			sw.n, minT, maxT, maxTAt, minM, maxM, maxMAt)
	}
}

// wirePeak estimates the live-heap peak an op adds: GC percent 5 keeps
// garbage near zero while a sampler polls the heap-objects metric; the
// result is max(sample) minus the post-GC baseline. An estimate (sampling
// can miss a transient), not an exact peak.
func wirePeak(h *wireHarness, op string, c *wireCase) uint64 {
	old := debug.SetGCPercent(5)
	defer debug.SetGCPercent(old)
	runtime.GC()
	runtime.GC()
	const name = "/memory/classes/heap/objects:bytes"
	base := []metrics.Sample{{Name: name}}
	metrics.Read(base)
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s := []metrics.Sample{{Name: name}}
		for {
			metrics.Read(s)
			if v := s[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	_ = h.op(op, c)
	close(stop)
	<-done
	return peak.Load() - min(peak.Load(), base[0].Value.Uint64())
}

// TestRerankWireMeasurePeak puts the TotalAlloc figures next to the live
// peak they imply: Score against the raw responder (client side only),
// max of three runs.
func TestRerankWireMeasurePeak(t *testing.T) {
	wireMeasure(t)
	h := newWireHarness(t)
	cases := []*wireCase{
		newWireCase(t, h.query, "n1000/65536B", 1000, 64<<10, wireText(64<<10), nil, false),
		newWireCase(t, h.query, "n1000/4096B", 1000, 4<<10, wireText(4<<10), nil, false),
		newWireCase(t, h.query, "n128/4KiB-lt", 128, 4<<10, strings.Repeat("<", 4<<10), nil, false),
	}
	cases = append(cases, wireHostileCases(t, h.query)...)
	for _, c := range cases {
		h.rawUp.cur.Store(newWirePayload(c.resp))
		var p uint64
		for range 3 {
			p = max(p, wirePeak(h, "Traw", c))
		}
		t.Logf("W0210 wire: peak %-18s B=%d A=%d livePeak(Traw)=%d peak/B=%.3f", c.name, len(c.body), len(c.resp), p, float64(p)/float64(len(c.body)))
	}
}
