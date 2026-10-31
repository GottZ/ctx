package embed

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
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
