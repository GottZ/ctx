package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// errNoSuchSecret mirrors the store.ResolveSecret contract: name-free and
// value-free, so admitOverride may embed it in a WARN verbatim.
var errNoSuchSecret = errors.New("secrets: no such secret in scope — create it first")

// The injected-registry vehicle (design/01 §5.6, β8).
//
// Three registry CLASSES lost their last member with the chat tuple, and each of
// them guards something that must not go untested just because no key carries it
// today:
//
//   - secret:"fp" — the settings-side secret_ref resolution (build.go
//     admitOverride) and the fingerprint masking (dump.go maskSecret). Last
//     carrier: chat.api_key.
//   - the ".host" namespace convention — redactHostURL keeps a userinfo password
//     out of the boot dump. Last carrier: chat.host.
//   - mut:"coupled:embed-cache" — admitted for settings writes although every
//     other non-hot class is dropped. Last carrier: embed.host/protocol (β7),
//     which is why β7 already recorded this gap and pointed it at this wave.
//
// The vehicle is a test-local struct whose tags carry those classes, run through
// the REAL buildRegistry — the registry builder is reflective and struct-
// agnostic, so tag parsing, class validation and the parser dispatch are the
// production ones, not a hand-built entry literal. What a freshly built entry
// cannot have is a field to write to: its path indexes the synthetic struct,
// while every consumer (Build, Redacted, RenderValue) reflects over *Config. So
// the built entries are TRANSPLANTED onto a live donor field before use —
// classification from the synthetic tags, storage from a real key.
//
// Donor is scheduler.home_scope: a hot, plain string with no env var (so no
// stray CTX_* can shadow it), no cross-field check in Validate, and no
// normalization that would rewrite a value the test wrote. Its own registry
// entry stays in place; the synthetic entries are ADDED to the key index, never
// substituted for it.
//
// Only entryByKey is swapped, not registry(): the index is what admitOverride
// and RenderValue consult, while FromEnv/Redacted iterate registry() — leaving
// that alone keeps env loading, the boot dump and Validate byte-identical, so a
// synthetic key can never leak into an unrelated test's expectations.

const (
	synthSecretKey  = "legacy.demo_key"     // secret:"fp"
	synthHostKey    = "legacy.demo.host"    // ".host" namespace
	synthCoupledKey = "legacy.demo_coupled" // mut:"coupled:embed-cache"
	synthRestartKey = "legacy.demo_restart" // mut:"restart" — the negative control
	synthDonorKey   = "scheduler.home_scope"
)

// synthGroup carries the retired classes as struct tags. env:"-" keeps the
// synthetic keys off every env surface; the defaults mirror the donor's so a
// transplanted entry never disagrees with the field it writes to.
type synthGroup struct {
	Key     string `key:"legacy.demo_key" env:"-" default:"private" mut:"hot" secret:"fp" tenancy:"tenant-overridable"`
	Host    string `key:"legacy.demo.host" env:"-" default:"private" mut:"hot" tenancy:"global-only"`
	Coupled string `key:"legacy.demo_coupled" env:"-" default:"private" mut:"coupled:embed-cache" tenancy:"global-only"`
	Restart string `key:"legacy.demo_restart" env:"-" default:"private" mut:"restart" tenancy:"global-only"`
}

// synthEntries builds the vehicle entries through the real registry builder and
// transplants them onto the donor field.
func synthEntries(t *testing.T) map[string]entry {
	t.Helper()
	built, err := buildRegistry(reflect.TypeOf(synthGroup{}))
	if err != nil {
		t.Fatalf("buildRegistry over the vehicle struct: %v — the retired classes must stay valid registry vocabulary", err)
	}
	donor, ok := entryByKey()[synthDonorKey]
	if !ok {
		t.Fatalf("donor key %s is gone from the registry — pick another hot, unvalidated string key", synthDonorKey)
	}
	if donor.typ != typString || donor.Mut != "hot" || donor.EnvVar != "-" {
		t.Fatalf("donor %s changed shape (typ=%v mut=%q env=%q) — the vehicle needs a hot, env-less string",
			synthDonorKey, donor.typ, donor.Mut, donor.EnvVar)
	}
	out := make(map[string]entry, len(built))
	for _, e := range built {
		e.path = donor.path
		e.typ = donor.typ
		e.defRaw = donor.defRaw
		e.defVal = donor.defVal
		out[e.Key] = e
	}
	if len(out) != 4 {
		t.Fatalf("vehicle built %d entries, want 4", len(out))
	}
	return out
}

