package llmlog

// W06-9 unit gates (design/06 §4.6, masterplan K10/K26, §8 d01 (3), d02 (6)):
// the /v1 row vocabulary, the metadata.gw v1 stamp, RedactedErr and the one
// constructor GatewayEntry with its body policy. The database half (real
// inserts, the CHECK, the rollup) is gateway_integration_test.go; the body
// states over classifyBodies are internal/handler's
// llmlog_gateway_integration_test.go.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/redact"
)

const (
	gwTestConsumer = "0195a6f0-0000-7000-8000-00000000c901"
	gwTestModelKey = "0195a6f0-0000-7000-8000-00000000d901"
	gwTestKnowKey  = "0195a6f0-0000-7000-8000-00000000a901"
	gwTestCanary   = "PROMPT-CANARY-7f3c1e"
)

// TestGatewayVocabularyPin pins the /v1 row vocabulary exactly, like the
// abort vocabulary: the strings are the persisted contract the rollup, the
// usage readers and W02-07 share, so a rename is red here first.
func TestGatewayVocabularyPin(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(gatewayPipelineOps)), []string{"gateway-chat", "gateway-embeddings", "gateway-rerank"}; !slices.Equal(got, want) {
		t.Fatalf("gateway pipelines %q, want %q (gateway-responses only with W02-13)", got, want)
	}
	for pipeline, op := range map[string]string{
		PipelineGatewayChat:       "chat",
		PipelineGatewayEmbeddings: "embed",
		PipelineGatewayRerank:     "rerank",
		"gateway-responses":       "",
		"dream-eval":              "",
		"":                        "",
	} {
		if got := GatewayOp(pipeline); got != op {
			t.Errorf("GatewayOp(%q) = %q, want %q", pipeline, got, op)
		}
	}
	for got, want := range map[string]string{
		GatewayOpChat: "chat", GatewayOpEmbed: "embed", GatewayOpRerank: "rerank",
		FinishStop: "stop", FinishLength: "length", FinishToolCalls: "tool_calls",
		FinishContentFilter: "content_filter", FinishError: "error", FinishAborted: "aborted",
		UsageBackend: "backend", UsageEstimated: "estimated",
		BodyLoggingNone: "none", BodyLoggingErrors: "errors", BodyLoggingFull: "full",
	} {
		if got != want {
			t.Errorf("vocabulary constant %q, want %q", got, want)
		}
	}
	for _, p := range []string{"none", "errors", "full"} {
		if !BodyLoggingValid(p) {
			t.Errorf("BodyLoggingValid(%q) = false", p)
		}
	}
	for _, p := range []string{"", "None", " full", "all", "bodies"} {
		if BodyLoggingValid(p) {
			t.Errorf("BodyLoggingValid(%q) = true — config normalizes, the vocabulary is exact", p)
		}
	}
	if GatewayMetadataVersion != 1 {
		t.Errorf("metadata.gw version %d, want 1", GatewayMetadataVersion)
	}
	if gatewayBodyCap != 65536 {
		t.Errorf("body cap %d, want 64 KiB (design/06 §4.6)", gatewayBodyCap)
	}
}

