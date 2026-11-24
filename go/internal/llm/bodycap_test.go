package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

// repeatReader yields s n times without materializing the whole payload —
// the oversized bodies below never exist in test memory either.
func repeatReader(s string, n int) io.Reader {
	return io.LimitReader(&cycle{s: s}, int64(len(s))*int64(n))
}

type cycle struct {
	s   string
	off int
}

func (c *cycle) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		k := copy(p[n:], c.s[c.off:])
		n += k
		c.off = (c.off + k) % len(c.s)
	}
	return n, nil
}

const okFrame = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// capProofTimeout is how long the body-cap proofs wait for a Chat call or a
// parser. It is a hang guard only — each proof judges by what the call
// returns (ErrResponseTooLarge, the lines a parser read), and a wrong answer
// fails the test at any deadline — so it follows the test binary's own
// deadline (minus a margin for the report, never beyond it) instead of a
// fixed 30 s: decoding 16 MiB on three wires under -race took 24 s at host
// load 2 and all three wires ran into a 30 s deadline at an elevenfold core
// contention (host load ≥ 55 in the wave builds), and the frame-cap proof
// (4194312 empty data lines per parser) took 28–42 s for both parsers in the
// green nightlies of 14–23 Nov (-race with -covermode=atomic on the two-core
// runner) and over 30 s for one parser in the red ones (#540) — each time the
// fixed value reported a slow host as a cap failure.
// Without a binary deadline (-timeout 0) the net is 10 min.
func capProofTimeout(t *testing.T) time.Duration {
	t.Helper()
	if dl, ok := t.Deadline(); ok {
		return max(time.Until(dl)-10*time.Second, time.Second)
	}
	return 10 * time.Minute
}

// TestChatBodyCap (GHSA 2026-10-31 §3): a chat answer over MaxResponseBytes
// is ErrResponseTooLarge — on the openai, ollama and anthropic wires — and
// not a decoded result. The body is valid JSON, so without the cap it would
// decode (that is the red proof).
func TestChatBodyCap(t *testing.T) {
	pad := int(httpx.MaxResponseBytes/1024) + 64 // > 16 MiB of content
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		head, tail := `{"choices":[{"message":{"role":"assistant","content":"`, `"},"finish_reason":"stop"}]}`
		switch r.URL.Path {
		case "/api/chat":
			head, tail = `{"message":{"role":"assistant","content":"`, `"},"done":true}`
		case "/v1/messages":
			head, tail = `{"content":[{"type":"text","text":"`, `"}],"stop_reason":"end_turn"}`
		}
		_, _ = io.WriteString(w, head)
		_, _ = io.Copy(w, repeatReader(strings.Repeat("a", 1024), pad))
		_, _ = io.WriteString(w, tail)
	}))
	defer srv.Close()

	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama, backends.ProtocolAnthropic} {
		b := backends.Backend{Host: srv.URL, Protocol: p, Model: "m", APIKey: "k"}
		_, err := Chat(context.Background(), b, "s", "u", Options{}, capProofTimeout(t))
		if !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("%s: err = %v, want ErrResponseTooLarge", p, err)
		}
	}
}