// withSynthRegistry adds the vehicle entries to the key index for one test and
// restores it afterwards. It refuses to shadow a live key, so the vehicle can
// never quietly redefine real configuration.
func withSynthRegistry(t *testing.T) map[string]entry {
	t.Helper()
	synth := synthEntries(t)
	saved := entryByKey
	live := saved()
	merged := make(map[string]entry, len(live)+len(synth))
	for k, v := range live {
		merged[k] = v
	}
	for k, v := range synth {
		if _, clash := merged[k]; clash {
			t.Fatalf("vehicle key %s collides with a live registry key", k)
		}
		merged[k] = v
	}
	entryByKey = func() map[string]entry { return merged }
	t.Cleanup(func() { entryByKey = saved })
	return synth
}

// TestSynthRegistryVehicleCarriesTheRetiredClasses pins the vehicle itself.
// Every later test in this file reads a class off these entries; if the builder
// stopped honouring one of the tags, those tests would go green on an entry that
// no longer means what their name claims.
func TestSynthRegistryVehicleCarriesTheRetiredClasses(t *testing.T) {
	synth := synthEntries(t)
	for key, want := range map[string]struct{ secret, mut string }{
		synthSecretKey:  {"fp", "hot"},
		synthHostKey:    {"", "hot"},
		synthCoupledKey: {"", "coupled:embed-cache"},
		synthRestartKey: {"", "restart"},
	} {
		e, ok := synth[key]
		if !ok {
			t.Fatalf("vehicle is missing %s", key)
		}
		if e.Secret != want.secret || e.Mut != want.mut {
			t.Errorf("%s: secret=%q mut=%q, want secret=%q mut=%q", key, e.Secret, e.Mut, want.secret, want.mut)
		}
	}
	// The classes must still be UNOCCUPIED in the live registry — that is the
	// whole reason this vehicle exists. A new carrier is not an error, but it
	// means the vehicle is no longer the only coverage and the comment above is
	// stale.
	for _, e := range registry() {
		if e.Secret == "fp" {
			t.Errorf("%s carries secret:\"fp\" again — the class is occupied, update the vehicle's rationale", e.Key)
		}
		if e.Mut == "coupled" || e.Mut == "coupled:embed-cache" {
			t.Errorf("%s carries %q again — whoever added it owes the class a flush obligation (build.go)", e.Key, e.Mut)
		}
		if strings.HasSuffix(e.Key, ".host") {
			t.Errorf("%s is a live .host key again — redactHostURL has a real carrier, say so in dump.go", e.Key)
		}
	}
}

