package main

// Boot-contract suite (F1-W7) — successor of the W1 golden-equivalence test.
// The frozen pre-F1 reference implementation and the DeepEqual bridge proof
// died with the legacy Config struct; what this file keeps is everything the
// equivalence matrix pinned that no other test pins:
//
//   - the registry DEFAULTS as concrete values (the frozen pre-F1 fallbacks
//     became the registry default tags — drift here is a behavior change),
//   - the env-var → field WIRING for every var (a registry tag typo would
//     parse fine and land on the wrong field),
//   - the fatal semantics through env names (strict parse → SeverityError on
//     the exact field; main exits on HasErrors),
//   - the boot-level pins of the SURVIVING cross-field checks (V2, V9 family)
//     — the remainder of the Delta-6 pins after the tuple cut train took the
//     V1/V4/V7/V12 fixtures with the backend tuples they read (see
//     TestBootCrossFieldChecksPinned); the "old code boots" red-half of that
//     proof was carried by the reference implementation and is retired with
//     it — design/01-config-core.md §4 Delta 6 documents the delta,
//   - Delta 4: CTX_EMBED_DIMS is retired — no loader reads it.
//
// Fixture hygiene: documentation values only (RFC-2606 hosts, RFC-5737 IPs,
// synthetic keys) — the repo is public.

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

// resetAllEnv neutralizes every env var the loader reads. The registry is
// the authoritative list.
func resetAllEnv(t *testing.T) {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
}

