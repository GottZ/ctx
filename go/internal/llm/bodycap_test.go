package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// chatCapTimeout is the Chat deadline for the body-cap proofs. It is a safety
// net only — the proof is ErrResponseTooLarge, and a decoded result fails the
// test at any deadline — so it follows the test binary's own deadline (minus a
// margin for the report, never beyond it) instead of a fixed 30 s: decoding
// 16 MiB on three wires under -race took 24 s at host load 2 and all three
// wires ran into a 30 s deadline at an elevenfold core contention (host load
// ≥ 55 in the wave builds), which the fixed value reported as a cap failure.
// Without a binary deadline (-timeout 0) the net is 10 min.
func chatCapTimeout(t *testing.T) time.Duration {
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
		_, err := Chat(context.Background(), b, "s", "u", Options{}, chatCapTimeout(t))
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

// parseWithin runs parse in a goroutine and fails the test once it has not
// returned after limit: a parser that is quadratic in the lines of a frame
// would otherwise hold the test for hours instead of failing it.
func parseWithin(t *testing.T, limit time.Duration, parse func(io.Reader, func(StreamEvent) error) (*StreamResult, error), r io.Reader) (time.Duration, error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := parse(r, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(limit):
		t.Fatalf("parser still running after %v", limit)
		return 0, nil
	}
}

// TestSSEFrameLinearCost (§D): the frame cap is checked on every data line,
// so its check must not re-measure the whole buffered frame each time — a
// frame of 200000 empty data lines (1.2 MB) parses in milliseconds, where a
// per-line re-sum takes about ten seconds. The bound is generous on purpose
// (two orders of magnitude above the linear cost, a fifth of the quadratic
// one), so CI jitter cannot flip it. The frame is newlines, not JSON: the
// parser reports it as malformed, and the cap must not fire — 200 KB is far
// below it.
func TestSSEFrameLinearCost(t *testing.T) {
	for _, p := range sseParsers {
		took, err := parseWithin(t, 2*time.Second, p.parse, io.MultiReader(repeatReader("data:\n", 200_000), strings.NewReader("\n")))
		if err == nil || errors.Is(err, httpx.ErrResponseTooLarge) || !strings.Contains(err.Error(), "malformed stream chunk") {
			t.Errorf("%s: err = %v, want a malformed stream chunk", p.name, err)
		}
		t.Logf("%s: 200000 empty data lines in %v", p.name, took)
	}
}

// TestSSEFrameCapCountsEmptyLines (§D): an empty data: line adds the newline
// that joins it to the frame, so a frame of more than maxSSEFrameBytes empty
// lines is over the cap and ends with ErrResponseTooLarge — not a stream that
// runs on to EOF because its lines weigh nothing.
func TestSSEFrameCapCountsEmptyLines(t *testing.T) {
	for _, p := range sseParsers {
		_, err := parseWithin(t, 30*time.Second, p.parse, io.MultiReader(repeatReader("data:\n", maxSSEFrameBytes+8), strings.NewReader("\n")))
		if !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("%s: err = %v, want ErrResponseTooLarge", p.name, err)
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