// TestSSEStreamCaps: both SSE parsers end a stream whose single line, single
// data frame or total size exceeds its cap with ErrResponseTooLarge. Each
// fixture is otherwise a complete, valid stream (it ends with a finished
// frame), so an uncapped parser returns a result instead.
func TestSSEStreamCaps(t *testing.T) {
	anthropicEnd := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	big := strings.Repeat("x", 1024)
	cases := []struct {
		name string
		body func(end string) io.Reader
	}{
		{"one long line", func(end string) io.Reader {
			return io.MultiReader(strings.NewReader(": "), repeatReader(big, maxSSEFrameBytes/1024+1), strings.NewReader("\n\n"+end))
		}},
		{"one endless frame", func(end string) io.Reader {
			return io.MultiReader(repeatReader("data: "+big+"\n", maxSSEFrameBytes/1024+8), strings.NewReader("\n"+end))
		}},
		{"total over the stream cap", func(end string) io.Reader {
			return io.MultiReader(repeatReader(": "+big+"\n", int(maxStreamBytes/1024)+16), strings.NewReader(end))
		}},
	}
	for _, c := range cases {
		if _, err := parseSSEStream(c.body(okFrame), nil); !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("openai %s: err = %v, want ErrResponseTooLarge", c.name, err)
		}
		if _, err := parseAnthropicSSE(c.body(anthropicEnd), nil); !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("anthropic %s: err = %v, want ErrResponseTooLarge", c.name, err)
		}
	}

	// Controls: the same parsers accept the fixtures' endings on their own.
	if _, err := parseSSEStream(strings.NewReader(okFrame), nil); err != nil {
		t.Errorf("openai control: %v", err)
	}
	if _, err := parseAnthropicSSE(strings.NewReader(anthropicEnd), nil); err != nil {
		t.Errorf("anthropic control: %v", err)
	}
}

// sseParsers runs one stream through both SSE wires.
var sseParsers = []struct {
	name  string
	parse func(io.Reader, func(StreamEvent) error) (*StreamResult, error)
}{
	{"openai", parseSSEStream},
	{"anthropic", parseAnthropicSSE},
}

// lineFeed is a stream of n copies of line and then the line tail that
// counts the lines it has handed out. Up to line exact it fills each Read
// with as many lines as fit; from there on it hands out at most one line per
// Read. A parser's bufio.Reader reads only when its buffer holds no complete
// line, so once the parser has returned, read is exactly the line at which
// it stopped if that line is exact or later, and at most exact if it stopped
// earlier. Line by line from the start, the 4 MiB lines of the frame-cap
// proof cost twice the parse under -race (5.7–7.5 s against 11.5–17.7 s for
// both parsers, #540); the window keeps the count exact where it is judged.
type lineFeed struct {
	line, tail string
	total      int          // lines of the stream: n copies of line, then tail
	exact      int          // the last line handed out in bulk
	sent, off  int          // complete lines handed out, bytes of the current one
	lines      atomic.Int64 // sent, published after each Read (a hang report reads it)
}

func newLineFeed(line string, n int, tail string, exact int) *lineFeed {
	return &lineFeed{line: line, tail: tail, total: n + 1, exact: exact}
}

