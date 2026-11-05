package federation

// F-W9 G1/G2 (design/04 §4.6 "Fail-closed ohne Policy", §4.7, §5.3 D1/D2/D6;
// Findings 2/25): the policy of a foreign prompt on A.
//
//	go test -short -count=1 -run 'TestResolvePolicy|TestScanRaise|TestPolicyFrom' -v ./internal/federation/

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// awsKey is a credentials fixture the scanner recognises (AKIA + 16 base32),
// built at run time so no secret scanner flags the source.
func awsKey() string { return "AKIA" + strings.Repeat("Z", 16) }

func consumer(kind auth.ConsumerKind, sens string) *auth.ConsumerAuth {
	return &auth.ConsumerAuth{ConsumerID: "c-" + string(kind) + "-" + sens, Kind: kind, PromptSensitivity: sens, IsValid: true}
}

// TestResolvePolicyHeaderRaisesNeverLowers is G1 (a): Ctx-Sensitivity raises
// the consumer's floor and never lowers it — a credentials consumer that
// sends "public" stays credentials (mutation "header replaces" ⇒ red).
func TestResolvePolicyHeaderRaisesNeverLowers(t *testing.T) {
	for _, c := range []struct {
		name       string
		ca         *auth.ConsumerAuth
		hdr        string
		want       backends.Sensitivity
		egressLoc  bool
		wantEgress bool
	}{
		{"peer credentials + public", consumer(auth.KindCtxPeer, "credentials"), "public", backends.SensCredentials, false, false},
		{"app credentials + public", consumer(auth.KindApp, "credentials"), "public", backends.SensCredentials, true, true},
		{"peer internal + personal", consumer(auth.KindCtxPeer, "internal"), "personal", backends.SensPersonal, false, false},
		{"peer personal + internal", consumer(auth.KindCtxPeer, "personal"), "internal", backends.SensPersonal, false, false},
		{"unknown floor + public", consumer(auth.KindApp, "nonsense"), "public", backends.SensCredentials, false, false},
		{"empty floor + public", consumer(auth.KindApp, ""), "public", backends.SensCredentials, false, false},
		{"peer public + unknown header", consumer(auth.KindCtxPeer, "public"), "loud", backends.SensCredentials, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pol := ResolvePolicy(c.ca, fedhop.Inbound{Sensitivity: c.hdr, EgressLocal: c.egressLoc})
			if pol.Required != c.want || pol.EgressLocal != c.wantEgress {
				t.Errorf("ResolvePolicy = %+v, want {Required:%s EgressLocal:%v}", pol, c.want, c.wantEgress)
			}
		})
	}
}