// gwJSON is metadata as the database receives it (pgx marshals the map).
func gwJSON(t *testing.T, e Entry) map[string]any {
	t.Helper()
	raw, err := json.Marshal(e.Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return out
}

func gwObject(t *testing.T, e Entry) map[string]any {
	t.Helper()
	gw, ok := gwJSON(t, e)["gw"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.gw missing or not an object: %v", e.Metadata)
	}
	return gw
}

func fullGateway() Gateway {
	ttft := int64(212)
	return Gateway{
		Op:           GatewayOpChat,
		Alias:        "team-chat",
		Stream:       true,
		Hop:          2,
		ClaimedChain: []string{"0195a6f0-0000-7000-8000-0000000000e1", "0195a6f0-0000-7000-8000-0000000000e2"},
		PeerPipeline: "dream-eval",
		Usage:        UsageEstimated,
		TTFTMs:       &ttft,
		ClientReqID:  "req_01:abc-9.Z",
		Finish:       FinishStop,
		ModelKeyID:   gwTestModelKey,
	}
}

// TestStampGatewayShape is the metadata.gw v1 schema: exactly the keys the
// rollup trigger and the readers expect, never origin (K26), never a
// metadata.consumer_id (§8 d02 (6)), and the consumer in the column.
func TestStampGatewayShape(t *testing.T) {
	e := Entry{Pipeline: PipelineGatewayChat, Metadata: map[string]any{"consumer_id": gwTestConsumer, "ignored_params": []string{"user"}}}
	e.StampGateway(gwTestConsumer, fullGateway())
	if e.ConsumerID != gwTestConsumer || e.APIKeyID != "" {
		t.Fatalf("identity: consumer %q api_key %q", e.ConsumerID, e.APIKeyID)
	}
	md := gwJSON(t, e)
	if _, ok := md["consumer_id"]; ok {
		t.Errorf("metadata.consumer_id survived the stamp (§8 d02 (6): the column is the attribution)")
	}
	if _, ok := md["ignored_params"]; !ok {
		t.Errorf("the stamp dropped a foreign metadata key")
	}
	gw := gwObject(t, e)
	keys := slices.Sorted(maps.Keys(gw))
	want := []string{"alias", "claimed_chain", "client_req_id", "finish", "hop", "model_key_id", "op", "peer_pipeline", "stream", "ttft_ms", "usage", "v"}
	if !slices.Equal(keys, want) {
		t.Fatalf("metadata.gw keys %q, want %q", keys, want)
	}
	for k, v := range map[string]any{
		"v": 1.0, "op": "chat", "alias": "team-chat", "stream": true, "hop": 2.0,
		"peer_pipeline": "dream-eval", "usage": "estimated", "ttft_ms": 212.0,
		"client_req_id": "req_01:abc-9.Z", "finish": "stop", "model_key_id": gwTestModelKey,
	} {
		if gw[k] != v {
			t.Errorf("metadata.gw.%s = %#v, want %#v", k, gw[k], v)
		}
	}
	if chain, _ := gw["claimed_chain"].([]any); len(chain) != 2 {
		t.Errorf("claimed_chain = %#v, want the two ids", gw["claimed_chain"])
	}

	// The zero Gateway still says which version and which op it is and
	// leaves out what was not measured.
	z := Entry{Pipeline: PipelineGatewayEmbeddings}
	z.StampGateway(gwTestConsumer, Gateway{Op: GatewayOpEmbed})
	if got := slices.Sorted(maps.Keys(gwObject(t, z))); !slices.Equal(got, []string{"hop", "op", "stream", "v"}) {
		t.Errorf("zero gateway keys %q, want [hop op stream v]", got)
	}
}

// captureSlog routes the default logger into a buffer for one test.
func captureSlog(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestStampGatewayClearsAPIKeyID is B17: a gateway row never carries a
// knowledge key; the stamp clears one that a caller left on the entry and
// says so at ERROR (the CHECK would refuse the row otherwise).
func TestStampGatewayClearsAPIKeyID(t *testing.T) {
	buf := captureSlog(t, slog.LevelDebug)
	e := Entry{Pipeline: PipelineGatewayChat, APIKeyID: gwTestKnowKey}
	e.StampGateway(gwTestConsumer, Gateway{Op: GatewayOpChat})
	if e.APIKeyID != "" || e.ConsumerID != gwTestConsumer {
		t.Fatalf("api_key %q consumer %q, want the key cleared and the consumer set", e.APIKeyID, e.ConsumerID)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "api_key_id") {
		t.Fatalf("no ERROR naming api_key_id: %q", out)
	}
	if strings.Contains(out, gwTestKnowKey) {
		t.Errorf("the ERROR line echoes the knowledge key id: %q", out)
	}
	buf.Reset()
	ok := Entry{Pipeline: PipelineGatewayChat}
	ok.StampGateway(gwTestConsumer, Gateway{Op: GatewayOpChat})
	if buf.Len() != 0 {
		t.Errorf("a clean stamp logged: %q", buf.String())
	}
}

// TestStampGatewayBoundsValues: every value of metadata.gw is bounded and
// well-formed before it reaches the JSONB column — an out-of-range number
// is clamped like the rollup clamps it, a malformed token or id is dropped
// (a NUL would make PostgreSQL refuse the whole row).
func TestStampGatewayBoundsValues(t *testing.T) {
	ids := make([]string, 0, 20)
	for i := range 20 {
		ids = append(ids, "0195A6F0-0000-7000-8000-0000000001"+string(rune('a'+i%6))+string(rune('0'+i%10)))
	}
	ids = append([]string{"not-a-uuid", ""}, ids...)
	neg := int64(-5)
	g := Gateway{
		Op:           "embeddings",
		Alias:        strings.Repeat("ä", 200),
		Stream:       false,
		Hop:          17,
		ClaimedChain: ids,
		PeerPipeline: "dream\x00eval",
		Usage:        "none",
		TTFTMs:       &neg,
		ClientReqID:  "has space",
		Finish:       "done",
		ModelKeyID:   "mk-1",
	}
	e := Entry{Pipeline: PipelineGatewayChat}
	e.StampGateway(gwTestConsumer, g)
	gw := gwObject(t, e)
	for _, k := range []string{"op", "peer_pipeline", "usage", "ttft_ms", "client_req_id", "finish", "model_key_id"} {
		if v, ok := gw[k]; ok {
			t.Errorf("metadata.gw.%s = %#v kept, want it dropped", k, v)
		}
	}
	if gw["hop"] != 16.0 {
		t.Errorf("hop = %v, want clamped to 16", gw["hop"])
	}
	if a, _ := gw["alias"].(string); len([]rune(a)) != gatewayAliasMax {
		t.Errorf("alias %d runes, want cut to %d", len([]rune(a)), gatewayAliasMax)
	}
	chain, _ := gw["claimed_chain"].([]any)
	if len(chain) != gatewayClaimedChainMax {
		t.Fatalf("claimed_chain %d entries, want %d", len(chain), gatewayClaimedChainMax)
	}
	for _, c := range chain {
		if s, _ := c.(string); s != strings.ToLower(s) || len(s) != 36 {
			t.Errorf("claimed_chain entry %q not canonical", s)
		}
	}

	// Negative hop clamps to 0; a TTFT on a non-stream row is dropped; the
	// TTFT of a stream row is kept, and 0 is a measurement.
	zero := int64(0)
	e2 := Entry{Pipeline: PipelineGatewayChat}
	e2.StampGateway(gwTestConsumer, Gateway{Op: GatewayOpChat, Hop: -3, Stream: true, TTFTMs: &zero})
	gw2 := gwObject(t, e2)
	if gw2["hop"] != 0.0 || gw2["ttft_ms"] != 0.0 {
		t.Errorf("hop %v ttft %v, want 0 and 0", gw2["hop"], gw2["ttft_ms"])
	}
	long := strings.Repeat("a", gatewayTokenMax+1)
	e3 := Entry{Pipeline: PipelineGatewayChat}
	e3.StampGateway(gwTestConsumer, Gateway{Op: GatewayOpChat, ClientReqID: long, PeerPipeline: long[:gatewayTokenMax]})
	gw3 := gwObject(t, e3)
	if _, ok := gw3["client_req_id"]; ok {
		t.Errorf("client_req_id of %d bytes kept", len(long))
	}
	if gw3["peer_pipeline"] != long[:gatewayTokenMax] {
		t.Errorf("peer_pipeline of exactly %d bytes dropped", gatewayTokenMax)
	}
}

// TestRedactedErr: Error() is class, status and code — and nothing a
// caller could smuggle a body through.
func TestRedactedErr(t *testing.T) {
	for _, tc := range []struct {
		err  *RedactedErr
		want string
	}{
		{&RedactedErr{Class: "bad_request", Status: 400, Code: "upstream_rejected"}, "bad_request 400 upstream_rejected"},
		{&RedactedErr{Class: "busy", Status: 503}, "busy 503"},
		{&RedactedErr{Code: "upstream_timeout"}, "upstream_timeout"},
		{&RedactedErr{}, "error"},
		{nil, "error"},
		{&RedactedErr{Class: "rejected: " + gwTestCanary, Status: 400, Code: gwTestCanary + " says hi"}, redact.Redacted + " 400 " + redact.Redacted},
		{&RedactedErr{Class: strings.Repeat("x", redactedTokenMax+1), Code: "ok"}, redact.Redacted + " ok"},
		{&RedactedErr{Class: "http_4xx", Status: 400, Code: gwTestCanary}, "http_4xx 400 " + redact.Redacted},
		{&RedactedErr{Class: strings.Repeat("x", redactedTokenMax), Code: "a_b_9"}, strings.Repeat("x", redactedTokenMax) + " a_b_9"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%#v.Error() = %q, want %q", tc.err, got, tc.want)
		}
	}
	var err error = &RedactedErr{Class: "x", Status: 1}
	if !strings.Contains(err.Error(), "x 1") {
		t.Errorf("RedactedErr does not satisfy error usefully: %q", err.Error())
	}
}

// gwRow is a complete GatewayRow as W02-07 would hand it over.
func gwRow(policy string, rerr *RedactedErr, sensitivity string) GatewayRow {
	qw := int64(7)
	return GatewayRow{
		Pipeline:   PipelineGatewayChat,
		ConsumerID: gwTestConsumer,
		Gateway:    Gateway{Alias: "team-chat", ModelKeyID: gwTestModelKey, Finish: FinishStop, Usage: UsageBackend},
		Telemetry: Entry{
			Model: "served-model", Host: "spark", Duration: 1500 * time.Millisecond,
			PromptTokens: 11, CompletionTokens: 22,
			BackendName: "spark-chat", BackendTrust: "full-trust", BackendLocality: "lan",
			RequiredSensitivity: sensitivity, QueueWaitMs: &qw, DispatchClass: "external",
			// What a careless caller might leave on the walk's entry: the raw
			// upstream error and bodies. GatewayEntry must not persist them.
			Err:             &httpx.StatusError{Code: 400, Body: "upstream says " + gwTestCanary},
			RequestSystem:   gwTestCanary,
			RequestUser:     gwTestCanary,
			ResponseContent: gwTestCanary,
		},
		Attempts:    []WireAttempt{{Backend: "spark-chat", Class: "", Ms: 1500, WaitMs: 7}},
		Err:         rerr,
		Request:     `{"messages":[{"role":"user","content":"hello"}]}`,
		Response:    `{"content":"world"}`,
		BodyLogging: policy,
	}
}

// TestGatewayEntryBodyPolicy is the body half of B-06-10 at the
// constructor: none ⇒ '' bodies, errors ⇒ bodies only on error rows, full ⇒
// the serialized request and response; Slimmed(false) on every row, so a
// credentials-class row stays sealed under every policy; the entry's own
// bodies (whatever the walk left there) never reach the row.
func TestGatewayEntryBodyPolicy(t *testing.T) {
	rerr := &RedactedErr{Class: "bad_request", Status: 400, Code: "upstream_rejected"}
	captureSlog(t, slog.LevelDebug)
	for _, tc := range []struct {
		name, policy, sensitivity string
		err                       *RedactedErr
		bodies                    bool
	}{
		{"none, success", BodyLoggingNone, "internal", nil, false},
		{"none, error", BodyLoggingNone, "internal", rerr, false},
		{"errors, success", BodyLoggingErrors, "internal", nil, false},
		{"errors, error", BodyLoggingErrors, "internal", rerr, true},
		{"full, success", BodyLoggingFull, "internal", nil, true},
		{"full, error", BodyLoggingFull, "internal", rerr, true},
		{"full, credentials", BodyLoggingFull, "credentials", nil, false},
		{"errors, credentials error", BodyLoggingErrors, "credentials", rerr, false},
		{"full, public", BodyLoggingFull, "public", nil, true},
		{"full, personal", BodyLoggingFull, "personal", nil, true},
		{"full, empty sensitivity is credentials", BodyLoggingFull, "", nil, false},
		{"full, unknown sensitivity is credentials", BodyLoggingFull, "Credentials", nil, false},
		{"errors, unknown sensitivity error", BodyLoggingErrors, "secret", rerr, false},
		{"unknown policy is none", "everything", "internal", rerr, false},
		{"empty policy is none", "", "internal", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := GatewayEntry(gwRow(tc.policy, tc.err, tc.sensitivity))
			if e.RequestSystem != "" {
				t.Errorf("request_system = %q, want '' (the serialized request goes to request_user)", e.RequestSystem)
			}
			if tc.bodies {
				if e.RequestUser == "" || e.ResponseContent == "" {
					t.Errorf("bodies empty (%q / %q), want the serialized request and response", e.RequestUser, e.ResponseContent)
				}
			} else if e.RequestUser != "" || e.ResponseContent != "" {
				t.Errorf("bodies %q / %q, want ''", e.RequestUser, e.ResponseContent)
			}
			for _, s := range []string{e.RequestSystem, e.RequestUser, e.ResponseContent} {
				if strings.Contains(s, gwTestCanary) {
					t.Errorf("a body carries the canary of the entry's own fields: %q", s)
				}
			}
		})
	}
}

// TestGatewayEntryUndefinedSensitivityLogs: a row whose sensitivity the
// writer left empty is stored without bodies AND reported, once per
// interval — the fail-closed seal must not hide the wiring defect.
func TestGatewayEntryUndefinedSensitivityLogs(t *testing.T) {
	buf := captureSlog(t, slog.LevelDebug)
	lastSensWarn.Store(0)
	t.Cleanup(func() { lastSensWarn.Store(0) })
	for range 3 {
		if e := GatewayEntry(gwRow(BodyLoggingFull, nil, "")); e.RequestUser != "" || e.ResponseContent != "" {
			t.Fatalf("bodies stored for an undefined sensitivity")
		}
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("%d ERROR line(s) for three rows, want 1 (throttled): %q", n, buf.String())
	}
	buf.Reset()
	GatewayEntry(gwRow(BodyLoggingNone, nil, ""))
	GatewayEntry(gwRow(BodyLoggingFull, nil, "internal"))
	if buf.Len() != 0 {
		t.Errorf("a row that stores no body, or a defined class, logged: %q", buf.String())
	}
}

// TestGatewayEntryBodyCap: full bodies are cut to 64 KiB with the visible
// truncation marker, on a rune boundary, and cleaned of what PostgreSQL
// refuses in a text column.
func TestGatewayEntryBodyCap(t *testing.T) {
	r := gwRow(BodyLoggingFull, nil, "internal")
	r.Request = strings.Repeat("€", gatewayBodyCap) // 3 bytes each
	r.Response = "ok\x00done\xff"
	e := GatewayEntry(r)
	if len(e.RequestUser) > gatewayBodyCap {
		t.Fatalf("request body %d bytes, cap %d", len(e.RequestUser), gatewayBodyCap)
	}
	if !strings.HasSuffix(e.RequestUser, redact.Truncated) {
		t.Fatalf("a cut body lacks the truncation marker: …%q", e.RequestUser[len(e.RequestUser)-20:])
	}
	if strings.ContainsRune(strings.TrimSuffix(e.RequestUser, redact.Truncated), '�') {
		t.Errorf("the cut split a rune")
	}
	if strings.ContainsRune(e.ResponseContent, 0) || strings.Contains(e.ResponseContent, "\xff") {
		t.Errorf("response body keeps NUL or invalid UTF-8: %q", e.ResponseContent)
	}
	r.Request = strings.Repeat("a", gatewayBodyCap)
	if got := GatewayEntry(r).RequestUser; got != r.Request {
		t.Errorf("a body of exactly the cap was changed (%d bytes)", len(got))
	}
}

// TestGatewayEntryErrIsRedacted is the error half of B-06-10: the error
// column of a /v1 row is RedactedErr or nothing — never the walk's raw
// error, whose StatusError text carries the upstream body.
func TestGatewayEntryErrIsRedacted(t *testing.T) {
	rerr := &RedactedErr{Class: "bad_request", Status: 400, Code: "upstream_rejected"}
	e := GatewayEntry(gwRow(BodyLoggingFull, rerr, "internal"))
	if e.Err == nil || e.Err.Error() != "bad_request 400 upstream_rejected" {
		t.Fatalf("error %v, want the redacted error", e.Err)
	}
	if strings.Contains(e.Err.Error(), gwTestCanary) {
		t.Fatalf("error text carries the upstream canary: %q", e.Err.Error())
	}
	ok := GatewayEntry(gwRow(BodyLoggingFull, nil, "internal"))
	if ok.Err != nil {
		t.Fatalf("a success row keeps the walk's error %v — a typed nil would land as an error row", ok.Err)
	}
}

// TestGatewayEntryOneRow: every physical attempt folds into the ONE row —
// attempt = number of attempts, metadata.chain = the attempts in order.
func TestGatewayEntryOneRow(t *testing.T) {
	r := gwRow(BodyLoggingNone, nil, "internal")
	r.Attempts = []WireAttempt{{Backend: "a", Class: "http_5xx", Ms: 30}, {Backend: "b", Ms: 900, WaitMs: 3}}
	e := GatewayEntry(r)
	if e.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", e.Attempt)
	}
	chain, _ := gwJSON(t, e)["chain"].([]any)
	if len(chain) != 2 {
		t.Fatalf("metadata.chain = %#v, want 2 attempts", gwJSON(t, e)["chain"])
	}
	if first, _ := chain[0].(map[string]any); first["backend"] != "a" || first["err_class"] != "http_5xx" {
		t.Errorf("chain[0] = %#v", chain[0])
	}
	r.Attempts = nil
	r.Telemetry.Metadata = map[string]any{"chain": []WireAttempt{{Backend: "stale"}}}
	e0 := GatewayEntry(r)
	if _, ok := e0.Metadata["chain"]; ok || e0.Attempt != 0 {
		t.Errorf("no attempts: chain %v attempt %d, want neither (Attempts is the one source)", e0.Metadata["chain"], e0.Attempt)
	}
}

