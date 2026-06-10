package config

import (
	"crypto/sha256"
	"encoding/hex"
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

// TestMaskSecretPerClass pins §3.5: fp-class keys of fpMinLen+ render a
// sha256 fingerprint ONLY in the boot dump; short keys and the presence-class
// db password render "set" everywhere; SurfaceAPI never fingerprints.
func TestMaskSecretPerClass(t *testing.T) {
	vals := map[string]string{
		"chat.api_key":       longKey,
		"embed.api_key":      shortKey,
		"server.db_password": longKey, // long but human-class: still presence-only
	}

	boot := dumpFor(t, vals, SurfaceBootDump)
	sum := sha256.Sum256([]byte(longKey))
	wantFP := "set:sha256:" + hex.EncodeToString(sum[:4])
	if got := group(t, boot, "chat")["api_key"]; got != wantFP {
		t.Errorf("boot dump long fp key = %v, want %s", got, wantFP)
	}
	if got := group(t, boot, "embed")["api_key"]; got != "set" {
		t.Errorf("boot dump short fp key = %v, want presence-only 'set'", got)
	}
	if got := group(t, boot, "server")["db_password"]; got != "set" {
		t.Errorf("boot dump db password = %v, want 'set' (never fingerprinted)", got)
	}
	if got := group(t, boot, "dream")["api_key"]; got != "" {
		t.Errorf("unset secret = %v, want empty string", got)
	}

	api := dumpFor(t, vals, SurfaceAPI)
	if got := group(t, api, "chat")["api_key"]; got != "set" {
		t.Errorf("API surface must never fingerprint, got %v", got)
	}
}

// TestDumpNeverLeaksSecretValues sweeps the full rendered dump for the raw
// secret on both surfaces. Red-proof: drop the e.Secret branch in renderField
// and this fails on every surface.
func TestDumpNeverLeaksSecretValues(t *testing.T) {
	vals := map[string]string{
		"chat.api_key":          longKey,
		"chat_fallback.api_key": longKey,
		"embed.api_key":         longKey,
		"dream.api_key":         longKey,
		"dream_embed.api_key":   longKey,
		"rerank.api_key":        longKey,
		"server.db_password":    shortKey,
	}
	for s, name := range map[Surface]string{SurfaceBootDump: "boot", SurfaceAPI: "api"} {
		rendered := fmt.Sprintf("%v", dumpFor(t, vals, s))
		if strings.Contains(rendered, longKey) || strings.Contains(rendered, shortKey) {
			t.Errorf("%s surface leaks a raw secret: %s", name, rendered)
		}
	}
}

// TestDumpRedactsHostURLs pins the §3.5 URL convention on the dump itself:
// host fields pass through (*url.URL).Redacted(), so a userinfo password is
// masked even in the window before Validate aborts the boot; unparseable
// URLs are withheld entirely ((*url.Error).Error() would embed them raw).
func TestDumpRedactsHostURLs(t *testing.T) {
	const secret = "hunter2-secret-marker"
	dump := dumpFor(t, map[string]string{
		"chat.host":  "http://admin:" + secret + "@chat.example:8089",
		"embed.host": "http://admin:" + secret + "@bad host:8081",
	}, SurfaceBootDump)

	rendered := fmt.Sprintf("%v", dump)
	if strings.Contains(rendered, secret) {
		t.Fatalf("dump leaks a userinfo password: %s", rendered)
	}
	if got := group(t, dump, "chat")["host"]; got != "http://admin:xxxxx@chat.example:8089" {
		t.Errorf("parseable userinfo host = %v, want stdlib-redacted form", got)
	}
	if got, ok := group(t, dump, "embed")["host"].(string); !ok || strings.Contains(got, "bad host") {
		t.Errorf("unparseable host must be withheld, got %v", got)
	}
	// Hosts without credentials render unchanged — the dump stays useful.
	clean := dumpFor(t, map[string]string{"chat.host": "http://chat.example:8089"}, SurfaceBootDump)
	if got := group(t, clean, "chat")["host"]; got != "http://chat.example:8089" {
		t.Errorf("clean host must render verbatim, got %v", got)
	}
}

func TestDumpInheritMarkers(t *testing.T) {
	dump := dumpFor(t, map[string]string{}, SurfaceBootDump)
	for key, field := range map[string]string{"model": "model", "num_ctx": "num_ctx", "think": "think"} {
		if got := group(t, dump, "dream")[field]; got != "(inherit chat)" {
			t.Errorf("dream.%s = %v, want '(inherit chat)'", key, got)
		}
	}
	if got := group(t, dump, "dream_embed")["host"]; got != "(inherit embed)" {
		t.Errorf("dream_embed.host = %v, want '(inherit embed)'", got)
	}

	set := dumpFor(t, map[string]string{"dream.model": "dream-model-x"}, SurfaceBootDump)
	if got := group(t, set, "dream")["model"]; got != "dream-model-x" {
		t.Errorf("set dream.model = %v, want verbatim value", got)
	}
}

func TestDumpSourcesAndRendering(t *testing.T) {
	dump := dumpFor(t, map[string]string{
		"chat.host":             "http://chat.example:8089",
		"dream.backoff_cap":     "45d",
		"dream.backoff_min":     "12h",
		"chat_fallback.timeout": "420",
	}, SurfaceBootDump)

	sources, ok := dump["sources"].(map[string]string)
	if !ok {
		t.Fatal("dump has no sources map")
	}
	if sources["chat.host"] != "env" || sources["dream.host"] != "default" {
		t.Errorf("sources: chat.host=%q dream.host=%q", sources["chat.host"], sources["dream.host"])
	}

	dream := group(t, dump, "dream")
	if dream["backoff_cap"] != "45d" || dream["backoff_min"] != "12h" {
		t.Errorf("hours rendering: cap=%v min=%v", dream["backoff_cap"], dream["backoff_min"])
	}
	if got := group(t, dump, "chat_fallback")["timeout"]; got != "7m0s" {
		t.Errorf("timeout rendering = %v, want 7m0s", got)
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
