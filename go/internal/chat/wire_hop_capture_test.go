package chat

// F-W4 — the dynamic stage of the hop-stamp census (design/04 §4.3 "Zensus,
// zweistufig"). stampPin (dispatch_invariant_test.go) counts per wire file
// the stamp and Observe calls in the source; this file proves on the wire
// what the source claims, generated from wirePin: every pinned wire file needs
// a driver in hopDrivers, the driver fires each of the file's sites once
// against a capturing httptest server, and the number of captured requests
// must equal the file's wirePin count. Red: a wire file without a driver, a
// site that sends no Ctx-Hop to a tier ≠ None target or sends one to
// openrouter, a site that lets a row's Ctx-*/X-Ctx-* header through, and a
// site that does not hand the answer to fedhop.Observe — including the sites
// stamped indirectly through setChatHeaders (stream.go, passthrough.go).
//
//	go test -count=1 -run 'TestWireHop' ./internal/chat/

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rerank"
)

// hopDriver fires every wire site of one pinned file exactly once against b
// (b.Host is the capture server) and returns each site's error. sites names
// the requests the capture must see, in order, as hopSiteKey renders them —
// one per wire site, so a driver that reaches one site twice and another not
// at all is red although the count matches.
type hopDriver struct {
	sites []string
	drive func(ctx context.Context, b backends.Backend) []error
}

// hopSiteKey names a captured request: its path, plus " stream" when it asks
// for an event stream (the two passthrough sites share one path).
func hopSiteKey(c hopCapture) string {
	if c.Header.Get("Accept") == "text/event-stream" {
		return c.Path + " stream"
	}
	return c.Path
}

var hopMsgs = []llm.ChatMsg{{Role: "user", Content: "hi"}}

func hopNoEvent(llm.StreamEvent) error { return nil }

// hopDrivers is the driver table of the capture census, one entry per wirePin
// file. Each driver reaches the sites through their exported entry points,
// one call per site, protocol set per site.
var hopDrivers = map[string]hopDriver{
	"llm/client.go": {[]string{"/api/chat", "/v1/chat/completions"}, func(ctx context.Context, b backends.Backend) (errs []error) {
		for _, p := range []backends.Protocol{backends.ProtocolOllama, backends.ProtocolOpenAI} {
			b.Protocol = p
			_, err := llm.Chat(ctx, b, "s", "u", llm.Options{}, time.Second)
			errs = append(errs, err)
		}
		return errs
	}},
	"llm/stream.go": {[]string{"/v1/chat/completions stream"}, func(ctx context.Context, b backends.Backend) []error {
		b.Protocol = backends.ProtocolOpenAI
		_, err := llm.ChatStream(ctx, b, "m", hopMsgs, nil, llm.Options{}, nil, time.Second, hopNoEvent)
		return []error{err}
	}},
	"llm/anthropic.go": {[]string{"/v1/messages"}, func(ctx context.Context, b backends.Backend) []error {
		b.Protocol = backends.ProtocolAnthropic
		_, err := llm.Chat(ctx, b, "s", "u", llm.Options{}, time.Second)
		return []error{err}
	}},
	"llm/anthropic_stream.go": {[]string{"/v1/messages stream"}, func(ctx context.Context, b backends.Backend) []error {
		b.Protocol = backends.ProtocolAnthropic
		_, err := llm.ChatStreamBackend(ctx, b, "m", hopMsgs, nil, llm.Options{}, time.Second, hopNoEvent)
		return []error{err}
	}},
	"embed/embed.go": {[]string{"/api/embed", "/v1/embeddings"}, func(ctx context.Context, b backends.Backend) (errs []error) {
		for _, p := range []backends.Protocol{backends.ProtocolOllama, backends.ProtocolOpenAI} {
			b.Protocol = p
			_, _, err := embed.Embed(ctx, b, "x", embed.PrefixQuery)
			errs = append(errs, err)
		}
		return errs
	}},
	"rerank/rerank.go": {[]string{"/v1/rerank"}, func(ctx context.Context, b backends.Backend) []error {
		_, _, err := rerank.Score(ctx, b, "m", "q", []string{"a", "b", "c"}, b.ScoreDomain())
		return []error{err}
	}},
	"llm/passthrough.go": {[]string{"/v1/chat/completions", "/v1/chat/completions stream"}, func(ctx context.Context, b backends.Backend) (errs []error) {
		b.Protocol = backends.ProtocolOpenAI
		for _, stream := range []bool{false, true} {
			body, err := llm.BuildUpstreamBody(llm.UpstreamSpec{
				Messages: json.RawMessage(`[{"role":"user","content":"hi"}]`), Role: "chat", Stream: stream, MaxTokens: 16,
			}, &b)
			if err == nil && stream {
				_, err = llm.ChatRawStream(ctx, b, body, time.Second, 0, func([]byte) error { return nil })
			} else if err == nil {
				_, err = llm.ChatRaw(ctx, b, body, time.Second, httpx.MaxResponseBytes)
			}
			errs = append(errs, err)
		}
		return errs
	}},
}

