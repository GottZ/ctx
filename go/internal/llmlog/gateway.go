package llmlog

import (
	"log/slog"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/redact"
)

// The /v1 row contract (design/06 §4.6, masterplan K10/K26; wave W06-9).
// Every request on the OpenAI-compatible gateway writes exactly ONE
// context_llm_log row, built by GatewayEntry and written by RecordGateway —
// the only way a /v1 row comes to exist. The vocabulary below is the
// persisted contract the usage rollup (migration 159), the usage readers and
// the gateway handlers share; like the dispatch_abort vocabulary, these
// constants plus their pin test ARE the value contract.

// Gateway pipelines. gateway-responses joins with /v1/responses (W02-13).
const (
	PipelineGatewayChat       = "gateway-chat"
	PipelineGatewayEmbeddings = "gateway-embeddings"
	PipelineGatewayRerank     = "gateway-rerank"
)

// metadata.gw.op — the operation class a pipeline belongs to.
const (
	GatewayOpChat   = "chat"
	GatewayOpEmbed  = "embed"
	GatewayOpRerank = "rerank"
)

// metadata.gw.finish — how the request ended for the client.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
	FinishError         = "error"
	FinishAborted       = "aborted"
)

// metadata.gw.usage — where the token columns come from: the backend's own
// usage report, or ctx's estimate when the backend reported none (the
// rollup counts estimated rows, migration 159).
const (
	UsageBackend   = "backend"
	UsageEstimated = "estimated"
)

// The body policy gateway.body_logging (masterplan K10, global-only): none
// (default) stores no body, errors stores them only on rows with an error,
// full on every row. config's V43 validates with BodyLoggingValid.
const (
	BodyLoggingNone   = "none"
	BodyLoggingErrors = "errors"
	BodyLoggingFull   = "full"
)

// GatewayMetadataVersion is metadata.gw.v.
const GatewayMetadataVersion = 1

// Bounds of the /v1 row. Hardcoded on purpose (design/06 §4.6 names them):
// they bound the row the process writes about a request, not anybody's
// policy.
const (
	// gatewayBodyCap caps each stored body under errors/full, the
	// truncation marker included.
	gatewayBodyCap = 64 << 10
	// gatewayAliasMax is the alias name limit of the alias registry
	// (^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$); the rollup keeps 100 of it.
	gatewayAliasMax = 128
	// gatewayHopMax is the deepest federation hop (design/04 §4.6); the
	// rollup clamps to the same range.
	gatewayHopMax = 16
	// gatewayClaimedChainMax bounds the claimed instance chain of a hop,
	// display only — never an index, rollup or event key.
	gatewayClaimedChainMax = 16
	// gatewayTokenMax bounds client_req_id and peer_pipeline.
	gatewayTokenMax = 128
	// redactedTokenMax bounds the class and code of a RedactedErr.
	redactedTokenMax = 64
)

// gatewayToken is the grammar of client_req_id and peer_pipeline:
// [A-Za-z0-9._:-]{1,128}. Anything else is dropped, never cut — a cut
// request id names another request.
var gatewayToken = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// redactedVocab is the grammar of a RedactedErr class or code: the lower
// snake case of the oai code vocabulary and the wire error classes.
var redactedVocab = regexp.MustCompile(`^[a-z0-9_]+$`)

var gatewayPipelineOps = map[string]string{
	PipelineGatewayChat:       GatewayOpChat,
	PipelineGatewayEmbeddings: GatewayOpEmbed,
	PipelineGatewayRerank:     GatewayOpRerank,
}

// GatewayOp maps a gateway pipeline to its op; "" for any other pipeline.
func GatewayOp(pipeline string) string { return gatewayPipelineOps[pipeline] }

// BodyLoggingValid reports whether s is exactly one of the policies.
func BodyLoggingValid(s string) bool {
	switch s {
	case BodyLoggingNone, BodyLoggingErrors, BodyLoggingFull:
		return true
	}
	return false
}

var (
	gatewayOps      = map[string]bool{GatewayOpChat: true, GatewayOpEmbed: true, GatewayOpRerank: true}
	gatewayFinishes = map[string]bool{
		FinishStop: true, FinishLength: true, FinishToolCalls: true,
		FinishContentFilter: true, FinishError: true, FinishAborted: true,
	}
	gatewayUsages = map[string]bool{UsageBackend: true, UsageEstimated: true}
)

