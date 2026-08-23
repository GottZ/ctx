package config

// Fixture hygiene (wave-1 mandate): documentation values only — RFC-2606
// hostnames (*.example), RFC-5737 IPs (192.0.2.x), synthetic keys. Never real
// deployment topology or credentials; the repo is public.

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// lookupMap adapts a map keyed by CANONICAL REGISTRY KEY to the fromSources
// signature. A present-but-empty value counts as provided (the F2 settings
// source can deliver that; FromEnv cannot).
func lookupMap(vals map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vals[key]
		return v, ok
	}
}

// cfgFrom builds a Config via fromSources from canonical keys, injecting the
// required db password unless the test sets one itself.
func cfgFrom(t *testing.T, vals map[string]string) (*Config, []Issue) {
	t.Helper()
	if _, ok := vals["server.db_password"]; !ok {
		vals["server.db_password"] = "test-password"
	}
	return fromSources(lookupMap(vals))
}

func issueFor(issues []Issue, field string) *Issue {
	for i := range issues {
		if issues[i].Field == field {
			return &issues[i]
		}
	}
	return nil
}

// --- parseCooldownHours (moved from cmd/ctxd, table preserved 1:1) ---.

func TestParseCooldownHours(t *testing.T) {
	cases := []struct {
		in    string
		want  float64
		valid bool
	}{
		{"12h", 12, true},
		{"45d", 45 * 24, true},
		{"1w", 7 * 24, true},
		{"1m", 30 * 24, true},  // month = 30d
		{"1y", 365 * 24, true}, // year = 365d
		{"2D", 2 * 24, true},   // uppercase suffix accepted
		{"36", 36, true},       // bare number = hours
		{"1.5d", 1.5 * 24, true},
		{" 12h ", 12, true},  // surrounding space tolerated
		{"", 0, false},       // empty → invalid (caller uses default)
		{"12x", 0, false},    // unknown suffix
		{"-5d", 0, false},    // negative rejected
		{"abc", 0, false},    // non-numeric
		{"12m30s", 0, false}, // Go-duration syntax NOT accepted (no minutes/seconds)
	}
	for _, c := range cases {
		got, ok := parseCooldownHours(c.in)
		if ok != c.valid {
			t.Errorf("parseCooldownHours(%q) valid=%v, want %v", c.in, ok, c.valid)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseCooldownHours(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- parseScopesValue (legacy parseScopes semantics) ---.

func TestParseScopesValue(t *testing.T) {
	def := []string{"private", "shared", "work"}
	cases := []struct {
		in      string
		want    []string
		wantErr bool // err ⇒ caller keeps default + emits WARN (legacy: silent default)
	}{
		{"crag", []string{"crag"}, false},
		{"private,shared,work,crag", []string{"private", "shared", "work", "crag"}, false},
		{"  private , shared  , work  ", []string{"private", "shared", "work"}, false},
		{"a,,b,,,c", []string{"a", "b", "c"}, false},
		{"hth,crag,project-x,team_alpha", []string{"hth", "crag", "project-x", "team_alpha"}, false},
		{" , , , ", def, true}, // no usable part → default, now with a WARN
	}
	for _, c := range cases {
		got, err := parseScopesValue(c.in, def)
		if (err != nil) != c.wantErr {
			t.Errorf("parseScopesValue(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseScopesValue(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- typed parsers ---.

func TestParseBoolExactMatch(t *testing.T) {
	// Legacy `getEnv(...) == "true"`: only the exact literal activates.
	p := parserFor(typBool)
	for in, want := range map[string]bool{
		"true": true, "TRUE": false, "1": false, "yes": false, "false": false, "banana": false,
	} {
		got, err := p(in, nil)
		if err != nil || got.(bool) != want {
			t.Errorf("bool parser(%q) = (%v, %v), want (%v, nil)", in, got, err, want)
		}
	}
}

// TestParserForUnoccupiedTypes keeps the two parser arms that lost their last
// registry carrier in β8 exercised: chat.protocol was the only typProtocol
// field, chat.think the only typThink one. Both arms stay in parserFor because
// they are generic registry vocabulary — a protocol and a think mode are what a
// backends.Backend carries, and a future key of either type must not find a nil
// parser (buildEntry rejects an unsupported field type outright, so the failure
// would be a boot panic on the next registry build).
//
// This is deliberately the LOWEST altitude that still has a subject. The
// precedence matrix in build_test.go dropped its protocol row instead of
// substituting one, because there is no key to substitute; here the parser is
// the subject and needs none.
func TestParserForUnoccupiedTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		in   string
		want any
	}{
		{"protocol", typProtocol, "openai", backends.Protocol("openai")},
		{"protocol passes typos through", typProtocol, "olama", backends.Protocol("olama")},
		{"think", typThink, "true", backends.ThinkMode("true")},
		{"think empty", typThink, "", backends.ThinkMode("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parserFor(tc.typ)
			if p == nil {
				t.Fatalf("parserFor(%v) is nil — a key of this type would fail buildEntry", tc.typ)
			}
			got, err := p(tc.in, nil)
			if err != nil || got != tc.want {
				t.Errorf("parser(%q) = (%v, %v), want (%v, nil)", tc.in, got, err, tc.want)
			}
		})
	}
	// Both parsers are deliberately TOLERANT: they convert without validating,
	// which is why V4 had to exist at all and why its successor lives at the
	// pool write path (backends/validate.go validateIdentity) rather than here.
	// Asserting the tolerance is what keeps that reasoning checkable.
}

func TestParseDurationSeconds(t *testing.T) {
	if v, err := parseDurationSeconds("420", nil); err != nil || v.(time.Duration) != 420*time.Second {
		t.Errorf("parseDurationSeconds(420) = (%v, %v), want 7m0s", v, err)
	}
	if _, err := parseDurationSeconds("4.2", nil); err == nil {
		t.Error("parseDurationSeconds should reject non-integer input (legacy getEnvIntSafe semantics)")
	}
	// Negative passes the parser — exactly like the legacy strconv.Atoi paths.
	// The sign is V17's job in Validate, where it is fatal for EVERY duration
	// key; rejecting it here would degrade 33 of the 37 to WARN + default.
	if v, err := parseDurationSeconds("-5", nil); err != nil || v.(time.Duration) != -5*time.Second {
		t.Errorf("parseDurationSeconds(-5) = (%v, %v), want -5s (legacy equivalence)", v, err)
	}
}

// TestParseDurationSecondsOverflow pins the range guard that only the PARSER
// can hold (issue #29): `n * time.Second` wraps for |n| > maxDurationSeconds,
// and the wrap lands on a plausible SMALL duration — 9223372036854 s renders
// as -775.808ms, 9223372036855 s as 224.192ms — so no downstream range check,
// V17 included, can tell the result from a configured value. A rejected input
// must therefore never produce a Duration at all.
func TestParseDurationSecondsOverflow(t *testing.T) {
	if v, err := parseDurationSeconds("9223372036", nil); err != nil || v.(time.Duration) != 9223372036*time.Second {
		t.Errorf("parseDurationSeconds(9223372036) = (%v, %v), want the largest representable value", v, err)
	}
	if v, err := parseDurationSeconds("-9223372036", nil); err != nil || v.(time.Duration) != -9223372036*time.Second {
		t.Errorf("parseDurationSeconds(-9223372036) = (%v, %v), want the smallest representable value", v, err)
	}
	for _, raw := range []string{
		"9223372037",          // first value past the ceiling
		"-9223372037",         // first value past the floor
		"9223372036854",       // the issue's report: wraps to -775.808ms
		"9223372036855",       // wraps to a plausible POSITIVE 224.192ms
		"-9223372036854",      // the mirror: a huge negative wraps positive
		"9223372036854775807", // math.MaxInt64: Atoi ACCEPTS it on a 64-bit int, the multiply wraps to -1s
	} {
		v, err := parseDurationSeconds(raw, nil)
		if err == nil {
			t.Errorf("parseDurationSeconds(%s) = %v, want an error (out of range)", raw, v)
			continue
		}
		if v != nil {
			t.Errorf("parseDurationSeconds(%s) rejected but returned %v — a rejected value must yield no Duration", raw, v)
		}
	}
}

func TestParseLocation(t *testing.T) {
	if v, err := parseLocationValue("", nil); err != nil || v.(*time.Location) != time.UTC {
		t.Errorf("empty timezone = (%v, %v), want UTC", v, err)
	}
	if v, err := parseLocationValue("Europe/Berlin", nil); err != nil || v.(*time.Location).String() != "Europe/Berlin" {
		t.Errorf("Europe/Berlin = (%v, %v)", v, err)
	}
	if _, err := parseLocationValue("Mars/Olympus_Mons", nil); err == nil {
		t.Error("unknown timezone should error (legacy fatal path)")
	}
}

// --- fromSources: source tracking, strictness, defaults ---.

func TestFromSourcesDefaultsAreClean(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	if len(issues) != 0 {
		t.Fatalf("defaults must parse without issues, got %v", issues)
	}
	if got := Validate(c); len(got) != 0 {
		t.Fatalf("default config must validate clean, got %v", got)
	}
	if c.Source("server.db_password") != "env" {
		t.Errorf("db_password source = %q, want env", c.Source("server.db_password"))
	}
	if c.Source("digest.mode") != "default" {
		t.Errorf("digest.mode source = %q, want default", c.Source("digest.mode"))
	}
	if c.Source("nonexistent.key") != "" {
		t.Errorf("unknown key source = %q, want empty", c.Source("nonexistent.key"))
	}
}

func TestFromSourcesSafeMalformedWarnsAndDefaults(t *testing.T) {
	// parse:"safe" fields: malformed value keeps the default — today's silent
	// getEnv*Safe fallback, now visible as a WARN (Delta 3).
	c, issues := cfgFrom(t, map[string]string{
		"rerank.max_docs":      "abc",
		"rerank.blend_weight":  "xyz",
		"dream.backoff_factor": "junk",
		"dream.idle_wait":      "zz",
	})
	for key, want := range map[string]any{
		"rerank.max_docs":      50,
		"rerank.blend_weight":  1.0,
		"dream.backoff_factor": 1.6,
	} {
		is := issueFor(issues, key)
		if is == nil || is.Severity != SeverityWarn {
			t.Errorf("%s: want WARN issue, got %v", key, is)
		}
		_ = want
	}
	if c.Rerank.MaxDocs != 50 || c.Rerank.BlendWeight != 1.0 || c.Dream.Backoff.Factor != 1.6 {
		t.Errorf("safe-malformed fields must keep defaults, got %d %g %g",
			c.Rerank.MaxDocs, c.Rerank.BlendWeight, c.Dream.Backoff.Factor)
	}
	if c.Dream.IdleWait != 20*time.Second {
		t.Errorf("idle_wait default = %v, want 20s", c.Dream.IdleWait)
	}
}

func TestFromSourcesStrictMalformedErrors(t *testing.T) {
	// parse:"strict" fields: malformed value is a SeverityError on the exact
	// field (today's getEnvInt fatal paths). The value stays default so the
	// rest of the dump remains renderable before the boot abort.
	for _, key := range []string{
		"server.db_port", "graph_overview.label_batch",
		"query.rate_limit_write", "query.rate_limit_read",
	} {
		_, issues := cfgFrom(t, map[string]string{key: "not_a_number"})
		is := issueFor(issues, key)
		if is == nil || is.Severity != SeverityError {
			t.Errorf("%s: malformed strict field must be SeverityError, got %v", key, is)
		}
	}
	_, issues := cfgFrom(t, map[string]string{"query.timezone": "Mars/Olympus_Mons"})
	if is := issueFor(issues, "query.timezone"); is == nil || is.Severity != SeverityError {
		t.Errorf("query.timezone: unknown zone must be SeverityError, got %v", is)
	}
}

func TestFromSourcesScopesFreshSlice(t *testing.T) {
	// Snapshots never share a mutable backing array (immutability convention;
	// risk 7 — a future in-place sort must not corrupt parallel readers of
	// OTHER generations).
	c1, _ := cfgFrom(t, map[string]string{"scheduler.read_scopes": "a,b,c"})
	c2, _ := cfgFrom(t, map[string]string{"scheduler.read_scopes": "a,b,c"})
	c1.Scheduler.ReadScopes[0] = "mutated"
	if c2.Scheduler.ReadScopes[0] != "a" {
		t.Error("two loads share a ReadScopes backing array")
	}
}

func TestFromEnvReadsEnvironment(t *testing.T) {
	for _, v := range EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv("CTX_DIGEST_MODE", "env-mode")
	t.Setenv("CTX_DREAM_BACKOFF_CAP", "45d")

	c, issues := FromEnv()
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if c.Digest.Mode != "env-mode" {
		t.Errorf("Digest.Mode = %q", c.Digest.Mode)
	}
	if c.Dream.Backoff.CapHours != Hours(45*24) {
		t.Errorf("CapHours = %v, want 1080", c.Dream.Backoff.CapHours)
	}
	// The "env" side rode on dream.host until β6, embed.host until β7 and
	// chat.host until β8 cut the last tuple; digest.mode is set in this fixture
	// and, like dream.language on the "default" side, outlives the cut train.
	if c.Source("digest.mode") != "env" || c.Source("dream.language") != "default" {
		t.Errorf("sources wrong: digest.mode=%q dream.language=%q",
			c.Source("digest.mode"), c.Source("dream.language"))
	}
	// Empty env == unset (legacy getEnv semantics). It rode on embed.host until
	// β7 cut the tuple; server.listen_addr is a string key with a non-empty
	// default that the loop above blanks like every other env var, and unlike
	// the backend hosts it outlives the whole cut train.
	if c.Server.ListenAddr != ":8080" {
		t.Errorf("empty env must fall back to default, got %q", c.Server.ListenAddr)
	}
	if !slices.Equal(c.Scheduler.ReadScopes, []string{"private", "shared", "work"}) {
		t.Errorf("ReadScopes default = %v", c.Scheduler.ReadScopes)
	}
	if c.Scheduler.HomeScope != "private" {
		t.Errorf("HomeScope = %q, want private (no env knob in F1)", c.Scheduler.HomeScope)
	}
}
