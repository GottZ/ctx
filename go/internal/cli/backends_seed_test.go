package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// buildSeedRows is the whole payload contract of `ctx backends seed` in one
// pure function — the posture the design pins (full-trust + confirm, neutral
// names, role split, priority 100, _global) is asserted here; the wire
// behaviour (idempotency, guards, abort classes) is the integration gate in
// internal/handler/backends_seed_cli_integration_test.go.

func mustRows(t *testing.T, spec seedSpec) map[string]seedRow {
	t.Helper()
	rows, err := buildSeedRows(spec)
	if err != nil {
		t.Fatalf("buildSeedRows: %v", err)
	}
	out := map[string]seedRow{}
	for _, r := range rows {
		out[r.name] = r
	}
	return out
}

func TestSeedRowsCarryFullTrustPosture(t *testing.T) {
	rows := mustRows(t, seedSpec{
		Chat:  seedBackend{Host: "http://gpu:11434", Model: "qwen3"},
		Embed: seedBackend{Host: "http://gpu:11434", Model: "qwen3-embed"},
	})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	for _, name := range []string{seedChatName, seedEmbedName} {
		r, ok := rows[name]
		if !ok {
			t.Fatalf("missing target row %q (E8 neutral names)", name)
		}
		if got := r.payload["trust"]; got != "full-trust" {
			t.Errorf("%s: trust = %v, want full-trust — a public row is trust-filtered out of its own credentials chain", name, got)
		}
		if got := r.payload["confirm_trust_elevation"]; got != true {
			t.Errorf("%s: confirm_trust_elevation = %v, want true — the create is rejected with 400 without it", name, got)
		}
		if got := r.payload["scope"]; got != "_global" {
			t.Errorf("%s: scope = %v, want _global", name, got)
		}
		if got := r.payload["priority"]; got != seedPriority {
			t.Errorf("%s: priority = %v, want %d", name, got, seedPriority)
		}
		if got := r.payload["protocol"]; got != "ollama" {
			t.Errorf("%s: protocol = %v, want the ollama default", name, got)
		}
		if _, ok := r.payload["api_key_ref"]; ok {
			t.Errorf("%s: api_key_ref must be absent without an api_key", name)
		}
		if r.secretName != "" || r.secretValue != "" {
			t.Errorf("%s: no secret must be planned without an api_key", name)
		}
	}
}

func TestSeedRowsSplitRolesAndModelMap(t *testing.T) {
	think := false
	rows := mustRows(t, seedSpec{
		Chat:  seedBackend{Host: "http://gpu:11434", Model: "qwen3", Think: &think, NumCtx: 262144},
		Embed: seedBackend{Host: "http://gpu:11435", Model: "qwen3-embed", Protocol: "openai"},
	})
	chat := rows[seedChatName]
	wantRoles := []string{"synthesis", "translate", "chat", "digest", "dream"}
	if got := chat.payload["roles"]; !reflect.DeepEqual(got, wantRoles) {
		t.Errorf("chat roles = %v, want %v", got, wantRoles)
	}
	wantMap := map[string]any{"default": map[string]any{
		"model":  "qwen3",
		"params": map[string]any{"think": false},
	}}
	if got := chat.payload["model_map"]; !reflect.DeepEqual(got, wantMap) {
		t.Errorf("chat model_map = %v, want %v", got, wantMap)
	}
	if got := chat.payload["num_ctx"]; got != 262144 {
		t.Errorf("chat num_ctx = %v, want 262144", got)
	}

	embed := rows[seedEmbedName]
	wantEmbedRoles := []string{"embed", "dream-embed"}
	if got := embed.payload["roles"]; !reflect.DeepEqual(got, wantEmbedRoles) {
		t.Errorf("embed roles = %v, want %v", got, wantEmbedRoles)
	}
	if got := embed.payload["protocol"]; got != "openai" {
		t.Errorf("embed protocol = %v, want openai", got)
	}
	if _, ok := embed.payload["num_ctx"]; ok {
		t.Error("embed num_ctx must be absent when unset (0 means 'server default', not 'zero context')")
	}
	// The payload must survive the wire unchanged — it travels as manage data.
	if _, err := json.Marshal(embed.payload); err != nil {
		t.Fatalf("payload not marshalable: %v", err)
	}
}