// Gateway is what a /v1 row says about the request beyond the columns —
// metadata.gw, version 1. Every value is bounded by StampGateway before it
// is stored: numbers are clamped like the rollup clamps them, a malformed
// token or id is dropped. There is no origin (K26: a claimed origin is
// forgeable and never a key) and no consumer — the consumer is the
// consumer_id column (§8 d02 (6)).
type Gateway struct {
	// Op is chat, embed or rerank; GatewayEntry derives it from the
	// pipeline.
	Op string
	// Alias is the resolved model alias the client named (or the
	// unresolved marker of the alias registry), at most 128 runes.
	Alias string
	// Stream marks a streamed answer.
	Stream bool
	// Hop is the federation hop of the request, clamped to 0..16.
	Hop int
	// ClaimedChain is the instance chain a peer claims (UUIDs, at most
	// 16, display only).
	ClaimedChain []string
	// PeerPipeline is the pipeline a ctx peer names for its call — set it
	// only for a ctx-peer consumer.
	PeerPipeline string
	// Usage is UsageBackend or UsageEstimated.
	Usage string
	// TTFTMs is the time to the first token of a stream in ms; nil =
	// unmeasured, ignored on a non-stream row. 0 is a measurement.
	TTFTMs *int64
	// ClientReqID is the client's request id ([A-Za-z0-9._:-]{1,128},
	// else dropped).
	ClientReqID string
	// Finish is one of the Finish* values.
	Finish string
	// ModelKeyID is the model key the request authenticated with —
	// forensics ("which requests ran over the leaked key?") through the
	// consumer index plus this field (§8 d01 (3)); never a column.
	ModelKeyID string
}

// gatewayWire is metadata.gw as stored: the keys the rollup trigger reads
// (alias, hop, usage, ttft_ms — migration 159) and the rest of v1.
type gatewayWire struct {
	V            int      `json:"v"`
	Op           string   `json:"op,omitempty"`
	Alias        string   `json:"alias,omitempty"`
	Stream       bool     `json:"stream"`
	Hop          int      `json:"hop"`
	ClaimedChain []string `json:"claimed_chain,omitempty"`
	PeerPipeline string   `json:"peer_pipeline,omitempty"`
	Usage        string   `json:"usage,omitempty"`
	TTFTMs       *int64   `json:"ttft_ms,omitempty"`
	ClientReqID  string   `json:"client_req_id,omitempty"`
	Finish       string   `json:"finish,omitempty"`
	ModelKeyID   string   `json:"model_key_id,omitempty"`
}

// StampGateway records WHO made a /v1 request and how it ran: the
// consumer_id column and metadata.gw. It is the ONLY setter of both
// (census TestGatewayConsumerIDCensus), the pattern of StampPrompt. A row
// names one caller: a knowledge api_key_id left on the entry is cleared and
// logged at ERROR (design/01 B17) — kept, it would get the row refused by
// ck_llm_log_one_identity, and the rollup, the tenant spend and the tenant
// llmlog filter read api_key_id as a knowledge key. A metadata.consumer_id
// (the transitional spelling of design/02 rev. 0) is removed: the column is
// the attribution.
func (e *Entry) StampGateway(consumerID string, g Gateway) {
	if e.APIKeyID != "" {
		slog.Error("llmlog: a gateway row carried a knowledge api_key_id — cleared, the row is attributed to the consumer only (B17)",
			"pipeline", e.Pipeline)
		e.APIKeyID = ""
	}
	e.ConsumerID = consumerID
	md := maps.Clone(e.Metadata)
	if md == nil {
		md = map[string]any{}
	}
	delete(md, "consumer_id")
	md["gw"] = g.wire()
	e.Metadata = md
}

