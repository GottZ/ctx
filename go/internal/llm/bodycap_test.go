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
		_, err := Chat(context.Background(), b, "s", "u", Options{}, 30*time.Second)
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
