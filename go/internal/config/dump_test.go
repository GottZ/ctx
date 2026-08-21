package config

import (
	"fmt"
	"strings"
	"testing"
)

const (
	// 38 chars ≥ fpMinLen — fingerprint-eligible machine key.
	longKey = "sk-test-0123456789abcdefghijklmnopqrst"
	// 10 chars < fpMinLen — could be human-chosen, presence-only.
	shortKey = "hunter2-pw"
)

func dumpFor(t *testing.T, vals map[string]string, s Surface) map[string]any {
	t.Helper()
	c, _ := cfgFrom(t, vals)
	return c.Redacted(s)
}

func group(t *testing.T, dump map[string]any, name string) map[string]any {
	t.Helper()
	g, ok := dump[name].(map[string]any)
	if !ok {
		t.Fatalf("dump has no group %q: %v", name, dump)
	}
	return g
}

// TestMaskSecretPerClass pins §3.5 for the class that still has a member: the
// human-chosen db password is "presence" and renders "set" on every surface,
// never a fingerprint (an unsalted hash prefix of a human-chosen value is an
// offline dictionary oracle).
//
// The fp half of this test — long value fingerprinted in the boot dump, short
// value presence-only, unset value empty, API surface never fingerprinting —
// rode on chat.api_key, the registry's LAST secret:"fp" key, and left with it
// in β8. It did not lose its subject, only its member: maskSecret's fpMinLen
// branch is untouched and now runs on the injected-registry vehicle
// (synthreg_test.go, TestSynthMaskSecretPerClass), which drives renderField
// with a synthetic fp entry over all four cases. Substituting server.db_password
// here would have been the opposite of coverage — it can never take the branch.
func TestMaskSecretPerClass(t *testing.T) {
	boot := dumpFor(t, map[string]string{"server.db_password": longKey}, SurfaceBootDump)
	if got := group(t, boot, "server")["db_password"]; got != "set" {
		t.Errorf("boot dump db password = %v, want 'set' (never fingerprinted)", got)
	}
	api := dumpFor(t, map[string]string{"server.db_password": longKey}, SurfaceAPI)
	if got := group(t, api, "server")["db_password"]; got != "set" {
		t.Errorf("API surface db password = %v, want 'set'", got)
	}
	// The empty-value branch of maskSecret, on the one key that can still reach
	// it: cfgFrom injects a password only when the fixture omits it, so an
	// explicit empty string is the way to render an unset secret.
	unset := dumpFor(t, map[string]string{"server.db_password": ""}, SurfaceBootDump)
	if got := group(t, unset, "server")["db_password"]; got != "" {
		t.Errorf("unset secret = %v, want empty string", got)
	}
}

// TestDumpNeverLeaksSecretValues sweeps the full rendered dump for the raw
// secret on both surfaces. Red-proof: drop the e.Secret branch in renderField
// and this fails on every surface.
func TestDumpNeverLeaksSecretValues(t *testing.T) {
	// The fp-class half of this sweep left with chat.api_key in β8; the
	// presence-class half is the whole sweep now. shortKey is deliberately the
	// value: it is below fpMinLen, so a regression that started fingerprinting
	// presence keys would still not print it — what this test catches is the
	// raw value appearing anywhere in the rendered dump.
	vals := map[string]string{
		"server.db_password": shortKey,
	}
	for s, name := range map[Surface]string{SurfaceBootDump: "boot", SurfaceAPI: "api"} {
		rendered := fmt.Sprintf("%v", dumpFor(t, vals, s))
		if strings.Contains(rendered, shortKey) {
			t.Errorf("%s surface leaks a raw secret: %s", name, rendered)
		}
	}
}

// TestDumpRedactsHostURLs died with chat.host in β8 — the last .host key in the
// registry. It pinned the §3.5 URL convention on the dump: a host field passes
// through (*url.URL).Redacted(), so a userinfo password is masked even in the
// window before Validate aborts the boot, and an unparseable URL is withheld
// entirely because (*url.Error).Error() would embed it raw.
//
// redactHostURL itself is untouched and deliberately kept: it selects on the KEY
// SUFFIX, not on a field list, so it is a standing guard for whatever .host key
// is added next rather than a chat.* artifact. Its three cases (parseable
// userinfo, unparseable, clean) moved to synthreg_test.go's
// TestSynthHostKeyIsRedacted, which drives renderField with a synthetic
// legacy.demo.host entry plus a non-.host control proving the suffix is what
// selects redaction.