func (g Gateway) wire() gatewayWire {
	w := gatewayWire{
		V:      GatewayMetadataVersion,
		Alias:  cleanText(g.Alias, gatewayAliasMax),
		Stream: g.Stream,
		Hop:    min(max(g.Hop, 0), gatewayHopMax),
	}
	if gatewayOps[g.Op] {
		w.Op = g.Op
	}
	for _, id := range g.ClaimedChain {
		if len(w.ClaimedChain) == gatewayClaimedChainMax {
			break
		}
		if u, err := uuid.Parse(id); err == nil {
			w.ClaimedChain = append(w.ClaimedChain, u.String())
		}
	}
	if validToken(g.PeerPipeline) {
		w.PeerPipeline = g.PeerPipeline
	}
	if gatewayUsages[g.Usage] {
		w.Usage = g.Usage
	}
	if g.Stream && g.TTFTMs != nil && *g.TTFTMs >= 0 {
		v := *g.TTFTMs
		w.TTFTMs = &v
	}
	if validToken(g.ClientReqID) {
		w.ClientReqID = g.ClientReqID
	}
	if gatewayFinishes[g.Finish] {
		w.Finish = g.Finish
	}
	if u, err := uuid.Parse(g.ModelKeyID); err == nil {
		w.ModelKeyID = u.String()
	}
	return w
}

func validToken(s string) bool {
	return len(s) <= gatewayTokenMax && gatewayToken.MatchString(s)
}

// storable makes s storable in a JSONB string or a text column: invalid
// UTF-8 becomes U+FFFD and NUL is removed — PostgreSQL refuses both, and a
// refused insert costs the whole row, attribution included.
func storable(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

// cleanText is storable, cut to maxRunes runes.
func cleanText(s string, maxRunes int) string {
	s = storable(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes])
}

// RedactedErr is the only error a /v1 row stores (design/02 §3.4, design/06
// §4.6): class, status and ctx code — never the upstream body, which can
// carry prompt fragments (httpx.StatusError.Error() includes it) and which
// tenant-admins would read in the error column of /api/llmlog. Class and
// Code must have the shape of vocabulary words ([a-z0-9_]{1,64}); anything
// else prints as the redaction marker, so free text cannot reach the row.
// The shape is all this package can check: that Code is a code of the
// gateway vocabulary (oai.Codes) and Class one of the wire classes is the
// writer's duty — llmlog cannot import oai, whose handlers import llmlog —
// and neither may ever be copied from an upstream answer (its error.code or
// type is body content).
type RedactedErr struct {
	Class  string
	Status int
	Code   string
}

// Error renders "<class> <status> <code>", leaving out what is empty;
// "error" when nothing is set.
func (e *RedactedErr) Error() string {
	if e == nil {
		return "error"
	}
	var parts []string
	if e.Class != "" {
		parts = append(parts, redactedToken(e.Class))
	}
	if e.Status != 0 {
		parts = append(parts, strconv.Itoa(e.Status))
	}
	if e.Code != "" {
		parts = append(parts, redactedToken(e.Code))
	}
	if len(parts) == 0 {
		return "error"
	}
	return strings.Join(parts, " ")
}

func redactedToken(s string) string {
	if len(s) > redactedTokenMax || !redactedVocab.MatchString(s) {
		return redact.Redacted
	}
	return s
}

// GatewayRow is everything a /v1 handler hands over for its one row.
type GatewayRow struct {
	// Pipeline is one of the gateway pipelines.
	Pipeline string
	// ConsumerID is the authenticated consumer (auth.ConsumerAuth).
	ConsumerID string
	// Gateway becomes metadata.gw; Op is derived from Pipeline.
	Gateway Gateway
	// Telemetry is the row as the walk measured it — model, host,
	// duration, tokens, the serving backend, sensitivity, cost and the
	// dispatch columns, plus free metadata keys. GatewayEntry overwrites
	// its Pipeline, ConsumerID (and clears APIKeyID), Err, the three body
	// fields, Attempt, metadata.gw and metadata.chain; every other field
	// is kept as it is.
	Telemetry Entry
	// Attempts is every physical attempt of the request, in order — the
	// row's attempt count and metadata.chain. A failover is one row.
	Attempts []WireAttempt
	// Err is the row's error; nil = success. Only a RedactedErr is
	// accepted, the walk's raw error never reaches the row.
	Err *RedactedErr
	// Request and Response are the serialized client input (messages or
	// input) and the answer the client received. Stored only as the body
	// policy says, capped at 64 KiB each.
	Request, Response string
	// BodyLogging is gateway.body_logging of the request's settings
	// snapshot; anything but errors or full stores no body, and neither
	// does a row whose Telemetry.RequiredSensitivity is credentials, empty
	// or unknown.
	BodyLogging string
}

