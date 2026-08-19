package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
)

// Prompt versions accepted by V5. Mirrors llm.PromptVersionV52/V6 — kept as
// literals here because the layering rule forbids importing config from llm
// and vice versa would couple the domain package to config.
const (
	promptVersionV52 = "v5.2"
	promptVersionV6  = "v6"
)

// V14 shape gate for dream.language (validateDream). The tag is the ONLY
// config value that reaches an LLM system prompt verbatim, so the accepted
// alphabet is deliberately narrower than full BCP-47: primary subtag +
// optional alphanumeric subtags, nothing else — no spaces, no quotes, no
// newlines, no non-ASCII. The length cap bounds the interpolation.
const (
	dreamLanguagePattern = `^[a-z]{2,3}(-[a-z0-9]{2,8})*$`
	dreamLanguageMaxLen  = 35
)

var dreamLanguageRe = regexp.MustCompile(dreamLanguagePattern)

// Validate checks the cross-field invariants V1–V14 and returns all findings.
// WARN classes with "today's silent fallback" semantics (V5 prompt version,
// V6 back-off, V10 parallelism clamp) NORMALIZE the config in place — exactly
// what llm's init(), dream.SetBackoffConfig and the scheduler clamp did
// downstream until now, made visible. Callers: boot aborts on HasErrors after
// logging everything; Store.Replace rejects.
func Validate(c *Config) []Issue {
	var issues []Issue
	issues = append(issues, validateBackendTuples(c)...) // V1, V4, V7, V12
	issues = append(issues, validateQuery(c)...)         // V2, V5, V11
	issues = append(issues, validateRerankGraph(c)...)   // V3, V8, V9, V13
	issues = append(issues, validateDream(c)...)         // V6, V10, V14
	return issues
}

func validateBackendTuples(c *Config) []Issue {
	var issues []Issue

	// V7 — host URLs: parseable, http(s), no trailing slash (would double on
	// path join), no userinfo (would bypass the field-name-based secret
	// masking everywhere hosts flow: dump, error logs, F2 API).
	for _, h := range []struct{ key, host string }{
		{"chat.host", c.Chat.Host},
		{"chat_fallback.host", c.Fallback.Host},
		{"embed.host", c.Embed.Host},
		{"dream.host", c.Dream.Host},
		{"dream_embed.host", c.Dream.Embed.Host},
		{"rerank.host", c.Rerank.Host},
	} {
		if h.host == "" {
			continue
		}
		issues = append(issues, validateHostURL(h.key, h.host)...)
	}

	// V4 — protocol typos fell silently onto the ollama wire path until now
	// (chatWithFormat: != "openai" ⇒ ollama) → 404 on llama.cpp. Delta 6.
	for _, p := range []struct {
		key        string
		proto      backends.Protocol
		allowEmpty bool // dream_embed inherits when empty
	}{
		{"chat.protocol", c.Chat.Protocol, false},
		{"chat_fallback.protocol", c.Fallback.Protocol, false},
		{"embed.protocol", c.Embed.Protocol, false},
		{"dream.protocol", c.Dream.Protocol, false},
		{"dream_embed.protocol", c.Dream.Embed.Protocol, true},
	} {
		if p.allowEmpty && p.proto == "" {
			continue
		}
		if p.proto != backends.ProtocolOllama && p.proto != backends.ProtocolOpenAI {
			issues = append(issues, Issue{Field: p.key, Severity: SeverityError,
				Msg: fmt.Sprintf("unknown protocol %q — must be %q or %q (typos silently selected the ollama wire path before F1)",
					string(p.proto), backends.ProtocolOllama, backends.ProtocolOpenAI)})
		}
	}

	// V1 — chat and dream with explicitly different num_ctx on the same host:
	// under ollama that means two runner instances of the same model (VRAM
	// OOM); under openai the parameter is discarded on the wire → WARN only.
	if c.Dream.NumCtx > 0 && c.Chat.NumCtx > 0 && c.Dream.NumCtx != c.Chat.NumCtx &&
		c.Dream.Host == c.Chat.Host {
		sev := SeverityWarn
		if c.Dream.Protocol == backends.ProtocolOllama {
			sev = SeverityError
		}
		issues = append(issues, Issue{Field: "dream.num_ctx", Severity: sev,
			Msg: fmt.Sprintf("dream num_ctx %d != chat num_ctx %d on the same host — two runner instances of one model (VRAM OOM under ollama)",
				c.Dream.NumCtx, c.Chat.NumCtx)})
	}

	// V12 — credential boundary of the dream-embed inheritance: with a
	// foreign dream-embed host and no own key, the field-by-field fallback
	// would send the embed credential as Bearer to the FOREIGN host on every
	// keyword embed and backfill, silently.
	if c.Dream.Embed.Host != "" && c.Dream.Embed.Host != c.Embed.Host &&
		c.Dream.Embed.APIKey == "" && c.Embed.APIKey != "" {
		issues = append(issues, Issue{Field: "dream_embed.api_key", Severity: SeverityError,
			Msg: "credential would be inherited across hosts — set dream_embed.api_key explicitly (F2: secret_ref) or leave dream_embed.host empty"})
	}

	return issues
}

