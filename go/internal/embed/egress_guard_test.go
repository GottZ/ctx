package embed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

// TestEmbedTenantRowNeverDialsLoopback (GHSA 2026-10-31 §3): both embed wires
// refuse a loopback base_url for a tenant row at dial time; a _global row on
// the same server embeds.
func TestEmbedTenantRowNeverDialsLoopback(t *testing.T) {
	rec := newEmbedWireRecorder(t)
	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		b := backends.Backend{Host: rec.srv.URL, Scope: "acme", Protocol: p, Model: "m"}
		_, _, err := Embed(context.Background(), b, "x", PrefixDocument)
		if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
			t.Fatalf("%s: tenant embed error = %v, want SSRF guard refusal", p, err)
		}
		if c := backends.Classify(err, ""); c != backends.ClassTransport {
			t.Errorf("%s: Classify = %v, want transport", p, c)
		}
		if path, _, _ := rec.recorded(); path != "" {
			t.Fatalf("%s: tenant embed reached the server (%s)", p, path)
		}
	}
	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		b := backends.Backend{Host: rec.srv.URL, Scope: backends.GlobalScope, Protocol: p, Model: "m"}
		if _, _, err := Embed(context.Background(), b, "x", PrefixDocument); err != nil {
			t.Errorf("_global %s embed: %v", p, err)
		}
	}
}

// TestEmbedBodyCap (GHSA 2026-10-31 §3): an embed answer over
// httpx.MaxResponseBytes is ErrResponseTooLarge on both wires, not decoded.
func TestEmbedBodyCap(t *testing.T) {
	chunk := strings.Repeat("0.5,", 256)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head, tail := `{"embeddings":[[`, `0.5]]}`
		if r.URL.Path == "/v1/embeddings" {
			head, tail = `{"data":[{"embedding":[`, `0.5]}]}`
		}
		_, _ = io.WriteString(w, head)
		for i := int64(0); i <= httpx.MaxResponseBytes/int64(len(chunk)); i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, tail)
	}))
	defer srv.Close()
	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		b := backends.Backend{Host: srv.URL, Protocol: p, Model: "m"}
		if _, _, err := Embed(context.Background(), b, "x", PrefixDocument); !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("%s: err = %v, want ErrResponseTooLarge", p, err)
		}
	}
}