func applyEnv(t *testing.T, env map[string]string) {
	t.Helper()
	resetAllEnv(t)
	if _, ok := env["CONTEXT_DB_PASSWORD"]; !ok {
		env["CONTEXT_DB_PASSWORD"] = "test-password-123"
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func hasErrorOn(issues []config.Issue, field string) bool {
	for _, is := range issues {
		if is.Field == field && is.Severity == config.SeverityError {
			return true
		}
	}
	return false
}

// loadFromEnv is the exact boot parse main runs: FromEnv + Validate appended.
func loadFromEnv() (*config.Config, []config.Issue) {
	cc, issues := config.FromEnv()
	issues = append(issues, config.Validate(cc)...)
	return cc, issues
}

// TestBootDefaults pins every registry default as a concrete value. These
// literals are the frozen pre-F1 fallbacks (golden equivalence, W1) — a
// drifting default tag is a silent behavior change for every deployment that
// relies on unset vars, so it must fail here first.
func TestBootDefaults(t *testing.T) {
	applyEnv(t, map[string]string{})

	cc, issues := loadFromEnv()
	if len(issues) != 0 {
		t.Fatalf("defaults must parse + validate clean, got %v", issues)
	}

	// Server (password is the injected required value).
	if cc.Server.DB != "context_store" || cc.Server.DBUser != "context_user" ||
		cc.Server.DBHost != "localhost" || cc.Server.DBPort != 5432 ||
		cc.Server.DBSSL != "disable" || cc.Server.ListenAddr != ":8080" {
		t.Errorf("server defaults drifted: %+v", cc.Server)
	}
	if cc.Server.ListenAddr != defaultListenAddr {
		t.Errorf("registry default %q != -health mode fallback %q", cc.Server.ListenAddr, defaultListenAddr)
	}

	// The chat block pinned the six config-side defaults of the primary
	// chat/synthesis tuple (chat.host/api_key/model/num_ctx/protocol/think). It
	// died with ChatConfig in β8 — the LAST backend tuple with config-side
	// defaults, after the fallback tuple in β4, dream_embed in β5, dream in β6
	// and embed in β7. There is no successor assertion here because there is no
	// successor DEFAULT: which host, model and context window serve a role is a
	// pool question now, and a context_backends row carries its own base_url,
	// model_map and num_ctx. No registry key is left to drift.

	// Dream + back-off (the dream chat tuple left the registry in β6).
	if cc.Dream.Enabled ||
		cc.Dream.IdleWait != 20*time.Second || cc.Dream.Parallelism != 1 {
		t.Errorf("dream defaults drifted: %+v", cc.Dream)
	}
	if cc.Dream.Backoff.Mode != "exp" || cc.Dream.Backoff.Factor != 1.6 ||
		cc.Dream.Backoff.Grace != 0 || cc.Dream.Backoff.CapHours != config.Hours(45*24) ||
		cc.Dream.Backoff.MinHours != config.Hours(12) || cc.Dream.Backoff.InertOffset != 7 {
		t.Errorf("backoff defaults drifted: %+v", cc.Dream.Backoff)
	}

	// Rerank: stage off, sweep knobs. The dispatch fields (host/api_key/model)
	// retired with the tuple in β3 — which reranker runs is a pool question.
	if cc.Rerank.Enabled || cc.Rerank.MaxDocs != 50 || cc.Rerank.BlendWeight != 1.0 {
		t.Errorf("rerank defaults drifted: %+v", cc.Rerank)
	}

	// Graph: stage off by default — byte-identical pipeline when unset.
	g := cc.Graph
	if g.Enabled || !g.Directed || g.HopDepth != 1 || g.SeedCount != 5 ||
		g.SeedScoreFloor != 0.5 || g.PerSeedCap != 3 || g.MaxInjected != 10 ||
		g.MinConfidence != 0.75 || g.MinConfidenceRecurrent != 0.8 ||
		g.BoostWeight != 0.20 || !g.HubDamping ||
		g.WeightTopical != 0.5 || g.WeightFactual != 0.9 || g.WeightCausal != 0.9 ||
		g.WeightRecurrent != 1.0 || g.NewPlacementFrac != 0.6 {
		t.Errorf("graph defaults drifted: %+v", g)
	}

	// Query: synthesis trio (frozen pre-W2 llm-init fallbacks), UTC, limits.
	if cc.Query.ScoreThreshold != 0.001 || cc.Query.ConfidentThreshold != 0.008 ||
		cc.Query.PromptVersion != "v5.2" || cc.Query.Timezone != time.UTC ||
		cc.Query.RateLimitWrite != 100 || cc.Query.RateLimitRead != 0 {
		t.Errorf("query defaults drifted: %+v", cc.Query)
	}

	// Scheduler scopes.
	if len(cc.Scheduler.ReadScopes) != 3 || cc.Scheduler.ReadScopes[0] != "private" ||
		cc.Scheduler.ReadScopes[1] != "shared" || cc.Scheduler.ReadScopes[2] != "work" ||
		cc.Scheduler.HomeScope != "private" {
		t.Errorf("scheduler defaults drifted: %+v", cc.Scheduler)
	}
}

// TestBootEnvWiring sets every env var to a non-default value and asserts it
// lands on its field. This is the runtime proof behind the registry tag
// table: a swapped env tag would still parse and validate clean.
func TestBootEnvWiring(t *testing.T) {
	applyEnv(t, map[string]string{
		"CONTEXT_DB": "ctxtest", "CONTEXT_DB_USER": "ctxuser",
		"CONTEXT_DB_PASSWORD": "test-password-123",
		"CONTEXT_DB_HOST":     "db.example", "CONTEXT_DB_PORT": "5433",
		"CONTEXT_DB_SSLMODE": "require", "LISTEN_ADDR": ":9090",

		// The six CTX_CHAT_* tuple vars left the env surface with the registry
		// cut in β8 — the last of the six backend tuples. Their ABSENCE is
		// gated separately and mechanically: internal/config/retired_test.go's
		// TestRetiredEnvNamesFollowTheCut derives each cut key's CTX_* name and
		// fails if it is still in EnvVars(). Wiring them here would prove
		// nothing — an unread name parses onto no field at all.

		"CTX_RERANK_ENABLED":  "true",
		"CTX_RERANK_MAX_DOCS": "40", "CTX_RERANK_BLEND_WEIGHT": "0.5",

		"CTX_GRAPH_EXPAND_ENABLED": "true", "CTX_GRAPH_EXPAND_DIRECTED": "false",
		"CTX_GRAPH_EXPAND_HOP_DEPTH": "2", "CTX_GRAPH_EXPAND_SEED_COUNT": "7",
		"CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR": "0.4", "CTX_GRAPH_EXPAND_PER_SEED_CAP": "4",
		"CTX_GRAPH_EXPAND_MAX_INJECTED": "12", "CTX_GRAPH_EXPAND_MIN_CONFIDENCE": "0.7",
		"CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT": "0.85",
		"CTX_GRAPH_EXPAND_BOOST_WEIGHT":             "0.25",
		"CTX_GRAPH_EXPAND_HUB_DAMPING":              "false",
		"CTX_GRAPH_EXPAND_WEIGHT_TOPICAL":           "0.6",
		"CTX_GRAPH_EXPAND_WEIGHT_FACTUAL":           "0.8",
		"CTX_GRAPH_EXPAND_WEIGHT_CAUSAL":            "0.85",
		"CTX_GRAPH_EXPAND_WEIGHT_RECURRENT":         "0.95",
		"CTX_GRAPH_EXPAND_NEW_PLACEMENT_FRAC":       "0.45",

		"CTX_DREAM_ENABLED": "true",
		// The six CTX_DREAM_* tuple vars left the env surface with the registry
		// cut in β6 — retired_test.go asserts their absence from EnvVars().
		"CTX_DREAM_IDLE_WAIT": "30", "CTX_DREAM_PARALLELISM": "4",
		"CTX_DREAM_BACKOFF_MODE": "log", "CTX_DREAM_BACKOFF_FACTOR": "2.5",
		"CTX_DREAM_BACKOFF_GRACE": "3", "CTX_DREAM_BACKOFF_CAP": "30d",
		"CTX_DREAM_BACKOFF_MIN": "6h", "CTX_DREAM_BACKOFF_INERT_OFFSET": "5",

		"CTX_TIMEZONE":         "Europe/Berlin",
		"CTX_RATE_LIMIT_WRITE": "200", "CTX_RATE_LIMIT_READ": "50",
		"CTX_READ_SCOPES": "private,shared,work,crag",

		"CTX_SCORE_THRESHOLD": "0.002", "CTX_CONFIDENT_THRESHOLD": "0.01",
		"CTX_PROMPT_VERSION": "v6",
	})

	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("wiring fixture must validate clean, got %v", issues)
	}

	if cc.Server.DB != "ctxtest" || cc.Server.DBUser != "ctxuser" ||
		cc.Server.DBPass != "test-password-123" || cc.Server.DBHost != "db.example" ||
		cc.Server.DBPort != 5433 || cc.Server.DBSSL != "require" ||
		cc.Server.ListenAddr != ":9090" {
		t.Errorf("server wiring: %+v", cc.Server)
	}
	// The chat wiring assertion (six CTX_CHAT_* names → Config.Chat fields)
	// died with ChatConfig in β8: no field, no env tag, nothing to mis-wire.
	// The env names themselves are gated by absence in retired_test.go
	// (TestRetiredEnvNamesFollowTheCut), see the fixture above.
	if !cc.Rerank.Enabled || cc.Rerank.MaxDocs != 40 || cc.Rerank.BlendWeight != 0.5 {
		t.Errorf("rerank wiring: %+v", cc.Rerank)
	}
	g := cc.Graph
	if !g.Enabled || g.Directed || g.HopDepth != 2 || g.SeedCount != 7 ||
		g.SeedScoreFloor != 0.4 || g.PerSeedCap != 4 || g.MaxInjected != 12 ||
		g.MinConfidence != 0.7 || g.MinConfidenceRecurrent != 0.85 ||
		g.BoostWeight != 0.25 || g.HubDamping ||
		g.WeightTopical != 0.6 || g.WeightFactual != 0.8 || g.WeightCausal != 0.85 ||
		g.WeightRecurrent != 0.95 || g.NewPlacementFrac != 0.45 {
		t.Errorf("graph wiring: %+v", g)
	}
	if !cc.Dream.Enabled ||
		cc.Dream.IdleWait != 30*time.Second || cc.Dream.Parallelism != 4 {
		t.Errorf("dream wiring: %+v", cc.Dream)
	}
	if cc.Dream.Backoff.Mode != "log" || cc.Dream.Backoff.Factor != 2.5 ||
		cc.Dream.Backoff.Grace != 3 || cc.Dream.Backoff.CapHours != config.Hours(30*24) ||
		cc.Dream.Backoff.MinHours != config.Hours(6) || cc.Dream.Backoff.InertOffset != 5 {
		t.Errorf("backoff wiring: %+v", cc.Dream.Backoff)
	}
	if cc.Query.ScoreThreshold != 0.002 || cc.Query.ConfidentThreshold != 0.01 ||
		cc.Query.PromptVersion != "v6" || cc.Query.Timezone.String() != "Europe/Berlin" ||
		cc.Query.RateLimitWrite != 200 || cc.Query.RateLimitRead != 50 {
		t.Errorf("query wiring: %+v", cc.Query)
	}
	if len(cc.Scheduler.ReadScopes) != 4 || cc.Scheduler.ReadScopes[3] != "crag" {
		t.Errorf("scheduler wiring: %+v", cc.Scheduler)
	}
}

// TestBootFatalSemantics: per strict field one malformed env fixture must
// yield a SeverityError on the exact field — main refuses to start on
// HasErrors, preserving the pre-F1 fatal-parse semantics.
func TestBootFatalSemantics(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		field string
	}{
		{"db port", map[string]string{"CONTEXT_DB_PORT": "not_a_port"}, "server.db_port"},
		// The three num_ctx fixtures left with their tuples — dream in β6,
		// embed in β7, chat in β8. CTX_*_NUM_CTX is no longer read by any
		// loader, so a malformed value has no parse path left to be fatal on;
		// a backend row's num_ctx is validated at the pool write path instead.
		// The parse:"strict" CLASS this table pins is unaffected: 33 registry
		// entries still carry the tag (measured over registry(), not grepped —
		// config.go has ten further prose mentions, two of them saying NOT
		// strict), and CONTEXT_DB_PORT (two rows here, plain and whitespace)
		// exercises the same code path the chat row did.
		{"rate limit write", map[string]string{"CTX_RATE_LIMIT_WRITE": "abc"}, "query.rate_limit_write"},
		{"rate limit read", map[string]string{"CTX_RATE_LIMIT_READ": "abc"}, "query.rate_limit_read"},
		{"timezone", map[string]string{"CTX_TIMEZONE": "Nope/Nowhere"}, "query.timezone"},
		{"whitespace int", map[string]string{"CONTEXT_DB_PORT": " 5432 "}, "server.db_port"},
		{"required password", map[string]string{"CONTEXT_DB_PASSWORD": ""}, "server.db_password"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyEnv(t, c.env)
			_, issues := loadFromEnv()
			if !hasErrorOn(issues, c.field) {
				t.Errorf("want SeverityError on %s, got %v", c.field, issues)
			}
			if !config.HasErrors(issues) {
				t.Error("HasErrors must be true — main exits on this")
			}
		})
	}
}