// validateHostURL implements the V7 checks for one host field. On url.Parse
// FAILURE the error text is never embedded — (*url.Error).Error() carries the
// raw input including any userinfo password (§3.5).
func validateHostURL(key, host string) []Issue {
	u, err := url.Parse(host)
	if err != nil {
		return []Issue{{Field: key, Severity: SeverityError,
			Msg: "host URL is not parseable (raw value withheld — it may embed credentials)"}}
	}
	var issues []Issue
	if u.Scheme != "http" && u.Scheme != "https" {
		issues = append(issues, Issue{Field: key, Severity: SeverityError,
			Msg: fmt.Sprintf("host URL %s: scheme must be http or https", u.Redacted())})
	}
	if strings.HasSuffix(host, "/") {
		issues = append(issues, Issue{Field: key, Severity: SeverityError,
			Msg: fmt.Sprintf("host URL %s: trailing slash doubles on path join — drop it", u.Redacted())})
	}
	if u.User != nil {
		issues = append(issues, Issue{Field: key, Severity: SeverityError,
			Msg: "credentials in host URL not allowed — use api_key (F2: secret_ref)"})
	}
	return issues
}

func validateQuery(c *Config) []Issue {
	var issues []Issue

	// V11 — required check, moved from LoadConfig. Stays fatal.
	if c.Server.DBPass == "" {
		issues = append(issues, Issue{Field: "server.db_password", Severity: SeverityError,
			Msg: "CONTEXT_DB_PASSWORD is required"})
	}

	// V2 — inverted thresholds make "low_confidence" unreachable
	// (ClassifyConfidence checks confident first, then score).
	if c.Query.ScoreThreshold > c.Query.ConfidentThreshold {
		issues = append(issues, Issue{Field: "query.score_threshold", Severity: SeverityError,
			Msg: fmt.Sprintf("score_threshold %g > confident_threshold %g makes low_confidence unreachable",
				c.Query.ScoreThreshold, c.Query.ConfidentThreshold)})
	}

	// V5 — today's llm init() semantics: log + fall back to v5.2.
	if c.Query.PromptVersion != promptVersionV52 && c.Query.PromptVersion != promptVersionV6 {
		issues = append(issues, Issue{Field: "query.prompt_version", Severity: SeverityWarn,
			Msg: fmt.Sprintf("unknown prompt version %q — using %q", c.Query.PromptVersion, promptVersionV52)})
		c.Query.PromptVersion = promptVersionV52
	}

	// V9 (rate-limit part) — negative limits are range garbage.
	for _, r := range []struct {
		key string
		val int
	}{
		{"query.rate_limit_write", c.Query.RateLimitWrite},
		{"query.rate_limit_read", c.Query.RateLimitRead},
		{"pool.blob_rate_limit_write", c.Pool.BlobRateLimitWrite},
	} {
		if r.val < 0 {
			issues = append(issues, Issue{Field: r.key, Severity: SeverityError,
				Msg: fmt.Sprintf("rate limit %d must be >= 0", r.val)})
		}
	}

	// V9b (H12) — a negative context-window fallback is range garbage in the
	// one direction that matters: ChainRuneBudget reads <= 0 as "unset" and
	// refuses, so a negative value would silently mean "off" while reading as
	// a configured number in the settings surface.
	if c.Pool.ExternalNumCtxFallback < 0 {
		issues = append(issues, Issue{Field: "pool.external_num_ctx_fallback", Severity: SeverityError,
			Msg: fmt.Sprintf("context window fallback %d must be >= 0 (0 = unset)", c.Pool.ExternalNumCtxFallback)})
	}

	// V9c (E10-W2) — same shape as V9b for the same reason: the discovery TTL
	// reads <= 0 as "discovery off", so a negative value would silently mean
	// off while presenting as a configured duration.
	if c.Pool.OpenRouterWindowTTL < 0 {
		issues = append(issues, Issue{Field: "pool.openrouter_window_ttl", Severity: SeverityError,
			Msg: fmt.Sprintf("endpoint discovery TTL %d must be >= 0 (0 = off)", c.Pool.OpenRouterWindowTTL)})
	}

	return issues
}