// TestGatewayEntryKeepsTelemetry: the constructor sets pipeline, identity,
// metadata.gw, error, bodies and attempts and keeps every other field the
// walk measured; it never writes into the caller's metadata map.
func TestGatewayEntryKeepsTelemetry(t *testing.T) {
	r := gwRow(BodyLoggingNone, nil, "internal")
	callerMD := map[string]any{"oai_request_id": "req-1", "consumer_id": gwTestConsumer}
	r.Telemetry.Metadata = callerMD
	r.Telemetry.APIKeyID = gwTestKnowKey
	r.Telemetry.Pipeline = "dream-eval"
	r.Telemetry.ConsumerID = "0195a6f0-0000-7000-8000-00000000ffff"
	captureSlog(t, slog.LevelDebug)
	e := GatewayEntry(r)
	if e.Pipeline != PipelineGatewayChat || e.ConsumerID != gwTestConsumer || e.APIKeyID != "" {
		t.Fatalf("pipeline %q consumer %q api_key %q", e.Pipeline, e.ConsumerID, e.APIKeyID)
	}
	if e.Model != "served-model" || e.Host != "spark" || e.PromptTokens != 11 || e.CompletionTokens != 22 ||
		e.BackendName != "spark-chat" || e.BackendTrust != "full-trust" || e.BackendLocality != "lan" ||
		e.RequiredSensitivity != "internal" || e.QueueWaitMs == nil || *e.QueueWaitMs != 7 ||
		e.DispatchClass != "external" || e.Duration != 1500*time.Millisecond {
		t.Fatalf("telemetry not kept: %+v", e)
	}
	if gwObject(t, e)["op"] != "chat" {
		t.Errorf("op not derived from the pipeline")
	}
	md := gwJSON(t, e)
	if md["oai_request_id"] != "req-1" {
		t.Errorf("caller metadata key lost: %v", md)
	}
	if _, ok := md["consumer_id"]; ok {
		t.Errorf("metadata.consumer_id kept")
	}
	if _, ok := callerMD["gw"]; ok || callerMD["consumer_id"] == nil {
		t.Errorf("GatewayEntry wrote into the caller's metadata map: %v", callerMD)
	}
}

// TestGatewayEntryFlagsMissingIdentity: a /v1 row without a consumer or
// with a pipeline outside the vocabulary is a wiring defect — logged at
// ERROR, still written (losing the row would hide the defect too).
func TestGatewayEntryFlagsMissingIdentity(t *testing.T) {
	buf := captureSlog(t, slog.LevelDebug)
	r := gwRow(BodyLoggingNone, nil, "internal")
	r.ConsumerID = ""
	e := GatewayEntry(r)
	if e.Pipeline != PipelineGatewayChat || !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("no ERROR for a row without consumer: %q", buf.String())
	}
	buf.Reset()
	r = gwRow(BodyLoggingNone, nil, "internal")
	r.Pipeline = "gateway-responses"
	e = GatewayEntry(r)
	if e.Pipeline != "gateway-responses" || !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("pipeline %q, log %q — want the row kept and an ERROR", e.Pipeline, buf.String())
	}
	if _, ok := gwObject(t, e)["op"]; ok {
		t.Errorf("an unknown pipeline got an op")
	}
}