// hopCapture is one request the capture server saw.
type hopCapture struct {
	Path   string
	Header http.Header
}

// hopAnswer, when set, answers a captured request itself (true) instead of
// the wire shape; it may also only add response headers and return false.
type hopAnswer func(w http.ResponseWriter, r *http.Request) bool

// captureHops runs the driver of file against b on a fresh capture server
// and checks the census count: one captured request per wire site.
func captureHops(t *testing.T, ctx context.Context, file string, b backends.Backend, answer hopAnswer) ([]hopCapture, []error) {
	t.Helper()
	drv, ok := hopDrivers[file]
	if !ok {
		t.Fatalf("no capture driver for wire file %s: every wirePin file needs a hopDrivers entry that fires each of its sites once (design/04 §4.3)", file)
	}
	var (
		mu  sync.Mutex
		got []hopCapture
	)
	vec := wireVec1024()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, hopCapture{Path: r.URL.Path, Header: r.Header.Clone()})
		mu.Unlock()
		if answer != nil && answer(w, r) {
			return
		}
		serveWireShape(t, w, r, body, vec)
	}))
	t.Cleanup(srv.Close)
	b.Host = srv.URL
	errs := drv.drive(ctx, b)
	srv.Close() // waits for every handler: got is complete
	mu.Lock()
	defer mu.Unlock()
	if len(got) != wirePin[file] || len(drv.sites) != wirePin[file] {
		t.Errorf("%s: %d request(s) captured, %d site(s) in the driver table, wirePin says %d — the driver fires every site of the file once", file, len(got), len(drv.sites), wirePin[file])
	}
	seen := make([]string, len(got))
	for i, c := range got {
		seen[i] = hopSiteKey(c)
	}
	if !slices.Equal(seen, drv.sites) {
		t.Errorf("%s: captured requests %q, the driver table says %q — every site once, none twice", file, seen, drv.sites)
	}
	return got, errs
}

// pinnedWireFiles returns the wirePin keys in a stable order.
func pinnedWireFiles() []string {
	files := make([]string, 0, len(wirePin))
	for f := range wirePin {
		files = append(files, f)
	}
	slices.Sort(files)
	return files
}

// hopTestIdentity is the instance the capture tests stamp with.
func hopTestIdentity() fedhop.Identity {
	return fedhop.NewIdentity("7f2c9a4e-1b3d-4c5e-8f6a-0b1c2d3e4f5a", bytes.Repeat([]byte{0x5a}, 32))
}

// hopRow is a chat/embed/rerank-capable row of the given provider class with
// a key, so the core Authorization is set at every site.
func hopRow(class string) backends.Backend {
	b := invariantBackend("", backends.ProtocolOpenAI)
	b.ID = "row-" + class
	b.Name = "row-" + class
	b.ProviderClass = class
	b.APIKey = "core-key"
	return b
}

// federationHeaders lists every header of h named Ctx-… or X-Ctx-… (any case).
func federationHeaders(h http.Header) []string {
	var out []string
	for k := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "ctx-") || strings.HasPrefix(lk, "x-ctx-") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// checkSiteErrors fails on any site error of a driver run.
func checkSiteErrors(t *testing.T, where string, errs []error) {
	t.Helper()
	for i, err := range errs {
		if err != nil {
			t.Errorf("%s site %d: %v", where, i+1, err)
		}
	}
}

