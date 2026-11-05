package oai_test

// Pins of the relay sanitizer remnants (Fremd-Review 2 F2-6, lead decision
// H-7), cases from the review probe sanitize-1.log. Every pin only asserts
// that an identity or a network fragment is gone: over-replacement is
// allowed, under-replacement is not, so no pin here asserts what survives
// (the survival side is TestSanitizeUpstreamTextIdentities and G9 B4).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// sanFR2Snap is the pool of the review probe: row spark with a scheme, a
// mixed-case host and port, a namespaced model and a provider; row gx on an
// IPv6 literal.
func sanFR2Snap() []backends.Backend {
	return []backends.Backend{
		{Name: "spark", Host: "http://Spark-GPU.internal.lan:30000", Model: "meta-llama/Llama-3.3-70B-Instruct-Turbo", ProviderClass: "openrouter"},
		{Name: "gx", Host: "http://[fd00::17]:8000", Model: "qwen38-27b"},
	}
}

type sanCase struct{ name, in string }

// sanNoneIn fails when the sanitized text of any case still carries one of
// the fragments (case-insensitive).
func sanNoneIn(t *testing.T, cases []sanCase, fragments ...string) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := oai.SanitizeUpstreamText(tc.in, "chat", sanFR2Snap())
			for _, f := range fragments {
				if strings.Contains(strings.ToLower(got), strings.ToLower(f)) {
					t.Errorf("sanitize(%q) = %q, still carries %q", tc.in, got, f)
				}
			}
		})
	}
}

// TestSanitizeIPv6Zone (F2-6 H-7 1): an IPv6 literal goes with its zone
// index, bare, bracketed with a port, and in the URI form %25.
func TestSanitizeIPv6Zone(t *testing.T) {
	sanNoneIn(t, []sanCase{
		{"bare", "peer fe80::1%eth0 refused"},
		{"bracketed with port", "peer [fe80::1%eth0]:8080 refused"},
		{"uri zone", "peer fe80::1%25enp3s0 refused"},
	}, "fe80", "%", "eth0", "enp3s0")
}

// TestSanitizeUserinfoWithoutScheme (F2-6 H-7 2): user:password@host without
// a scheme goes as a whole — user, password and the host behind the @, also
// one that is no identity of the pool.
func TestSanitizeUserinfoWithoutScheme(t *testing.T) {
	sanNoneIn(t, []sanCase{
		{"pool host", "auth user:s3cret@spark-gpu.internal.lan failed"},
		{"foreign host with port", "auth user:s3cret@db.example.lan:5432 failed"},
		{"user without password", "auth user@10.9.8.7 failed"},
	}, "user", "s3cret", "@", "spark", "internal.lan", "db.example", "5432", "10.9")
}

// TestSanitizeIPv4MappedIPv6 (F2-6 H-7 3): an address with a leading :: goes
// as a whole — IPv4-mapped (::ffff:a.b.c.d), bracketed with a port, and the
// loopback ::1.
func TestSanitizeIPv4MappedIPv6(t *testing.T) {
	sanNoneIn(t, []sanCase{
		{"mapped", "mapped ::ffff:10.1.2.3 refused"},
		{"mapped bracketed with port", "mapped [::ffff:10.1.2.3]:8000 refused"},
		{"loopback", "peer ::1 refused"},
	}, "ffff", "10.1", "::", ":8000")
}

// sanFR2RowIdentities are the identity strings of row spark (§4.10.1 and
// H-7 5): name, host with and without scheme and port, hostname, model,
// basename, the model's prefixes at - boundaries, provider class.
var sanFR2RowIdentities = []string{
	"spark", "http://Spark-GPU.internal.lan:30000", "Spark-GPU.internal.lan:30000", "spark-gpu.internal.lan",
	"meta-llama/Llama-3.3-70B-Instruct-Turbo", "Llama-3.3-70B-Instruct-Turbo", "meta-llama/Llama-3.3-70B",
	"Llama-3.3-70B-Instruct", "Llama-3.3-70B", "Llama-3.3", "openrouter",
}

// TestSanitizeNoRowIdentitySurvivesHostPrefix (F2-6 H-7 4): spark.internal is
// no match of host spark-gpu.internal.lan, but it carries the row name; no
// identity of the row survives. Over-replacement (today chat.internal) is
// allowed — the pin says nothing about what stays.
func TestSanitizeNoRowIdentitySurvivesHostPrefix(t *testing.T) {
	sanNoneIn(t, []sanCase{
		{"name as host prefix", "node spark.internal refused"},
		{"name as hostname label", "node spark-01.lan refused"},
	}, sanFR2RowIdentities...)
}