func validateRerankGraph(c *Config) []Issue {
	var issues []Issue

	// V9 — range garbage produces silent scoring chaos, not a crash.
	if !(c.Rerank.BlendWeight >= 0 && c.Rerank.BlendWeight <= 1) { // NaN-safe
		issues = append(issues, Issue{Field: "rerank.blend_weight", Severity: SeverityError,
			Msg: fmt.Sprintf("blend_weight %g must be in [0,1]", c.Rerank.BlendWeight)})
	}
	if c.Rerank.MaxDocs < 0 {
		issues = append(issues, Issue{Field: "rerank.max_docs", Severity: SeverityError,
			Msg: fmt.Sprintf("max_docs %d must be >= 0", c.Rerank.MaxDocs)})
	}
	if c.Graph.HopDepth < 1 {
		issues = append(issues, Issue{Field: "graph.hop_depth", Severity: SeverityError,
			Msg: fmt.Sprintf("hop_depth %d must be >= 1", c.Graph.HopDepth)})
	}
	for _, w := range []struct {
		key string
		val float64
	}{
		{"graph.boost_weight", c.Graph.BoostWeight},
		{"graph.weight_topical", c.Graph.WeightTopical},
		{"graph.weight_factual", c.Graph.WeightFactual},
		{"graph.weight_causal", c.Graph.WeightCausal},
		{"graph.weight_recurrent", c.Graph.WeightRecurrent},
	} {
		if !(w.val >= 0 && w.val <= 1) { // NaN-safe
			issues = append(issues, Issue{Field: w.key, Severity: SeverityError,
				Msg: fmt.Sprintf("graph weight %g must be in [0,1]", w.val)})
		}
	}

	// V3 — Wave-3 empiricism: the cross-encoder as final arbiter over
	// graph-injected neighbors is destructive (R@10 0.715→0.571).
	if c.Rerank.BlendWeight == 1.0 && c.Graph.Enabled {
		issues = append(issues, Issue{Field: "rerank.blend_weight", Severity: SeverityWarn,
			Msg: "blend_weight 1.0 with graph expansion enabled: pure cross-encoder order overrides graph-injected neighbors (Wave-3: destructive) — consider 0.5"})
	}

	// V8 — legal LLM-judge path, but it runs without the body heartbeat:
	// judge queries >60s die at the reverse proxy. Make it a conscious choice.
	if c.Rerank.Enabled && c.Rerank.Host == "" {
		issues = append(issues, Issue{Field: "rerank.host", Severity: SeverityWarn,
			Msg: "rerank enabled without host = LLM-as-judge path, which runs WITHOUT the body heartbeat — judge queries >60s die at the reverse proxy"})
	}

	// V13 — third unprotected long-runner besides V8 (deliberate double-WARN:
	// two paths, two consequences): the heartbeat gate keys on the rerank
	// config, so fallback synthesis (~391s measured) runs against the
	// ABSOLUTE 120s WriteTimeout. Structural fix = gate broadening (X4, G26).
	// Spec form: Fallback.Host != "" && !(Rerank.Enabled && Rerank.Host != "").
	if c.Fallback.Host != "" && (!c.Rerank.Enabled || c.Rerank.Host == "") {
		issues = append(issues, Issue{Field: "chat_fallback.host", Severity: SeverityWarn,
			Msg: "fallback synthesis path runs without heartbeat; responses >120s die at server WriteTimeout / >60s at reverse proxy"})
	}

	return issues
}