func TestSeedRowsPlanSecretForAPIKey(t *testing.T) {
	rows := mustRows(t, seedSpec{
		Chat:  seedBackend{Host: "https://api.example.com", Protocol: "openai", Model: "big", APIKey: "sk-live"},
		Embed: seedBackend{Host: "http://gpu:11434", Model: "qwen3-embed", APIKeyRef: "existing-ref"},
	})
	chat := rows[seedChatName]
	if chat.secretName != seedChatName+"-key" || chat.secretValue != "sk-live" {
		t.Errorf("chat secret = %q/%q, want %q/sk-live", chat.secretName, chat.secretValue, seedChatName+"-key")
	}
	if got := chat.payload["api_key_ref"]; got != seedChatName+"-key" {
		t.Errorf("chat api_key_ref = %v, want %s-key", got, seedChatName)
	}
	if strings.Contains(string(mustJSON(t, chat.payload)), "sk-live") {
		t.Error("plaintext api_key leaked into the backend-create payload — it must only travel to /api/secrets")
	}
	embed := rows[seedEmbedName]
	if embed.secretName != "existing-ref" || embed.secretValue != "" {
		t.Errorf("embed secret = %q/%q, want existing-ref with no value to write", embed.secretName, embed.secretValue)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestSeedRowsRejectBadSpecs(t *testing.T) {
	think := true
	ok := seedBackend{Host: "http://gpu:11434", Model: "m"}
	cases := []struct {
		name string
		spec seedSpec
		want string
	}{
		{"chat host missing", seedSpec{Chat: seedBackend{Model: "m"}, Embed: ok}, "chat: host is required"},
		{"embed model missing", seedSpec{Chat: ok, Embed: seedBackend{Host: "http://x"}}, "embed: model is required"},
		{"bad protocol", seedSpec{Chat: seedBackend{Host: "http://x", Model: "m", Protocol: "grpc"}, Embed: ok}, "protocol"},
		{"bad trust", seedSpec{Chat: seedBackend{Host: "http://x", Model: "m", Trust: "sort-of"}, Embed: ok}, "trust"},
		{"key and ref", seedSpec{Chat: seedBackend{Host: "http://x", Model: "m", APIKey: "a", APIKeyRef: "b"}, Embed: ok}, "mutually exclusive"},
		{"negative num_ctx", seedSpec{Chat: seedBackend{Host: "http://x", Model: "m", NumCtx: -1}, Embed: ok}, "num_ctx"},
		{"think on embed", seedSpec{Chat: ok, Embed: seedBackend{Host: "http://x", Model: "m", Think: &think}}, "think"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSeedRows(tc.spec)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil — a bad spec must never reach the pool", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSeedRowsHonorNameOverride(t *testing.T) {
	rows := mustRows(t, seedSpec{
		Chat:  seedBackend{Name: "spark-chat", Host: "http://spark:30000", Model: "qwen3", Protocol: "openai"},
		Embed: seedBackend{Name: "spark-embed", Host: "http://spark:30001", Model: "qwen3-embed", Protocol: "openai"},
	})
	if _, ok := rows["spark-chat"]; !ok {
		t.Error("name override ignored — an export round-trip could not reproduce its own row names")
	}
	if _, ok := rows[seedChatName]; ok {
		t.Error("default name used despite an explicit name")
	}
}

func TestLoadSeedSpecShorthand(t *testing.T) {
	spec, err := loadSeedSpec(nil, "", "http://gpu:11434", "qwen3", "qwen3-embed")
	if err != nil {
		t.Fatalf("shorthand: %v", err)
	}
	if spec.Chat.Host != "http://gpu:11434" || spec.Embed.Host != "http://gpu:11434" {
		t.Errorf("--host must feed both legs, got %q/%q", spec.Chat.Host, spec.Embed.Host)
	}
	if spec.Chat.Model != "qwen3" || spec.Embed.Model != "qwen3-embed" {
		t.Errorf("models = %q/%q", spec.Chat.Model, spec.Embed.Model)
	}
	if _, err := loadSeedSpec(nil, "", "http://gpu:11434", "qwen3", ""); err == nil {
		t.Error("an incomplete shorthand must be refused, not half-applied")
	}
	if _, err := loadSeedSpec(nil, "", "", "", ""); err == nil {
		t.Error("an empty invocation must name the three input ways")
	}
}

func TestLoadSeedSpecFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.json")
	body := `{"chat":{"host":"http://gpu:11434","model":"qwen3"},"embed":{"host":"http://gpu:11434","model":"e"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec, err := loadSeedSpec(nil, path, "", "", "")
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if spec.Chat.Model != "qwen3" {
		t.Errorf("chat model = %q", spec.Chat.Model)
	}
	if _, err := loadSeedSpec(nil, path, "http://other", "", ""); err == nil {
		t.Error("--file plus shorthand must be refused — the effective spec would be unreadable from the command line")
	}
	bad := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(bad, []byte(`{"chat":{"hosts":"http://x","model":"m"},"embed":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadSeedSpec(nil, bad, "", "", ""); err == nil {
		t.Error("an unknown field must fail the parse — a typo would silently seed an incomplete row")
	}
}

func TestSeedSealboxUnavailableNamesTheFix(t *testing.T) {
	err := seedSealboxUnavailable([]byte(`{"success":false,"error":"secrets unavailable: sealbox: CTX_SECRETS_KEY is empty"}`))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"CTX_SECRETS_KEY", "openssl rand -hex 32", "docs/operations.md#backends", "Nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort message must mention %q; got: %v", want, err)
		}
	}
}

func TestWithOrphanSecretNoteNamesLeftovers(t *testing.T) {
	base := errorString("api_key_ref missing")
	if got := withOrphanSecretNote(base, nil); !errors.Is(got, base) || got.Error() != base.Error() {
		t.Errorf("nothing sealed must leave the error untouched, got %v", got)
	}
	got := withOrphanSecretNote(base, []string{"chat-primary-key"})
	if !strings.Contains(got.Error(), "chat-primary-key") || !strings.Contains(got.Error(), "ctx secrets rm") {
		t.Errorf("the abort must name the leftover secret and how to remove it, got: %v", got)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