// TestWireHopStampEveryPinnedSite — F-W4 gates (b) and (c): every wire site
// of every pinned file sends exactly one Ctx-Hop line with exactly one token
// of this instance to every target but openrouter (generic, llamacpp, an
// empty and an unknown provider class — tier L, Finding 6), no other
// federation header, and no header value carrying the instance id; and no two
// requests of the instance share a token.
func TestWireHopStampEveryPinnedSite(t *testing.T) {
	id := hopTestIdentity()
	ctx := fedhop.WithIdentity(context.Background(), id)
	seen := map[string]string{}
	for _, class := range []string{backends.ProviderGeneric, backends.ProviderLlamaCpp, "", "unregistered-class"} {
		for _, file := range pinnedWireFiles() {
			got, errs := captureHops(t, ctx, file, hopRow(class), nil)
			checkSiteErrors(t, file, errs)
			for i, c := range got {
				where := fmt.Sprintf("%s request %d (%s, provider_class %q)", file, i+1, c.Path, class)
				hop := c.Header.Values(fedhop.HeaderHop)
				if len(hop) != 1 || strings.Contains(hop[0], ",") || !id.IsOwnToken(hop[0]) {
					t.Errorf("%s: Ctx-Hop %q, want exactly one token of this instance", where, hop)
					continue
				}
				if prev, dup := seen[hop[0]]; dup {
					t.Errorf("%s: token %s already sent by %s — two requests of one instance must carry unlinkable tokens", where, hop[0], prev)
				}
				seen[hop[0]] = where
				if fh := federationHeaders(c.Header); !slices.Equal(fh, []string{fedhop.HeaderHop}) {
					t.Errorf("%s: federation headers %v, want only Ctx-Hop on tier L", where, fh)
				}
				for k, vs := range c.Header {
					for _, v := range vs {
						if strings.Contains(v, id.ID) {
							t.Errorf("%s: header %s carries the instance id (%q)", where, k, v)
						}
					}
				}
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no token captured at all")
	}
}

// TestWireHopStampAppendsInboundChain — F-W4 gate (b), second clause: with an
// inbound chain in the context (fedhop.WithInbound, the ingress's job) every
// site sends the chain plus one fresh own token at its end.
func TestWireHopStampAppendsInboundChain(t *testing.T) {
	id := hopTestIdentity()
	peer := fedhop.NewIdentity("3b0a6c1d-2e4f-4a5b-9c6d-7e8f9a0b1c2d", bytes.Repeat([]byte{0xa5}, 32))
	first, second := peer.MintToken(), peer.MintToken()
	in, err := fedhop.ParseInbound(http.Header{fedhop.HeaderHop: {first + ", " + second}})
	if err != nil {
		t.Fatalf("inbound chain: %v", err)
	}
	ctx := fedhop.WithInbound(fedhop.WithIdentity(context.Background(), id), in)
	for _, file := range pinnedWireFiles() {
		got, errs := captureHops(t, ctx, file, hopRow(backends.ProviderLlamaCpp), nil)
		checkSiteErrors(t, file, errs)
		for i, c := range got {
			out, err := fedhop.ParseInbound(http.Header{fedhop.HeaderHop: c.Header.Values(fedhop.HeaderHop)})
			if err != nil || len(out.Hop) != 3 || out.Hop[0] != first || out.Hop[1] != second || !id.IsOwnToken(out.Hop[2]) {
				t.Errorf("%s request %d (%s): Ctx-Hop %q (err %v), want the inbound chain plus one own token", file, i+1, c.Path, c.Header.Values(fedhop.HeaderHop), err)
			}
		}
	}
}

// rowForgedHeaders are extra_headers a row can only carry past the API
// denylist (psql): federation headers in several spellings. Stamp step 1
// removes them at every site.
var rowForgedHeaders = map[string]string{
	"Ctx-Hop":              "forged",
	"X-Ctx-Dispatch-Class": "interactive",
	"ctx-path":             "forged",
}

// TestWireHopRowCannotSetOrForgeMarkers — F-W4 gate (b), openrouter clause,
// and design/04 §5.2 S4 on the wire: a row's Ctx-*/X-Ctx-* extra_headers never
// reach a backend. An openrouter row (tier None) sends no federation header at
// all — not the row's, not the inbound chain, no own token; a generic row
// sends its own fresh token in place of the row's.
func TestWireHopRowCannotSetOrForgeMarkers(t *testing.T) {
	id := hopTestIdentity()
	in, err := fedhop.ParseInbound(http.Header{fedhop.HeaderHop: {id.MintToken()}})
	if err != nil {
		t.Fatalf("inbound chain: %v", err)
	}
	ctx := fedhop.WithInbound(fedhop.WithIdentity(context.Background(), id), in)
	for _, class := range []string{backends.ProviderOpenRouter, backends.ProviderGeneric} {
		for _, file := range pinnedWireFiles() {
			row := hopRow(class)
			row.ExtraHeaders = rowForgedHeaders
			got, errs := captureHops(t, ctx, file, row, nil)
			checkSiteErrors(t, file, errs)
			for i, c := range got {
				where := fmt.Sprintf("%s request %d (%s, provider_class %q)", file, i+1, c.Path, class)
				fh := federationHeaders(c.Header)
				if class == backends.ProviderOpenRouter {
					if len(fh) != 0 {
						t.Errorf("%s: sends %v to openrouter — tier None carries no federation header", where, fh)
					}
					continue
				}
				hop := c.Header.Values(fedhop.HeaderHop)
				if !slices.Equal(fh, []string{fedhop.HeaderHop}) || len(hop) != 1 || strings.Contains(hop[0], "forged") {
					t.Errorf("%s: federation headers %v, Ctx-Hop %q — the row's headers must be gone", where, fh, hop)
				}
			}
		}
	}
}

// hopObserver counts what fedhop.Observe reports, per backend id.
type hopObserver struct {
	mu        sync.Mutex
	peers     map[string]int
	selfLoops map[string]int
	next      fedhop.Observer
}

func newHopObserver(next fedhop.Observer) *hopObserver {
	return &hopObserver{peers: map[string]int{}, selfLoops: map[string]int{}, next: next}
}

func (o *hopObserver) NotePeerInstance(backendID, instance string) {
	o.mu.Lock()
	o.peers[backendID]++
	o.mu.Unlock()
	if o.next != nil {
		o.next.NotePeerInstance(backendID, instance)
	}
}

func (o *hopObserver) MarkSelfLoop(backendID string) {
	o.mu.Lock()
	o.selfLoops[backendID]++
	o.mu.Unlock()
	if o.next != nil {
		o.next.MarkSelfLoop(backendID)
	}
}

func (o *hopObserver) counts(backendID string) (peers, selfLoops int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.peers[backendID], o.selfLoops[backendID]
}

// TestWireHopObserveEveryTier — F-W4 gate (e) on the wire: every site hands
// the answer headers to fedhop.Observe on every tier, openrouter (None)
// included, the pool turns a row answering with Ctx-Instance into a status
// hint, and Observe runs before the status check — a 508 that echoes the own token in
// Ctx-Loop-Token reaches MarkSelfLoop at every site although the site then
// fails.
func TestWireHopObserveEveryTier(t *testing.T) {
	id := hopTestIdentity()
	const peerInstance = "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
	peerAnswer := func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set(fedhop.HeaderInstance, peerInstance)
		return false
	}
	loopAnswer := func(w http.ResponseWriter, r *http.Request) bool {
		hop := r.Header.Values(fedhop.HeaderHop)
		if len(hop) > 0 {
			chain := strings.Split(hop[len(hop)-1], ",")
			w.Header().Set(fedhop.HeaderLoopToken, strings.TrimSpace(chain[len(chain)-1]))
		}
		w.WriteHeader(http.StatusLoopDetected)
		return true
	}
	for _, class := range []string{backends.ProviderOpenRouter, backends.ProviderGeneric, backends.ProviderLlamaCpp} {
		for _, file := range pinnedWireFiles() {
			row := hopRow(class)
			pool := backends.NewPool(nil, nil)
			pool.SeedSnapshotForTest([]backends.Backend{row})
			obs := newHopObserver(pool)
			ctx := fedhop.WithObserver(fedhop.WithIdentity(context.Background(), id), obs)
			_, errs := captureHops(t, ctx, file, row, peerAnswer)
			checkSiteErrors(t, file, errs)
			if peers, _ := obs.counts(row.ID); peers != wirePin[file] {
				t.Errorf("%s (provider_class %q): Observe noted Ctx-Instance %d time(s), want %d — a site does not observe its answer", file, class, peers, wirePin[file])
			}
			// The pool is the observer of the boot (fedhop.SetDefaultObserver):
			// the row answering as a ctx instance shows as a status hint.
			if st := pool.Status(); len(st) != 1 || !slices.Equal(st[0].Hints, []string{backends.HintAnswersAsCtxInstance}) {
				t.Errorf("%s (provider_class %q): Pool.Status() = %+v, want the hint %s on the row", file, class, st, backends.HintAnswersAsCtxInstance)
			}
			if class == backends.ProviderOpenRouter {
				continue // tier None sends no token, so there is nothing to echo
			}
			obs = newHopObserver(nil)
			ctx = fedhop.WithObserver(fedhop.WithIdentity(context.Background(), id), obs)
			_, errs = captureHops(t, ctx, file, row, loopAnswer)
			for i, err := range errs {
				var se *httpx.StatusError
				if !errors.As(err, &se) || se.Code != http.StatusLoopDetected {
					t.Errorf("%s site %d: error %v, want the 508 as a StatusError", file, i+1, err)
				}
			}
			if _, loops := obs.counts(row.ID); loops != wirePin[file] {
				t.Errorf("%s (provider_class %q): 508 with the own Ctx-Loop-Token reached MarkSelfLoop %d time(s), want %d — Observe runs before the status check", file, class, loops, wirePin[file])
			}
		}
	}
}