func validateDream(c *Config) []Issue {
	var issues []Issue

	// V6 — today's dream.SetBackoffConfig ignore semantics (dream.go:404-428),
	// pulled forward and made visible: invalid values keep the default.
	bc := &c.Dream.Backoff
	switch bc.Mode {
	case "exp", "log", "linear", "off":
	default:
		issues = append(issues, v6(bc, "dream.backoff_mode",
			fmt.Sprintf("backoff mode %q must be exp|log|linear|off", bc.Mode)))
	}
	if bc.Factor < 0 {
		issues = append(issues, v6(bc, "dream.backoff_factor",
			fmt.Sprintf("backoff factor %g must be >= 0", bc.Factor)))
	}
	if bc.Grace < 0 {
		issues = append(issues, v6(bc, "dream.backoff_grace",
			fmt.Sprintf("backoff grace %d must be >= 0", bc.Grace)))
	}
	if bc.CapHours <= 0 {
		issues = append(issues, v6(bc, "dream.backoff_cap",
			fmt.Sprintf("backoff cap %gh must be > 0", float64(bc.CapHours))))
	}
	if bc.MinHours < 0 {
		issues = append(issues, v6(bc, "dream.backoff_min",
			fmt.Sprintf("backoff min %gh must be >= 0", float64(bc.MinHours))))
	}
	if bc.InertOffset < 0 {
		issues = append(issues, v6(bc, "dream.backoff_inert_offset",
			fmt.Sprintf("backoff inert offset %d must be >= 0", bc.InertOffset)))
	}

	// V10 — today's scheduler clamp (Run: workers<1→1, >16→16), pulled into
	// Validate. The runtime clamp stays — this makes it visible at boot.
	if c.Dream.Parallelism < 1 {
		issues = append(issues, Issue{Field: "dream.parallelism", Severity: SeverityWarn,
			Msg: fmt.Sprintf("parallelism %d clamped to 1", c.Dream.Parallelism)})
		c.Dream.Parallelism = 1
	} else if c.Dream.Parallelism > 16 {
		issues = append(issues, Issue{Field: "dream.parallelism", Severity: SeverityWarn,
			Msg: fmt.Sprintf("parallelism %d clamped to 16", c.Dream.Parallelism)})
		c.Dream.Parallelism = 16
	}

	// V14 — dream.language shape. The value is INTERPOLATED into the daily
	// synthesis SYSTEM PROMPT (dream.langName), so free text here is a prompt
	// injection + prompt-length vector on a background pipeline nobody reads
	// before it runs. Normalize like every other case-insensitive key (trim +
	// lower, in place — the V-order normalization pattern above), then hard
	// ERROR on anything that is not a BCP-47-shaped tag: boot aborts, and a
	// Settings write is rejected instead of silently reaching the LLM.
	// Empty stays legal — it is the legacy-behavior default.
	c.Dream.Language = strings.ToLower(strings.TrimSpace(c.Dream.Language))
	if lang := c.Dream.Language; lang != "" {
		switch {
		case len(lang) > dreamLanguageMaxLen:
			issues = append(issues, Issue{Field: "dream.language", Severity: SeverityError,
				Msg: fmt.Sprintf("language %q is %d chars — max %d", lang, len(lang), dreamLanguageMaxLen)})
		case !dreamLanguageRe.MatchString(lang):
			issues = append(issues, Issue{Field: "dream.language", Severity: SeverityError,
				Msg: fmt.Sprintf("language %q must be a BCP-47-style tag (%s), e.g. \"de\", \"en\", \"pt-br\" — empty = legacy German report", lang, dreamLanguagePattern)})
		}
	}

	// V15 — dream.link_floor_confidence range. The value becomes the raw
	// confidence of every link the LLM names without a strength signal; an
	// out-of-range float would either die at the write gate (silent no-op
	// pipeline) or claim impossible certainty. Same fatal class as the other
	// out-of-range knobs (e.g. _BLEND_WEIGHT outside [0,1]).
	if f := c.Dream.LinkFloorConfidence; f < 0 || f > 1 {
		issues = append(issues, Issue{Field: "dream.link_floor_confidence", Severity: SeverityError,
			Msg: fmt.Sprintf("link floor confidence %g must be within [0,1]", f)})
	}

	// V16 — dream.temporal_timeout sign. Same class as V9b/V9c: the consumer
	// (dream.temporalTimeout) reads <= 0 as "unset" and silently substitutes
	// the package ValidateTimeout, so a negative value would present as a
	// configured duration in the settings surface while meaning the default.
	// 0 stays legal — it IS the documented "package default" sentinel.
	if d := c.Dream.TemporalTimeout; d < 0 {
		issues = append(issues, Issue{Field: "dream.temporal_timeout", Severity: SeverityError,
			Msg: fmt.Sprintf("temporal timeout %v must be >= 0 (0 = package default %v)", d, dream.ValidateTimeout)})
	}

	return issues
}

// v6 emits the V6 WARN for one back-off field and resets it to its registry
// default (the same value SetBackoffConfig would have kept by ignoring the
// input).
func v6(bc *BackoffConfig, key, msg string) Issue {
	def := defaultFor(key)
	switch key {
	case "dream.backoff_mode":
		bc.Mode = def.(string)
	case "dream.backoff_factor":
		bc.Factor = def.(float64)
	case "dream.backoff_grace":
		bc.Grace = def.(int)
	case "dream.backoff_cap":
		bc.CapHours = def.(Hours)
	case "dream.backoff_min":
		bc.MinHours = def.(Hours)
	case "dream.backoff_inert_offset":
		bc.InertOffset = def.(int)
	}
	return Issue{Field: key, Severity: SeverityWarn, Msg: msg + " — using default"}
}

// defaultFor returns the parsed registry default for a key.
func defaultFor(key string) any {
	for _, e := range registry() {
		if e.Key == key {
			return e.defVal
		}
	}
	panic("config: unknown registry key " + key)
}
