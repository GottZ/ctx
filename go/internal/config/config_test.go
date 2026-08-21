package config

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
)

// TestDreamBackendInheritance pins the derivation that replaces the 3×
// duplicated fallback chain, including Delta 1 (decision §7.2: NumCtx
// inherits from chat when 0 — unified with the daily-synthesis derivation).
func TestDreamBackendInheritance(t *testing.T) {
	cases := []struct {
		name string
		vals map[string]string
		want backends.Backend
	}{
		{
			"full inherit",
			map[string]string{
				"chat.model": "chat-27b", "chat.num_ctx": "98304", "chat.think": "true",
			},
			backends.Backend{
				Host: "http://localhost:11434", Protocol: backends.ProtocolOllama,
				Model: "chat-27b", NumCtx: 98304, Think: "true",
			},
		},
		{
			"explicit dream wins",
			map[string]string{
				"chat.model": "chat-27b", "chat.num_ctx": "98304", "chat.think": "true",
				"dream.model": "dream-9b", "dream.num_ctx": "32768", "dream.think": "false",
				"dream.host": "http://dream.example:8089",
			},
			backends.Backend{
				Host: "http://dream.example:8089", Protocol: backends.ProtocolOllama,
				Model: "dream-9b", NumCtx: 32768, Think: "false",
			},
		},
		{
			"partial: model own, num_ctx+think inherited (Delta 1)",
			map[string]string{
				"chat.model": "chat-27b", "chat.num_ctx": "98304", "chat.think": "false",
				"dream.model": "dream-9b",
			},
			backends.Backend{
				Host: "http://localhost:11434", Protocol: backends.ProtocolOllama,
				Model: "dream-9b", NumCtx: 98304, Think: "false",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := cfgFrom(t, c.vals)
			if got := cfg.DreamBackend(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("DreamBackend() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestDreamEmbedBackendInheritance pins the field-by-field fallback onto
// Embed.* (today's scheduler semantics; the cross-host credential case is
// V12's job, not this derivation's).
func TestDreamEmbedBackendInheritance(t *testing.T) {
	cfg, _ := cfgFrom(t, map[string]string{
		"embed.host": "http://embed.example:8081", "embed.model": "embed-8b",
		"embed.num_ctx": "2048", "embed.protocol": "openai",
		"dream_embed.model": "dream-embed-4b",
	})
	got := cfg.DreamEmbedBackend()
	want := backends.Backend{
		Host: "http://embed.example:8081", Protocol: backends.ProtocolOpenAI,
		Model: "dream-embed-4b", NumCtx: 2048,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DreamEmbedBackend() = %+v, want %+v", got, want)
	}
}

// TestDreamBackoffConversion pins the converter to the dream-stage parameter
// struct (F1-W6 — replaces the 6 dream package vars), including the
// Hours→float64 unit carry of the h|d|w|m|y suffix forms.
func TestDreamBackoffConversion(t *testing.T) {
	cfg, _ := cfgFrom(t, map[string]string{
		"dream.backoff_mode": "linear", "dream.backoff_factor": "2.5",
		"dream.backoff_grace": "3", "dream.backoff_cap": "10d",
		"dream.backoff_min": "6h", "dream.backoff_inert_offset": "4",
	})
	got := cfg.DreamBackoff()
	want := dream.BackoffConfig{
		Mode: "linear", Factor: 2.5, Grace: 3,
		MinHours: 6, CapHours: 240, InertOffset: 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DreamBackoff() = %+v, want %+v", got, want)
	}
}

// TestChatFallbackBackend died with the chat_fallback tuple in β4. It pinned
// the accessor's nil-when-off contract and the bare-int-seconds carry into
// Backend.Timeout; the accessor had no caller left (the query path's fallback
// leg moved to the pool with F3-P2, llm/client.go:154), and the seconds parse
// it exercised on the way is pinned on a living Duration key in
// TestDumpSourcesAndRendering (dream.idle_wait).

// TestDSN pins the legacy DSN shape including URL-encoding of credentials.
func TestDSN(t *testing.T) {
	cfg, _ := cfgFrom(t, map[string]string{
		"server.db_user":     "user@x",
		"server.db_password": "p@ss:w0rd",
		"server.db_host":     "db.example",
		"server.db_port":     "5433",
		"server.db":          "ctx",
		"server.db_sslmode":  "require",
	})
	want := "postgres://user%40x:p%40ss%3Aw0rd@db.example:5433/ctx?sslmode=require"
	if got := cfg.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

// TestSynthesisSettings pins the 1:1 mapping into the llm synthesis
// parameter struct (F1-W4: the derivation moves here from the cmd/ctxd
// bridge — one source, no third copy).
func TestSynthesisSettings(t *testing.T) {
	cfg, _ := cfgFrom(t, map[string]string{
		"query.score_threshold":     "0.002",
		"query.confident_threshold": "0.01",
		"query.prompt_version":      "v6",
	})
	ss := cfg.SynthesisSettings()
	if ss.ScoreThreshold != 0.002 || ss.ConfidentThreshold != 0.01 || ss.PromptVersion != "v6" {
		t.Errorf("SynthesisSettings() = %+v", ss)
	}
}

// TestRRFConversions pins the 1:1 mapping into the rrf parameter structs.
func TestRRFConversions(t *testing.T) {
	cfg, _ := cfgFrom(t, map[string]string{
		"rerank.enabled":  "true",
		"rerank.max_docs": "40", "rerank.blend_weight": "0.5",
		"graph.enabled": "true", "graph.hop_depth": "2", "graph.boost_weight": "0.25",
	})
	rc := cfg.RerankRRF()
	if !rc.Enabled || rc.MaxDocs != 40 || rc.BlendWeight != 0.5 {
		t.Errorf("RerankRRF() = %+v", rc)
	}
	gc := cfg.GraphRRF()
	if !gc.Enabled || gc.HopDepth != 2 || gc.BoostWeight != 0.25 {
		t.Errorf("GraphRRF() = %+v", gc)
	}
}