// TestSanitizeModelIDPrefix (F2-6 H-7 5): every prefix of the row model at
// a - boundary that still has two segments is an identity — of the
// basename and of the full id with its namespace.
func TestSanitizeModelIDPrefix(t *testing.T) {
	sanNoneIn(t, []sanCase{
		{"basename prefix", "model Llama-3.3-70B is overloaded"},
		{"shortest prefix", "model Llama-3.3 is overloaded"},
		{"longest prefix", "model Llama-3.3-70B-Instruct is overloaded"},
		{"full id prefix", "model meta-llama/Llama-3.3-70B is overloaded"},
		{"namespace", "namespace meta-llama is not served"},
	}, "llama", "3.3", "70b", "instruct")
}

// BenchmarkSanitizeUpstreamText512 is the cost of one relayed 4xx text
// (≤ 512 B by the reader) over the probe pool and over a 20-row pool with
// four model_map entries a row (the live pool size).
func BenchmarkSanitizeUpstreamText512(b *testing.B) {
	text := "upstream spark at http://Spark-GPU.internal.lan:30000 (user:s3cret@spark-gpu.internal.lan) rejected model " +
		"meta-llama/Llama-3.3-70B-Instruct-Turbo: Llama-3.3-70B is overloaded; peer fe80::1%eth0 and ::ffff:10.1.2.3 " +
		"refused, see /srv/models/config.json and C:\\models\\x.gguf; "
	for len(text) < 512 {
		text += "the prompt is too long for the context window of this model. "
	}
	text = text[:512]
	pool := make([]backends.Backend, 0, 20)
	for i := range 20 {
		mm := map[string]backends.ModelSpec{}
		for j := range 4 {
			mm[fmt.Sprintf("role%d", j)] = backends.ModelSpec{Model: fmt.Sprintf("org%d/Model-%d.%d-%dB-Instruct-v%d", i, i, j, 8*j+8, j)}
		}
		pool = append(pool, backends.Backend{
			Name: fmt.Sprintf("row-%02d", i), Host: fmt.Sprintf("http://gpu-%02d.internal.lan:%d", i, 30000+i),
			Model: fmt.Sprintf("vendor%d/Base-%d-27B-Chat", i, i), ModelMap: mm, ProviderClass: "openai",
		})
	}
	for _, p := range []struct {
		name string
		snap []backends.Backend
	}{{"probe-pool", sanFR2Snap()}, {"pool-20x4", pool}} {
		b.Run(p.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = oai.SanitizeUpstreamText(text, "chat", p.snap)
			}
		})
	}
}

// sanLeaks fails when the sanitized text of in over rows still carries one
// of the fragments (case-insensitive).
func sanLeaks(t *testing.T, rows []backends.Backend, in string, fragments ...string) {
	t.Helper()
	got := oai.SanitizeUpstreamText(in, "chat", rows)
	for _, f := range fragments {
		if strings.Contains(strings.ToLower(got), strings.ToLower(f)) {
			t.Errorf("sanitize(%q) = %q, still carries %q", in, got, f)
		}
	}
}

// TestSanitizeFoldOrderKeepsTheLongestIdentity (Fremd-Review 3 R3-2): the
// alternation is sorted by the (?i) fold, not by strings.ToLower — a row
// name "ſpark" (long s, folds to s) must not win over the host "spark-gpu"
// it is a fold prefix of; the Kelvin sign likewise.
func TestSanitizeFoldOrderKeepsTheLongestIdentity(t *testing.T) {
	sanLeaks(t, []backends.Backend{{Name: "ſpark", Host: "http://spark-gpu:8000", Model: "m1"}}, "error at spark-gpu now", "gpu")
	sanLeaks(t, []backends.Backend{{Name: "Kube", Host: "http://kube-node-a:8000", Model: "m1"}}, "error at kube-node-a now", "node-a")
}

// TestSanitizeZoneOfAnIdentityAddress (Fremd-Review 3 R3-3): an IPv6 address
// that is itself a row's identity is replaced by the alias — its zone index
// goes too (URI form %25 and plain).
func TestSanitizeZoneOfAnIdentityAddress(t *testing.T) {
	sanLeaks(t, []backends.Backend{{Name: "gpu1", Host: "http://[fd00::17]:8000", Model: "m1"}},
		"connect fd00::17%25en0 refused, also fd00::17%eth1", "en0", "eth1", "fd00")
}

// TestSanitizeHostFormsWithoutPortAndWithPath (Fremd-Review 3 R3-4): the
// bare hostname of a host with a port, and the host with its path (scheme
// stripped), are identities of the row.
func TestSanitizeHostFormsWithoutPortAndWithPath(t *testing.T) {
	sanLeaks(t, []backends.Backend{{Name: "r1", Host: "http://GPU-RTX.CORP.EXAMPLE:8000", Model: "m1"}},
		"dial GPU-RTX.CORP.EXAMPLE failed", "gpu-rtx", "corp.example")
	sanLeaks(t, []backends.Backend{{Name: "r2", Host: "http://spark-gpu:8000/v1beta", Model: "m1"}},
		"POST spark-gpu:8000/v1beta/chat failed", "v1beta", "spark-gpu")
}