// TestSynthSecretRefResolutionEndToEnd drives build.go's secret branch through
// the real Build: a row on an fp-class key is a secret_ref NAME that resolves to
// plaintext in memory, a resolution failure leaves the env/default value active,
// and a missing resolver does the same. This is the strecke chat.api_key carried
// until β8 (build_test.go TestBuildSecretRefResolution).
//
// Negative probe (run and reverted): drop the `if e.Secret != ""` branch in
// admitOverride and the resolve subtest fails with the raw ref name in the
// field — the override would be parsed as a plain string.
func TestSynthSecretRefResolutionEndToEnd(t *testing.T) {
	// Runtime-concatenated marker, never a secret-shaped literal (fixture
	// hygiene: scanners match FORM, not authenticity).
	plaintext := "RESOLVEDMARKER-" + strings.Repeat("r", 24)
	resolve := func(name string) (string, error) {
		if name == "prov-main" {
			return plaintext, nil
		}
		return "", errNoSuchSecret
	}

	t.Run("secret_ref resolves to plaintext in-memory", func(t *testing.T) {
		resetBuildEnv(t)
		withSynthRegistry(t)
		c, issues := Build([]Override{{Key: synthSecretKey, Value: "prov-main"}}, resolve)
		if HasErrors(issues) {
			t.Fatalf("build errors: %v", issues)
		}
		if got := c.Scheduler.HomeScope; got != plaintext {
			t.Errorf("donor field = %q, want the resolved plaintext", got)
		}
		if got := c.Source(synthSecretKey); got != SourceSettings {
			t.Errorf("Source(%s) = %q, want %q", synthSecretKey, got, SourceSettings)
		}
	})

	t.Run("resolver failure keeps the env/default value", func(t *testing.T) {
		resetBuildEnv(t)
		withSynthRegistry(t)
		c, issues := Build([]Override{{Key: synthSecretKey, Value: "does-not-exist"}}, resolve)
		if got := c.Scheduler.HomeScope; got != "private" {
			t.Errorf("donor field = %q, want the untouched default", got)
		}
		if !warnMentions(issues, synthSecretKey, "secret_ref resolution failed") {
			t.Errorf("issues = %v, want a WARN naming the key and the failure", issues)
		}
	})

	t.Run("nil resolver ignores the override", func(t *testing.T) {
		resetBuildEnv(t)
		withSynthRegistry(t)
		c, issues := Build([]Override{{Key: synthSecretKey, Value: "prov-main"}}, nil)
		if got := c.Scheduler.HomeScope; got != "private" {
			t.Errorf("donor field = %q, want the untouched default", got)
		}
		if !warnMentions(issues, synthSecretKey, "no secret resolver available") {
			t.Errorf("issues = %v, want a WARN about the missing resolver", issues)
		}
	})
}

// TestSynthSecretRefWarningNeverEchoesTheRefValue is the leak half of the same
// strecke: until the F2 write gate enforces the ref FORMAT, an operator can
// paste a provider key into the value slot. The resolver error path must then
// stay free of it — the message comes from the SecretResolver contract, and
// admitOverride must not add the input back in.
func TestSynthSecretRefWarningNeverEchoesTheRefValue(t *testing.T) {
	resetBuildEnv(t)
	withSynthRegistry(t)
	const pasted = "PASTEDMARKER-0123456789abcdefghij"
	_, issues := Build([]Override{{Key: synthSecretKey, Value: pasted}},
		func(string) (string, error) { return "", errNoSuchSecret })
	for _, is := range issues {
		if strings.Contains(is.Msg, pasted) {
			t.Fatalf("LEAK: issue %q carries the pasted ref value", is.Msg)
		}
	}
	if !warnMentions(issues, synthSecretKey, "secret_ref resolution failed") {
		t.Fatalf("issues = %v — the scan proved nothing without the WARN", issues)
	}
}

// TestSynthCoupledEmbedCacheKeyIsAdmitted closes the coverage gap β7 recorded:
// admitOverride lets mut:"coupled:embed-cache" through next to "hot" while every
// other non-hot class is dropped, and since β7 no registry key exercised the
// positive half. The pairing with the restart control is what makes it a gate
// rather than a tautology.
//
// Negative probe (run and reverted): remove `&& e.Mut != "coupled:embed-cache"`
// from the admission condition and the admitted subtest fails — the override is
// dropped with the mutability WARN.
func TestSynthCoupledEmbedCacheKeyIsAdmitted(t *testing.T) {
	resetBuildEnv(t)
	withSynthRegistry(t)

	c, issues := Build([]Override{{Key: synthCoupledKey, Value: "coupled-value"}}, nil)
	if got := c.Scheduler.HomeScope; got != "coupled-value" {
		t.Errorf("coupled:embed-cache override was not admitted: donor field = %q", got)
	}
	if got := c.Source(synthCoupledKey); got != SourceSettings {
		t.Errorf("Source(%s) = %q, want %q", synthCoupledKey, got, SourceSettings)
	}
	if warnMentions(issues, synthCoupledKey, "not runtime-overridable") {
		t.Errorf("issues = %v, want no mutability WARN on the coupled class", issues)
	}

	c2, issues2 := Build([]Override{{Key: synthRestartKey, Value: "restart-value"}}, nil)
	if got := c2.Scheduler.HomeScope; got != "private" {
		t.Errorf("restart override must be dropped, donor field = %q", got)
	}
	if !warnMentions(issues2, synthRestartKey, "not runtime-overridable") {
		t.Errorf("issues = %v, want the mutability WARN on the restart class", issues2)
	}
}

