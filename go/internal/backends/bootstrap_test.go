package backends

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// --- A02-W5: the conditional env seed (design/02 §4.1d).

// refusingQuerier fails every database call. It is the probe for "did the
// bootstrap touch the database at all": the default short-circuit has to
// return before the EXISTS query, so any contact surfaces as an error.
type refusingQuerier struct {
	queries int
	execs   int
}

func (r *refusingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	r.queries++
	return nil, errors.New("refusingQuerier: bootstrap must not probe here")
}

func (r *refusingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	r.execs++
	return pgconn.CommandTag{}, errors.New("refusingQuerier: bootstrap must not insert here")
}

// TestBootstrapSkipsDefaultIdenticalInput is the W5 gate at unit level: an
// input byte-identical to the registry defaults is a no-op that never reaches
// the database — the fresh install keeps its empty table for the replacement
// seed path, and the W4 advisory names the state.
//
// Mutation probe: drop the MatchesDefaults early return in Bootstrap and this
// goes red on the refusingQuerier error.
func TestBootstrapSkipsDefaultIdenticalInput(t *testing.T) {
	defaults := bootstrapFixture() // stands in for the registry defaults here
	q := &refusingQuerier{}

	n, err := Bootstrap(context.Background(), q, bootstrapFixture(), defaults)
	if err != nil {
		t.Fatalf("Bootstrap on default-identical input: %v, want no-op", err)
	}
	if n != 0 {
		t.Errorf("inserted = %d, want 0", n)
	}
	if q.queries != 0 || q.execs != 0 {
		t.Errorf("database touched (%d queries, %d execs) — the default check must return first",
			q.queries, q.execs)
	}
}

// TestBootstrapStillRunsForConfiguredInput is the negative half: ONE moved
// field is enough to make the input configured again, and the seed path must
// stay fully alive for that population — W5 conditionalizes the path, W6
// removes it. Reaching the probe is the assertion; the refusingQuerier turns
// it into a visible error.
func TestBootstrapStillRunsForConfiguredInput(t *testing.T) {
	defaults := bootstrapFixture()
	in := bootstrapFixture()
	in.Chat.Host = "http://configured.example:8089"

	q := &refusingQuerier{}
	if _, err := Bootstrap(context.Background(), q, in, defaults); err == nil {
		t.Fatal("configured input returned without probing the table — the env seed died a wave early")
	}
	if q.queries != 1 {
		t.Errorf("queries = %d, want 1 (the EXISTS probe)", q.queries)
	}
}

// TestMatchesDefaultsCoversEveryTupleField pins the whole-struct semantics:
// each configurable leg of the input — including the rerank fields and the
// optional fallback pointer — flips the verdict on its own. A field checklist
// would pass this test only by accident and lose the next added field.
func TestMatchesDefaultsCoversEveryTupleField(t *testing.T) {
	defaults := bootstrapFixture()
	if !bootstrapFixture().MatchesDefaults(defaults) {
		t.Fatal("identical inputs must match — comparison base is broken")
	}

	cases := map[string]func(*BootstrapInput){
		"chat host":     func(in *BootstrapInput) { in.Chat.Host = "http://other.example:8089" },
		"chat api key":  func(in *BootstrapInput) { in.Chat.APIKey = "sk-configured" },
		"chat num_ctx":  func(in *BootstrapInput) { in.Chat.NumCtx = 4096 },
		"chat think":    func(in *BootstrapInput) { in.Chat.Think = "true" },
		"embed model":   func(in *BootstrapInput) { in.Embed.Model = "other-embed" },
		"dream model":   func(in *BootstrapInput) { in.Dream.Model = "other-dream" },
		"dream embed":   func(in *BootstrapInput) { in.DreamEmbed.Host = "http://other.example:8090" },
		"fallback gone": func(in *BootstrapInput) { in.Fallback = nil },
		"fallback host": func(in *BootstrapInput) { in.Fallback.Host = "http://127.0.0.1:9099" },
		"rerank host":   func(in *BootstrapInput) { in.RerankHost = "" },
		"rerank model":  func(in *BootstrapInput) { in.RerankModel = "other-rerank" },
		"rerank key":    func(in *BootstrapInput) { in.RerankKey = "sk-configured" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := bootstrapFixture()
			mutate(&in)
			if in.MatchesDefaults(defaults) {
				t.Errorf("a moved %s still counts as untouched default", name)
			}
		})
	}
}