// TestBootSafeFieldsWarn pins Delta 3: malformed values on parse-safe fields
// keep the default (pre-F1: silent) and are VISIBLE as one WARN per field.
func TestBootSafeFieldsWarn(t *testing.T) {
	applyEnv(t, map[string]string{"CTX_RERANK_MAX_DOCS": "abc"})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("safe-malformed must not be fatal: %v", issues)
	}
	if cc.Rerank.MaxDocs != 50 {
		t.Errorf("safe-malformed must keep default 50, got %d", cc.Rerank.MaxDocs)
	}
	for _, is := range issues {
		if is.Field == "rerank.max_docs" && is.Severity == config.SeverityWarn {
			return
		}
	}
	t.Errorf("expected WARN on rerank.max_docs, got %v", issues)
}

// TestBootCrossFieldChecksPinned pins the SURVIVING cross-field checks of
// config.Validate at boot level: a malformed combination must abort with a
// SeverityError naming the field, so no class can silently degrade to WARN.
//
// This was TestBootDelta6Pinned and pinned "§4 Delta 6" as a whole — configs
// that BOOTED before F1 while already being broken at runtime. Delta 6 is no
// longer a thing this test can pin, because most of its fixtures rode on
// backend tuples that have left the registry:
//
//   - V4 (protocol typo) and V7 (host parseable / http(s) / no trailing slash
//     / no userinfo) — five rows here — died with validateBackendTuples and
//     validateHostURL in β8, the wave that cut the chat tuple. They are not
//     "lost": their protective content moved to the pool write path, where
//     hosts and protocols are configured now. backends/validate.go
//     validateIdentity makes userinfo in base_url a 422 FieldError and rejects
//     any protocol outside openai/ollama/rerank.
//   - V12 retired with the dream_embed tuple in β5, V1 with the dream chat
//     tuple in β6: both compared fields of two tuples, and which rows serve
//     which role is a pool question that Validate — parameter-pure, it never
//     sees the pool — cannot ask (design/01 §5.5 rules V1 out by name).
//
// What remains are the checks that never read a backend tuple: V2 (threshold
// ordering) and the V9 range family. Those are alive in validate.go, so the
// rows stay — dropping them with the dead ones would be an unforced coverage
// loss.
func TestBootCrossFieldChecksPinned(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		field string
	}{
		{"V2 inverted thresholds", map[string]string{
			"CTX_SCORE_THRESHOLD": "0.5", "CTX_CONFIDENT_THRESHOLD": "0.008",
		}, "query.score_threshold"},
		{"V9 blend weight range", map[string]string{"CTX_RERANK_BLEND_WEIGHT": "1.5"}, "rerank.blend_weight"},
		{"V9 hop depth zero", map[string]string{"CTX_GRAPH_EXPAND_HOP_DEPTH": "0"}, "graph.hop_depth"},
		{"V9 negative rate limit", map[string]string{"CTX_RATE_LIMIT_WRITE": "-1"}, "query.rate_limit_write"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyEnv(t, c.env)
			_, issues := loadFromEnv()
			if !hasErrorOn(issues, c.field) {
				t.Errorf("want SeverityError on %s, got %v", c.field, issues)
			}
			// The leak-hygiene assertion (an issue message must not echo the
			// password out of a userinfo host — (*url.Error).Error() embeds the
			// raw URL) went with the V7 fixture it rode on: no surviving row
			// feeds Validate a URL, so nothing here can leak one. The same
			// obligation is met at the pool boundary by construction —
			// validateIdentity's base_url message is a constant and derives
			// nothing from the input (backends/validate.go).
		})
	}
}

// TestEmbedDimsRetired pins Delta 4: CTX_EMBED_DIMS is dead. The registry
// never reads it (the real dimension is the embed.TargetDims constant), and
// since F1-W7 no bridge parse exists either — a set value is inert: no
// field, no issue, no fatal.
func TestEmbedDimsRetired(t *testing.T) {
	for _, v := range config.EnvVars() {
		if v == "CTX_EMBED_DIMS" {
			t.Fatal("CTX_EMBED_DIMS must not re-enter the registry (Delta 4)")
		}
	}
	applyEnv(t, map[string]string{"CTX_EMBED_DIMS": "not-even-a-number"})
	_, issues := loadFromEnv()
	if len(issues) != 0 {
		t.Errorf("CTX_EMBED_DIMS must be inert, got issues: %v", issues)
	}
}