func (f *lineFeed) Read(p []byte) (int, error) {
	if f.sent >= f.total {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && f.sent < f.total {
		cur := f.line
		if f.sent == f.total-1 {
			cur = f.tail
		}
		k := copy(p[n:], cur[f.off:])
		n += k
		if f.off += k; f.off < len(cur) {
			break // p is full inside a line
		}
		f.off = 0
		f.sent++
		if f.sent >= f.exact {
			break // from line exact on: one line per Read
		}
	}
	f.lines.Store(int64(f.sent))
	return n, nil
}

func (f *lineFeed) read() int { return int(f.lines.Load()) }

// parseWithin runs parse in a goroutine and returns its error and how long
// it took, once it has returned. The wait is a hang guard, not a speed
// judgement (capProofTimeout): a parser that has not returned by then fails
// the test as hanging, with how far it read when the stream is a lineFeed —
// a parser quadratic in the lines of a frame would otherwise hold the test
// for hours.
func parseWithin(t *testing.T, parse func(io.Reader, func(StreamEvent) error) (*StreamResult, error), r io.Reader) (time.Duration, error) {
	t.Helper()
	guard := capProofTimeout(t)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := parse(r, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(guard):
		progress := ""
		if f, ok := r.(*lineFeed); ok {
			progress = fmt.Sprintf(" (%d of %d lines read)", f.read(), f.total)
		}
		t.Fatalf("parser hangs: still running after %v%s", guard, progress)
		return 0, nil
	}
}

// linearCostPasses is how many passes TestSSEFrameLinearCost measures before
// it fails: a pass in which both bounds hold ends the test, so a host that
// stalls one pass does not flip it, and a parser that is quadratic fails
// every pass.
const linearCostPasses = 3

// TestSSEFrameLinearCost (§D): the frame cap is checked on every data line,
// so its check must not re-measure the whole buffered frame each time. The
// pin is the growth of the parse time with the lines of one frame: from
// 25000 to 200000 empty data lines — eight times the lines — a linear parser
// takes about eight times as long, a per-line re-sum sixty-four times (it took
// about ten seconds for 200000 lines before 98715fbb); the bound 22 sits
// between them. A growth is a ratio of two times on the same host, so a slow
// host does not move it; instrumentation (-race, coverage) slows the linear
// part, which dilutes a quadratic part in code it does not instrument
// (assembly such as bytes.Count), and frames past 200000 lines are
// TestSSEFrameCapCountsEmptyLines' to judge. Each size is the fastest of
// three runs: a stalled small run would hide a quadratic parser, a stalled
// large one would flip a linear one. The absolute bound (200000 lines within
// 2 s, two orders of magnitude above the linear cost) is judged only without
// coverage: under -covermode=atomic with -race it measures the
// instrumentation and the runner's load — up to 1.43 s of the 2 s in the
// nightly-form runs of 18–24 Nov (#540) — and is logged, not judged (the rule
// of TestExternalBulkPickCostScales in dispatch). The frames are newlines,
// not JSON: the parser reports them as malformed, and the cap must not fire —
// 200 KB is far below it.
func TestSSEFrameLinearCost(t *testing.T) {
	const small, large, growthMax, largeMax = 25_000, 200_000, 22.0, 2 * time.Second
	parse := func(p func(io.Reader, func(StreamEvent) error) (*StreamResult, error), name string, lines int) time.Duration {
		took, err := parseWithin(t, p, io.MultiReader(repeatReader("data:\n", lines), strings.NewReader("\n")))
		if err == nil || errors.Is(err, httpx.ErrResponseTooLarge) || !strings.Contains(err.Error(), "malformed stream chunk") {
			t.Fatalf("%s, %d lines: err = %v, want a malformed stream chunk", name, lines, err)
		}
		return took
	}
	var over []string
	for pass := 1; pass <= linearCostPasses; pass++ {
		var bad []string
		for _, p := range sseParsers {
			s := min(parse(p.parse, p.name, small), parse(p.parse, p.name, small), parse(p.parse, p.name, small))
			l := min(parse(p.parse, p.name, large), parse(p.parse, p.name, large), parse(p.parse, p.name, large))
			growth := float64(l) / float64(s)
			t.Logf("pass %d, %s: %d empty data lines in %v, %d in %v — growth %.1f (linear 8, quadratic 64)", pass, p.name, small, s, large, l, growth)
			if growth >= growthMax {
				bad = append(bad, fmt.Sprintf("%s: the parse time grows by %.1f from %d to %d lines of one frame, want < %v (one comparison per line)", p.name, growth, small, large, growthMax))
			}
			if l >= largeMax {
				msg := fmt.Sprintf("%s: %d empty data lines took %v, want < %v", p.name, large, l, largeMax)
				if mode := testing.CoverMode(); mode != "" {
					t.Logf("pass %d, %s — not judged under -covermode=%s", pass, msg, mode)
				} else {
					bad = append(bad, msg)
				}
			}
		}
		if len(bad) == 0 {
			return
		}
		over = append(over, fmt.Sprintf("pass %d: %s", pass, strings.Join(bad, "; ")))
	}
	t.Errorf("every pass over its bound:\n%s", strings.Join(over, "\n"))
}

// capRefLines and capGrowthMax: TestSSEFrameCapCountsEmptyLines' reference
// frame and its bound, in times the line ratio (the cap-size frame has 168
// times its lines). A linear parser stays near 1; GC of the larger frame and
// a host that slows down during the cap-size run lift it, and 8 leaves room
// for that while the review's growth forms of #540 (16 and 47) stay over it.
const (
	capRefLines  = 25_000
	capGrowthMax = 8.0
)

// TestSSEFrameCapCountsEmptyLines (§D): an empty data: line adds the newline
// that joins it to the frame, so a frame of more than maxSSEFrameBytes empty
// lines is over the cap and ends with ErrResponseTooLarge — not a stream that
// runs on to EOF because its lines weigh nothing. The pin is the line at
// which each parser stops (lineFeed counts it): a frame of j empty lines is
// j-1 joining newlines, so the frame holds maxSSEFrameBytes+1 of them (a
// payload of exactly the cap) and line maxSSEFrameBytes+2 is the first one
// the cap refuses — every empty line weighs one byte, no more, no less. A
// parser that reads ahead of the line it handles moves the count as well
// (lineFeed); the failure says so.
//
// The time of the 4 MiB of lines is judged only against itself: a fixed
// bound measured the runner (the nightly form's times for the same code
// spread past 30 s, capProofTimeout, #540), but the frame is also the only
// one of cap size, so a cost that grows faster than its lines only there
// (the buffer growing by a fixed step past some size, the frame re-measured
// every few hundred lines: 16 and 47 times the linear time in the review of
// #540) must still fail. The growth against the fastest of three frames of
// capRefLines lines, before and after it (the slower of the two, so a host
// that slows down during the test does not count), is at most capGrowthMax
// times the line ratio.
func TestSSEFrameCapCountsEmptyLines(t *testing.T) {
	// window: the lines before stop that the feed hands out one by one, so a
	// parser that stops up to that many lines early is reported at its line.
	const stop, window = maxSSEFrameBytes + 2, 4096
	ref := func(parse func(io.Reader, func(StreamEvent) error) (*StreamResult, error), name string) time.Duration {
		var best time.Duration
		for i := range 3 {
			took, err := parseWithin(t, parse, newLineFeed("data:\n", capRefLines, "\n", capRefLines+2))
			if err == nil || errors.Is(err, httpx.ErrResponseTooLarge) || !strings.Contains(err.Error(), "malformed stream chunk") {
				t.Fatalf("%s, %d lines: err = %v, want a malformed stream chunk", name, capRefLines, err)
			}
			if i == 0 || took < best {
				best = took
			}
		}
		return best
	}
	for _, p := range sseParsers {
		before := ref(p.parse, p.name)
		feed := newLineFeed("data:\n", maxSSEFrameBytes+8, "\n", stop-window)
		took, err := parseWithin(t, p.parse, feed)
		after := ref(p.parse, p.name)
		if !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("%s: err = %v, want ErrResponseTooLarge", p.name, err)
		}
		at := fmt.Sprintf("at line %d", feed.read())
		if feed.read() <= feed.exact {
			at = fmt.Sprintf("at or before line %d", feed.read())
		}
		if feed.read() != stop {
			t.Errorf("%s: the parser stopped %s of %d, want line %d — the first empty data line past the cap (or the parser read ahead of its line)", p.name, at, feed.total, stop)
		}
		growth := float64(took) / float64(max(before, after)) / (float64(stop) / capRefLines)
		t.Logf("%s: stopped %s after %v; %d lines in %v before, %v after — %.2f times the line ratio", p.name, at, took, capRefLines, before, after, growth)
		if growth >= capGrowthMax {
			t.Errorf("%s: %d lines of one frame took %.1f times the line ratio of a frame of %d lines, want < %v — the cost grows faster than the lines", p.name, feed.read(), growth, capRefLines, capGrowthMax)
		}
	}
}