// GatewayEntry builds the ONE row of a /v1 request: pipeline, StampGateway,
// the body policy, the redacted error and the folded attempts, and then
// Slimmed(false) — always false, never the tenant's devmode: tenant.devmode
// is tenant-overridable, and a tenant must not open the seal of a
// credentials-class /v1 row for itself (design/06 §4.6, design/04 R-06-1).
// A row without a consumer or with a pipeline outside the vocabulary is a
// wiring defect: logged at ERROR and still built, so the defect is visible
// in the log and the row is not lost.
func GatewayEntry(r GatewayRow) Entry {
	e := r.Telemetry
	e.Pipeline = r.Pipeline
	g := r.Gateway
	g.Op = GatewayOp(r.Pipeline)
	if g.Op == "" {
		slog.Error("llmlog: a gateway row names a pipeline outside the gateway vocabulary", "pipeline", r.Pipeline)
	}
	if r.ConsumerID == "" {
		slog.Error("llmlog: a gateway row without a consumer — the request is unattributed", "pipeline", r.Pipeline)
	}
	e.StampGateway(r.ConsumerID, g)

	e.Err = nil
	if r.Err != nil {
		e.Err = r.Err
	}

	e.Attempt = len(r.Attempts)
	if len(r.Attempts) > 0 {
		e.Metadata["chain"] = r.Attempts
	} else {
		delete(e.Metadata, "chain")
	}

	e.RequestSystem, e.RequestUser, e.ResponseContent = "", "", ""
	store := false
	switch strings.ToLower(strings.TrimSpace(r.BodyLogging)) {
	case BodyLoggingFull:
		store = true
	case BodyLoggingErrors:
		store = r.Err != nil
	}
	if store && bodiesAllowed(e.RequiredSensitivity, e.Pipeline) {
		e.RequestUser, e.ResponseContent = capBody(r.Request), capBody(r.Response)
	}
	return e.Slimmed(false)
}

// lastSensWarn throttles the ERROR of bodiesAllowed like the loss WARN
// (lossWarnInterval): a writer that forgot the sensitivity forgets it on
// every row.
var lastSensWarn atomic.Int64

// bodiesAllowed is the fail-closed half of the seal that Slimmed cannot
// give: Slimmed drops the bodies of exactly "credentials", while an empty
// or unknown sensitivity is NORMATIVELY credentials (backends.Sensitivity
// rank, fail-closed). A /v1 row stores bodies only for one of the defined
// classes below credentials; for an empty or unknown one it stores none and
// logs the wiring defect at ERROR (throttled).
func bodiesAllowed(sensitivity, pipeline string) bool {
	s := backends.Sensitivity(sensitivity)
	if backends.ValidSensitivity(s) {
		return s.Rank() < backends.SensCredentials.Rank()
	}
	now := time.Now().UnixNano()
	if last := lastSensWarn.Load(); (last == 0 || now-last >= int64(lossWarnInterval)) && lastSensWarn.CompareAndSwap(last, now) {
		slog.Error("llmlog: a gateway row without a defined required_sensitivity — bodies not stored (fail-closed as credentials)",
			"pipeline", pipeline, "required_sensitivity", cleanText(sensitivity, gatewayTokenMax))
	}
	return false
}

// RecordGateway writes the row of a /v1 request — the only write path of
// the gateway (census TestGatewayOAICensus: no llmlog.Record under
// internal/oai). Asynchronous like Record.
func RecordGateway(pool *pgxpool.Pool, r GatewayRow) {
	Record(pool, GatewayEntry(r))
}

// capBody cleans a body for the text column and cuts it to gatewayBodyCap
// bytes on a rune boundary, the visible truncation marker included.
func capBody(s string) string {
	s = storable(s)
	if len(s) <= gatewayBodyCap {
		return s
	}
	cut := gatewayBodyCap - len(redact.Truncated)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + redact.Truncated
}