// TestDumpInheritMarkers died with inheritMarkers in β6. It pinned both halves
// of the renderField branch: a zero field rendered its "(inherit …)" marker, a
// set one its verbatim value. The five dream_embed markers left in β5, the
// three dream→chat markers with DreamBackend in β6, and an empty map plus a
// branch no key can reach is not a thing to keep a test around for. The
// verbatim half is not lost — every non-secret, non-host key in
// TestDumpSourcesAndRendering asserts exactly that.

func TestDumpSourcesAndRendering(t *testing.T) {
	dump := dumpFor(t, map[string]string{
		"digest.mode":       "env-mode",
		"dream.backoff_cap": "45d",
		"dream.backoff_min": "12h",
		// The time.Duration rendering probe rode on chat_fallback.timeout until
		// β4 cut it. dream.idle_wait carries the same two properties — the
		// bare-int-seconds parse (load.go parseDurationSeconds) and the
		// Duration.String() render — on a key no cut wave touches.
		"dream.idle_wait": "420",
	}, SurfaceBootDump)

	sources, ok := dump["sources"].(map[string]string)
	if !ok {
		t.Fatal("dump has no sources map")
	}
	// The "default" side of the source probe rode on dream.host until β6 cut the
	// tuple, the "env" side on chat.host until β8 cut the last one. Both sides
	// sit on keys no cut wave touches now: digest.mode is provided by this
	// fixture, dream.language is not.
	if sources["digest.mode"] != "env" || sources["dream.language"] != "default" {
		t.Errorf("sources: digest.mode=%q dream.language=%q", sources["digest.mode"], sources["dream.language"])
	}

	dream := group(t, dump, "dream")
	if dream["backoff_cap"] != "45d" || dream["backoff_min"] != "12h" {
		t.Errorf("hours rendering: cap=%v min=%v", dream["backoff_cap"], dream["backoff_min"])
	}
	if got := dream["idle_wait"]; got != "7m0s" {
		t.Errorf("duration rendering = %v, want 7m0s", got)
	}
	if got := group(t, dump, "query")["timezone"]; got != "UTC" {
		t.Errorf("timezone rendering = %v, want UTC", got)
	}
}

func TestRenderHours(t *testing.T) {
	for in, want := range map[Hours]string{
		Hours(12):   "12h",
		Hours(36):   "36h",
		Hours(48):   "2d",
		Hours(1080): "45d",
		Hours(1.5):  "1.5h",
	} {
		if got := renderHours(in); got != want {
			t.Errorf("renderHours(%v) = %q, want %q", float64(in), got, want)
		}
	}
}

// TestBootDumpArgsShape pins the slog-args contract: alternating key/value
// pairs, fixed group order, sources + issues at the end.
//
// It doubles as the group-absence gate of the cut train, measured in β5: a
// name left in dumpGroupOrder for a group whose keys all went (dream_embed) is
// skipped by BootDumpArgs but still lands in want — the comparison fails, so
// the dead ordering is caught here and needs no pin of its own.
func TestBootDumpArgsShape(t *testing.T) {
	c, _ := cfgFrom(t, map[string]string{})
	issues := []Issue{{Field: "rerank.blend_weight", Severity: SeverityWarn, Msg: "test"}}
	args := BootDumpArgs(c, issues)
	if len(args)%2 != 0 {
		t.Fatalf("BootDumpArgs must return key/value pairs, got %d items", len(args))
	}
	var keys []string
	for i := 0; i < len(args); i += 2 {
		s, ok := args[i].(string)
		if !ok {
			t.Fatalf("arg %d is not a string key: %v", i, args[i])
		}
		keys = append(keys, s)
	}
	want := append(append([]string{}, dumpGroupOrder...), "sources", "issues")
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("arg keys = %v, want %v", keys, want)
	}
	rendered, ok := args[len(args)-1].([]map[string]string)
	if !ok || len(rendered) != 1 || rendered[0]["severity"] != "warn" {
		t.Errorf("issues rendering = %v", args[len(args)-1])
	}
}