// TestResolvePolicyMissingHeaderKeepsFloor is G1 (b), Finding 25: a consumer
// that server-admin lowered to personal, without Ctx-Sensitivity and with a
// harmless prompt, stays personal — the Rev.-0 merge ("missing header ⇒
// credentials") would make every lowering void.
func TestResolvePolicyMissingHeaderKeepsFloor(t *testing.T) {
	pol := ResolvePolicy(consumer(auth.KindCtxPeer, "personal"), fedhop.Inbound{})
	if pol.Required != backends.SensPersonal {
		t.Fatalf("personal consumer, no header: Required = %q, want personal", pol.Required)
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"What is the capital of France?"}]}`)
	if got := ScanRaise(pol.Required, body); got != backends.SensPersonal {
		t.Errorf("harmless prompt: ScanRaise = %q, want personal (the scan only raises on a hit)", got)
	}
}

// TestResolvePolicyAppHeaderRaises is G1 (d), lead decision 1: the header
// raises for every consumer — an app that says credentials gets credentials.
func TestResolvePolicyAppHeaderRaises(t *testing.T) {
	pol := ResolvePolicy(consumer(auth.KindApp, "public"), fedhop.Inbound{Sensitivity: "credentials"})
	if pol.Required != backends.SensCredentials {
		t.Fatalf("app public + Ctx-Sensitivity credentials: Required = %q, want credentials", pol.Required)
	}
}

// TestResolvePolicyNilConsumer: without a consumer the policy is the
// fail-closed default — PeerIngress refuses that case anyway (503).
func TestResolvePolicyNilConsumer(t *testing.T) {
	if pol := ResolvePolicy(nil, fedhop.Inbound{Sensitivity: "public"}); pol != (Policy{Required: backends.SensCredentials, EgressLocal: true}) {
		t.Fatalf("ResolvePolicy(nil) = %+v, want {credentials true}", pol)
	}
}

// TestScanRaiseCredentialsInEveryInput is G1 (c), Finding 2 / D6: a lowered
// floor plus credentials anywhere in the request — tool-call arguments of a
// chat request, a rerank document, an embedding input, a tool result —
// raises to credentials (mutation "scan removed" ⇒ red).
func TestScanRaiseCredentialsInEveryInput(t *testing.T) {
	key := awsKey()
	args, _ := json.Marshal(map[string]string{"access_key": key})
	chat, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "deploy it"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "deploy", "arguments": string(args)},
			}}},
		},
	})
	rerank, _ := json.Marshal(map[string]any{"model": "r", "query": "which one", "documents": []string{"first", "the key is " + key}})
	embed, _ := json.Marshal(map[string]any{"model": "e", "input": []string{"a", "b", key}})
	tool, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "tool", "tool_call_id": "call_1", "content": key}}})
	// A secret-named key next to its value: two JSON tokens, one assignment
	// for the scanner (Fremd-Review FR-3) — in metadata and in a rerank
	// document given as an object.
	secret := "Zq8vX2mL9pR4wT7yB3nK"
	meta, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "metadata": map[string]any{"client_secret": secret}})
	docObj, _ := json.Marshal(map[string]any{"model": "r", "query": "q", "documents": []any{map[string]any{"password": secret}}})
	parts, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "see " + key},
	}}}})
	ca := consumer(auth.KindCtxPeer, "personal")
	for name, body := range map[string][]byte{
		"chat tool_calls[0].function.arguments": chat,
		"rerank document":                       rerank,
		"embedding input":                       embed,
		"role tool":                             tool,
		"content part":                          parts,
		"metadata client_secret member":         meta,
		"rerank document object member":         docObj,
	} {
		t.Run(name, func(t *testing.T) {
			pol := ResolvePolicy(ca, fedhop.Inbound{})
			if pol.Required != backends.SensPersonal {
				t.Fatalf("precondition: Required = %q, want personal", pol.Required)
			}
			if got := ScanRaise(pol.Required, body); got != backends.SensCredentials {
				t.Errorf("ScanRaise = %q, want credentials (the scan must see %s)", got, name)
			}
		})
	}
}

// TestScanRaiseEscapedAndInvalidBodies: the scan reads decoded leaves, so a
// JSON escape cannot hide a key, and a body that is not JSON is scanned raw
// (fail-closed: an unparsable body never skips the scan).
func TestScanRaiseEscapedAndInvalidBodies(t *testing.T) {
	key := awsKey()
	// A real JSON escape in the body bytes (raw string: the backslash
	// sequence reaches the decoder, not the Go compiler — Fremd-Review
	// FR-4): a raw scan misses it, the decoded leaf does not.
	escaped := []byte(`{"input":"\u0041` + key[1:] + `"}`)
	if !bytes.Contains(escaped, []byte(`\u0041`)) {
		t.Fatalf("fixture %q carries no escape", escaped)
	}
	if _, raw := sensitivity.Scan(string(escaped)); raw {
		t.Fatalf("fixture %q is found by a raw scan — it does not test the decoding", escaped)
	}
	if got := ScanRaise(backends.SensPublic, escaped); got != backends.SensCredentials {
		t.Errorf("escaped key: %q, want credentials", got)
	}
	broken := []byte(`{"input":"` + key + `"`)
	if got := ScanRaise(backends.SensPublic, broken); got != backends.SensCredentials {
		t.Errorf("truncated JSON with a key: %q, want credentials", got)
	}
	if got := ScanRaise(backends.SensPublic, []byte(`{"input":"hello"}`)); got != backends.SensPublic {
		t.Errorf("harmless public: %q, want public", got)
	}
}

// TestScanRaiseRawScanOnParseError is F2-1 (Fremd-Review 2): a body the
// decoder cannot read is scanned raw, never skipped. Each fixture carries its
// secret only BEHIND the parse error, so no token before it can hit — a scan
// that gave up at the error (mutant M10) answers personal.
func TestScanRaiseRawScanOnParseError(t *testing.T) {
	const secret = "Zq7Lp2Wx9Kd4Rv8Tn3Hb6Yc1"
	for name, body := range map[string]string{
		"not JSON":                    `password: ` + secret,
		"valid JSON, then garbage":    `{"a":"b"} password=` + secret,
		"missing comma before secret": `{"a":"x" "password=` + secret + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ScanRaise(backends.SensPersonal, []byte(body)); got != backends.SensCredentials {
				t.Errorf("ScanRaise(personal, %q) = %q, want credentials (raw scan after the parse error)", body, got)
			}
		})
	}
}

// TestScanRaiseObjectKeys is F2-2 (Fremd-Review 2): object keys are scanned
// like values — a credential used as a key (metadata maps keyed by an access
// key id) raises; with the key scan off (mutant M15) it would not, the value
// beside it being a bool.
func TestScanRaiseObjectKeys(t *testing.T) {
	body := []byte(`{"metadata":{"` + awsKey() + `":true}}`)
	if got := ScanRaise(backends.SensPersonal, body); got != backends.SensCredentials {
		t.Fatalf("AWS key as an object key: %q, want credentials", got)
	}
}