// TestSSEFrameCapIsPayloadSize (§D): the frame cap bounds the payload as it
// is dispatched — the data lines and the newlines that join them. A frame of
// exactly maxSSEFrameBytes dispatches (it is no JSON, so the parser reports a
// malformed chunk); one byte more is ErrResponseTooLarge.
func TestSSEFrameCapIsPayloadSize(t *testing.T) {
	const lines = 5
	room := maxSSEFrameBytes - (lines - 1) // payload minus the joining newlines
	x := strings.Repeat("x", room/lines+room%lines+1)
	frame := func(extra int) io.Reader {
		var b strings.Builder
		for i := range lines {
			n := room / lines
			if i == lines-1 {
				n += room%lines + extra
			}
			b.WriteString("data: " + x[:n] + "\n")
		}
		b.WriteString("\n")
		return strings.NewReader(b.String())
	}
	for _, p := range sseParsers {
		if _, err := p.parse(frame(0), nil); err == nil || errors.Is(err, httpx.ErrResponseTooLarge) || !strings.Contains(err.Error(), "malformed stream chunk") {
			t.Errorf("%s at the cap: err = %v, want a malformed stream chunk", p.name, err)
		}
		if _, err := p.parse(frame(1), nil); !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("%s one byte over the cap: err = %v, want ErrResponseTooLarge", p.name, err)
		}
	}
}

