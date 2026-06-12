package backends

import (
	"testing"
	"time"
)

// Synthetic fixture hosts (RFC-2606) — never real deployment topology.
func bootstrapFixture() BootstrapInput {
	return BootstrapInput{
		Chat: Backend{Host: "http://gpu.example:8089", Protocol: ProtocolOpenAI,
			Model: "chat-model", NumCtx: 96000, Think: "false"},
		Fallback: &Backend{Host: "http://127.0.0.1:8088", Protocol: ProtocolOpenAI,
			Timeout: 420 * time.Second},
		Embed: Backend{Host: "http://127.0.0.1:8090", Protocol: ProtocolOpenAI,
			Model: "embed-model", NumCtx: 8192},
		Dream: Backend{Host: "http://gpu.example:8089", Protocol: ProtocolOpenAI,
			Model: "chat-model", NumCtx: 96000, Think: "false"},
		DreamEmbed: Backend{Host: "http://127.0.0.1:8090", Protocol: ProtocolOpenAI,
			Model: "embed-model", NumCtx: 8192},
		RerankHost: "http://gpu.example:8091", RerankModel: "rerank-model",
		RerankTimeoutS: 180,
	}
}

func rowByName(t *testing.T, rows []Backend, name string) *Backend {
	t.Helper()
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

// TestBootstrapRowsDedup: identical dream/chat and dream-embed/embed tuples
// merge into single rows — the historical chat==dream num_ctx invariant
// resolves structurally (one row, one num_ctx).
func TestBootstrapRowsDedup(t *testing.T) {
	rows, warnings := BootstrapRows(bootstrapFixture())
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(rows) != 4 {
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = r.Name
		}
		t.Fatalf("rows = %v, want 4 (chat, cpu, embed, rerank)", names)
	}

	chat := rowByName(t, rows, "herbert-chat")
	if chat == nil {
		t.Fatal("herbert-chat missing")
	}
	if !chat.HasRole(RoleDream) {
		t.Error("dream role not merged into chat row")
	}
	if !chat.HasRole(RoleDigest) || !chat.HasRole(RoleTranslate) || !chat.HasRole(RoleSynthesis) {
		t.Errorf("chat roles incomplete: %v", chat.Roles)
	}
	if _, hasOverride := chat.ModelMap[RoleDream]; hasOverride {
		t.Error("identical dream tuple produced a per-role override")
	}
	// Think travels as params.think (per-(backend,role) model behavior).
	if think, _ := chat.ModelMap["default"].Params["think"].(bool); think != false {
		t.Errorf("think param lost: %v", chat.ModelMap["default"].Params)
	}

	embed := rowByName(t, rows, "llama-embed")
	if embed == nil || !embed.HasRole(RoleDreamEmbed) {
		t.Error("identical dream-embed not merged into embed row")
	}

	cpu := rowByName(t, rows, "llama-cpu")
	if cpu == nil {
		t.Fatal("llama-cpu missing")
	}
	if cpu.HasRole(RoleDream) {
		t.Error("cpu got the dream role (risk 6.5: DreamTimeout tears at CPU rates)")
	}
	if cpu.ModelMap["default"].Model != "chat-model" {
		t.Error("fallback model inheritance not materialized")
	}
	if cpu.Timeouts[RoleSynthesis] != 420 {
		t.Errorf("fallback timeout lost: %v", cpu.Timeouts)
	}
	if cpu.Priority >= chat.Priority {
		t.Error("cpu priority must rank below the primary")
	}

	rr := rowByName(t, rows, "herbert-rerank")
	if rr == nil || rr.Protocol != "rerank" || rr.Timeouts[RoleRerank] != 180 {
		t.Errorf("rerank row malformed: %+v", rr)
	}
}

// TestBootstrapRowsSplit: diverging dream/dream-embed tuples become own rows
// with their own role — the config dimension never silently disappears.
func TestBootstrapRowsSplit(t *testing.T) {
	in := bootstrapFixture()
	in.Dream.Host = "http://other-gpu.example:9000"
	in.DreamEmbed.Host = "http://cpu2.example:8092"
	rows, _ := BootstrapRows(in)

	if rowByName(t, rows, "herbert-dream") == nil {
		t.Error("diverging dream host did not split into herbert-dream")
	}
	de := rowByName(t, rows, "dream-embed")
	if de == nil {
		t.Fatal("diverging dream-embed host did not split")
	}
	if !de.HasRole(RoleDreamEmbed) || de.HasRole(RoleEmbed) {
		t.Errorf("dream-embed must carry ONLY its own role (embed chain ordering semantics): %v", de.Roles)
	}

	chat := rowByName(t, rows, "herbert-chat")
	if chat.HasRole(RoleDream) {
		t.Error("chat kept the dream role despite the split")
	}
}

// TestBootstrapRowsModelOverride: same host, different dream model → one row
// with a per-role model_map entry (lossless merge).
func TestBootstrapRowsModelOverride(t *testing.T) {
	in := bootstrapFixture()
	in.Dream.Model = "dream-model-27b"
	rows, _ := BootstrapRows(in)

	chat := rowByName(t, rows, "herbert-chat")
	if chat.ModelMap[RoleDream].Model != "dream-model-27b" {
		t.Errorf("per-role dream model lost: %v", chat.ModelMap)
	}
	if chat.ModelMap["default"].Model != "chat-model" {
		t.Errorf("default model corrupted: %v", chat.ModelMap)
	}
}

// TestBootstrapRowsKeyWarning: env api keys cannot travel into the table —
// plaintext keys never live in context_backends.
func TestBootstrapRowsKeyWarning(t *testing.T) {
	in := bootstrapFixture()
	in.Chat.APIKey = "sk-test-0123456789abcdef"
	_, warnings := BootstrapRows(in)
	if len(warnings) == 0 {
		t.Fatal("env api key produced no warning")
	}
	for _, w := range warnings {
		if len(w) > 0 && (contains(w, "sk-test") || contains(w, "0123456789abcdef")) {
			t.Fatalf("warning leaks key material: %q", w)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && stringsIndex(s, sub) >= 0
}

func stringsIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestBootstrapRowsNoFallback: an absent fallback host produces no cpu row.
func TestBootstrapRowsNoFallback(t *testing.T) {
	in := bootstrapFixture()
	in.Fallback = nil
	in.RerankHost = ""
	rows, _ := BootstrapRows(in)
	if rowByName(t, rows, "llama-cpu") != nil || rowByName(t, rows, "herbert-rerank") != nil {
		t.Error("absent tuples produced rows")
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
}

// TestBootstrapLocalityDerivation: lan for the RFC-2606 GPU host is wrong on
// purpose? No: gpu.example is a public FQDN → external by derivation. The
// REAL deployment derives lan from its RFC1918 IP. This pins that the
// derivation runs at all.
func TestBootstrapLocalityDerivation(t *testing.T) {
	in := bootstrapFixture()
	in.Chat.Host = "http://10.13.37.11:8089"
	in.Dream.Host = in.Chat.Host
	rows, _ := BootstrapRows(in)
	chat := rowByName(t, rows, "herbert-chat")
	if chat.Locality != LocalityLAN {
		t.Errorf("RFC1918 host locality = %s, want lan", chat.Locality)
	}
	cpu := rowByName(t, rows, "llama-cpu")
	if cpu.Locality != LocalityLocal {
		t.Errorf("loopback host locality = %s, want local", cpu.Locality)
	}
}