// TestSynthMaskSecretPerClass pins §3.5 on the fp class: a machine-length value
// renders a sha256 fingerprint in the boot dump only, a short (possibly
// human-chosen) value stays presence-only, an empty value renders empty, and the
// API surface never fingerprints at all. renderField is the production function;
// only the entry is synthetic.
func TestSynthMaskSecretPerClass(t *testing.T) {
	synth := synthEntries(t)
	e := synth[synthSecretKey]

	sum := sha256.Sum256([]byte(longKey))
	wantFP := "set:sha256:" + hex.EncodeToString(sum[:4])
	if got := renderField(e, reflect.ValueOf(longKey), SurfaceBootDump); got != wantFP {
		t.Errorf("boot dump long fp value = %v, want %s", got, wantFP)
	}
	if got := renderField(e, reflect.ValueOf(shortKey), SurfaceBootDump); got != "set" {
		t.Errorf("boot dump short fp value = %v, want presence-only 'set'", got)
	}
	if got := renderField(e, reflect.ValueOf(""), SurfaceBootDump); got != "" {
		t.Errorf("unset secret = %v, want the empty string", got)
	}
	if got := renderField(e, reflect.ValueOf(longKey), SurfaceAPI); got != "set" {
		t.Errorf("API surface must never fingerprint, got %v", got)
	}
}

// TestSynthHostKeyIsRedacted pins the .host namespace convention: redactHostURL
// runs on the KEY SUFFIX, not on a field list, so it protects the next .host key
// somebody adds. Both url.Parse outcomes are covered — a parseable userinfo host
// renders the stdlib-redacted form, an unparseable one is withheld entirely
// (its error text would carry the raw input back).
func TestSynthHostKeyIsRedacted(t *testing.T) {
	synth := synthEntries(t)
	e := synth[synthHostKey]
	const secret = "hunter2-secret-marker"

	got := renderField(e, reflect.ValueOf("http://admin:"+secret+"@chat.example:8089"), SurfaceBootDump)
	if s, _ := got.(string); strings.Contains(s, secret) {
		t.Fatalf("dump leaks a userinfo password: %v", got)
	}
	if got != "http://admin:xxxxx@chat.example:8089" {
		t.Errorf("parseable userinfo host = %v, want the stdlib-redacted form", got)
	}

	bad := renderField(e, reflect.ValueOf("http://admin:"+secret+"@bad host:8081"), SurfaceBootDump)
	if s, _ := bad.(string); strings.Contains(s, secret) || strings.Contains(s, "bad host") {
		t.Fatalf("unparseable host must be withheld, got %v", bad)
	}

	clean := renderField(e, reflect.ValueOf("http://chat.example:8089"), SurfaceBootDump)
	if clean != "http://chat.example:8089" {
		t.Errorf("clean host must render verbatim, got %v", clean)
	}

	// The non-host control: the same value under a key without the suffix
	// renders raw. Without it the test could pass on a renderField that redacts
	// everything, which would hide real values from the boot dump.
	plain := renderField(synth[synthCoupledKey], reflect.ValueOf("http://admin:"+secret+"@chat.example:8089"), SurfaceBootDump)
	if s, _ := plain.(string); !strings.Contains(s, secret) {
		t.Errorf("non-.host key = %v, want the verbatim value (the suffix is what selects redaction)", plain)
	}
}

// warnMentions reports whether issues carry a WARN on key whose message
// contains want.
func warnMentions(issues []Issue, key, want string) bool {
	for _, is := range issues {
		if is.Field == key && is.Severity == SeverityWarn && strings.Contains(is.Msg, want) {
			return true
		}
	}
	return false
}