// TestScanRaiseMemberProbeScalarsOnly pins a NAMED LIMIT (F2-3, Fremd-Review
// 2; lead decision: no rework in F-W9): the key-value probe of FR-3 covers an
// object member with a scalar value only. A secret-named key over an array or
// an object value is not paired with the strings inside it, so a short secret
// without a vendor prefix there stays at the lowered floor — as it would
// under a raw scan of the serialized body (the assignment rule's capture ends
// at "[" or "{"). The scalar control raises. If this test turns red because
// the containers now raise too, the limit is closed: update docs/security.md
// and the handover in the F-W9 report.
func TestScanRaiseMemberProbeScalarsOnly(t *testing.T) {
	const secret = "Zq7Lp2Wx9Kd4Rv8Tn3Hb6Yc1"
	for _, c := range []struct {
		body string
		want backends.Sensitivity
	}{
		{`{"api_key":"` + secret + `"}`, backends.SensCredentials},
		{`{"api_key":["` + secret + `"]}`, backends.SensPersonal},
		{`{"api_key":{"value":"` + secret + `"}}`, backends.SensPersonal},
	} {
		if got := ScanRaise(backends.SensPersonal, []byte(c.body)); got != c.want {
			t.Errorf("ScanRaise(personal, %s) = %q, want %q", c.body, got, c.want)
		}
	}
}

// TestScanRaiseShortCircuit: at credentials there is nothing left to raise —
// no scan; an empty or unknown requirement is credentials (fail-closed).
func TestScanRaiseShortCircuit(t *testing.T) {
	for _, req := range []backends.Sensitivity{backends.SensCredentials, "", "loud"} {
		if got := ScanRaise(req, []byte(`{"input":"hello"}`)); got != backends.SensCredentials {
			t.Errorf("ScanRaise(%q) = %q, want credentials", req, got)
		}
	}
}

// TestScanRaiseCost8MiB measures the scan at the gateway's request cap
// (gateway.max_request_bytes default 8 MiB) for two body shapes: one large
// content string and 65 536 small leaves of 128 bytes. A measurement, not a
// timing gate — the numbers go to the wave report.
func TestScanRaiseCost8MiB(t *testing.T) {
	if testing.Short() && !testing.Verbose() {
		t.Skip("measurement: run with -v")
	}
	const capBytes = 8 << 20
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", capBytes/45)
	one, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": text}}})
	leaves := make([]string, 65536)
	for i := range leaves {
		leaves[i] = strings.Repeat("lorem ipsum ", 10) + "dolor sit"
	}
	many, _ := json.Marshal(map[string]any{"model": "e", "input": leaves})
	for name, body := range map[string][]byte{"one 8 MiB string": one, "65536 leaves": many} {
		start := time.Now()
		got := ScanRaise(backends.SensPublic, body)
		t.Logf("scan cost: %s, %d bytes: %v (result %s)", name, len(body), time.Since(start), got)
	}
}

// TestPolicyFromWithoutPolicy is G2 (design/04 §4.6, Finding 4.b): a handler
// reached without PeerIngress works in the narrowest mode — credentials and
// egress local — never in the zero value's public + any.
func TestPolicyFromWithoutPolicy(t *testing.T) {
	got := PolicyFrom(context.Background())
	if got.Required != backends.SensCredentials || !got.EgressLocal {
		t.Fatalf("PolicyFrom(no policy) = %+v, want {Required:credentials EgressLocal:true}", got)
	}
	set := Policy{Required: backends.SensInternal, EgressLocal: false}
	if got := PolicyFrom(WithPolicy(context.Background(), set)); got != set {
		t.Errorf("PolicyFrom(WithPolicy(%+v)) = %+v", set, got)
	}
	// A hand-built policy with an unknown requirement reads as credentials.
	if got := PolicyFrom(WithPolicy(context.Background(), Policy{Required: "", EgressLocal: false})); got.Required != backends.SensCredentials {
		t.Errorf("PolicyFrom(Required \"\") = %+v, want Required credentials", got)
	}
}

// TestAssignmentProbe: the value part of the member probe is the whole first
// token the scanner's assignment rule would capture — leading whitespace and
// a quote skipped, cut at whitespace, quote, comma or semicolon, never cut by
// length (bot review on PR #126) — so a long prose value costs a word of
// probe, not a second scan.
func TestAssignmentProbe(t *testing.T) {
	long := strings.Repeat("x", 768)
	for in, want := range map[string]string{
		"Zq8vX2mL9pR4wT7yB3nK":            "Zq8vX2mL9pR4wT7yB3nK",
		"  \"Zq8vX2mL9pR4wT7yB3nK\" rest": "Zq8vX2mL9pR4wT7yB3nK",
		"lorem ipsum dolor":               "lorem",
		"a,b;c":                           "a",
		"":                                "",
		long:                              long,
	} {
		if got := assignmentProbe(in); got != want {
			t.Errorf("assignmentProbe(%.40q) = %.40q, want %.40q", in, got, want)
		}
	}
	// The probe keeps the hit of a secret value behind leading whitespace.
	body := []byte(`{"metadata":{"api_key":"   Zq8vX2mL9pR4wT7yB3nK"}}`)
	if got := ScanRaise(backends.SensPublic, body); got != backends.SensCredentials {
		t.Errorf("api_key with leading spaces: %q, want credentials", got)
	}
}