// errAfter yields r, then fails every further read with err — a connection
// that breaks mid-stream.
type errAfter struct {
	r   io.Reader
	err error
}

func (e *errAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, e.err
	}
	return n, err
}

// TestSSEReadErrorWins (§D): a read error ends the stream with that error.
// The line it cut short is not a line — its empty stand-in must not count as
// the blank separator that dispatches the buffered frame: not a content delta
// to onEvent, and not a buffered end frame ([DONE] / message_stop) that would
// turn the error into a finished turn. Each case buffers one frame without
// its blank line, then fails the next line: an over-long line (the frame
// cap) or a broken connection.
func TestSSEReadErrorWins(t *testing.T) {
	errReset := errors.New("connection reset")
	overlong := func(head string) io.Reader {
		return io.MultiReader(strings.NewReader(head), repeatReader(strings.Repeat("x", 1024), maxSSEFrameBytes/1024+1))
	}
	broken := func(head string) io.Reader {
		return &errAfter{r: strings.NewReader(head), err: errReset}
	}
	anthropicStop := "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n"
	heads := []struct {
		name, openai, anthropic string
	}{
		{"buffered end frame",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n",
			anthropicStop},
		{"buffered content frame",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n"},
	}
	tails := []struct {
		name string
		body func(head string) io.Reader
		want error
	}{
		{"over-long line", overlong, httpx.ErrResponseTooLarge},
		{"broken connection", broken, errReset},
	}
	for _, h := range heads {
		for _, tl := range tails {
			for _, p := range sseParsers {
				head := h.openai
				if p.name == "anthropic" {
					head = h.anthropic
				}
				var deltas []string
				onEvent := func(ev StreamEvent) error {
					if ev.ContentDelta != "" {
						deltas = append(deltas, ev.ContentDelta)
					}
					return nil
				}
				res, err := p.parse(tl.body(head), onEvent)
				if !errors.Is(err, tl.want) || res != nil {
					t.Errorf("%s, %s, %s: res = %v, err = %v; want no result and %v", p.name, h.name, tl.name, res != nil, err, tl.want)
				}
				// The end-frame case dispatched its content frame before the
				// buffered end frame; the content case dispatched nothing.
				wantDeltas := 0
				if h.name == "buffered end frame" && p.name == "openai" {
					wantDeltas = 1
				}
				if len(deltas) != wantDeltas {
					t.Errorf("%s, %s, %s: onEvent saw %q, want %d content deltas", p.name, h.name, tl.name, deltas, wantDeltas)
				}
			}
		}
	}
}
