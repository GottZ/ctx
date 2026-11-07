package oai

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
)

// POST /v1/embeddings (wave W02-09; design/02 §4.8, §4.2.1, §4.11.1, §4.12;
// design/06 §4.1). The order of one request — the stage order of the chat
// route (lead decision M1 (a): built here from the free functions, chat.go
// untouched; one extraction wave joins the two after W02-10):
//
//	invariants (admit, inference dependencies) → write deadline at entry →
//	request deadline on the request context → body → parse (model, input,
//	encoding_format, dimensions) → Resolve → kind embedding → policy
//	(inputs, dimensions) → answer reserve → RequiredSensitivity + scan →
//	chain build (gateway wire embedding: openai, ollama) → GateTenant →
//	input runes → limiter (op embed) → planner (Units n, SubBatch) → k = 1
//	rule → watcher → WalkChain(attempt = the sub-blocks of subblock.go:
//	block one under First/Next, blocks 2…k under Continue on the request
//	context, sticky) → check before the write → streamed answer → one llmlog
//	row, one access record.
//
// Nothing is written before the last block returned: every failure is a
// real status (design/02 §4.8 "Antwort"). Every refusal names its code as
// the access record's reason.
//
// The route holds on the heap only what its in-flight reservation covers
// (FOLGE-E, the ledger of FOLGE-P F-0 at this route, requestLedger): every
// term is booked before its allocation, in the order the request allocates
// it — the body read into one buffer of its declared length
// (readRequestBody), the nesting state of the JSON check (jsonDepthStateBytes
// per level), the decoded inputs (their exact decoded length and
// embedInputSlotBytes each, sized before the decode — readInput), the
// sensitivity scan below credentials (scanBooking: scanReserveFactor × the
// body + scanReserveBase + scanDepthStateBytes per level), the upstream body
// of every block of an attempt before its first wire call
// (upstreamBooking: each body's exact length, embed.WireBodyLen — internal/embed
// writes it in one allocation of that length since FOLGE-E hardening 1 —
// with the allocation's rounding, the transport's copy buffer and the wire
// call, and the copy and the fresh connection of the call's retry
// (httpx.DoRetryOnce), booked with the first call; its texts' part before
// the acquire, reserveUpstreamTexts) and the log
// row's text of the inputs (bodyLogSize). The request share the budget stage
// reserved (requestReserveFactor × the body) is spent first, the
// reservation grows past it, never shrinks. A booking the budget cannot hold
// is 503 server_overloaded before the allocation — nothing booked, nothing
// allocated, no wire; the log row goes without its text. The model name past
// modelRawMax is never decoded (probeNoAlias, the answer of an unknown
// model). Every answer, upstream body and log row is the one the route gave
// before, except where a booking passes the budget.

// embedPipeline is the llmlog pipeline of the route's row (a local name for
// the llmlog constant, the shape the promptguard census resolves).
const embedPipeline = llmlog.PipelineGatewayEmbeddings

// EmbedEncodeBuffer is the encoder buffer of the streamed answer (design/02
// §4.2.1: "64 KiB = Encoder-Puffer"): the items are encoded into it and it is
// flushed after every item, so the answer never exists as one JSON in
// memory. Its size is part of the answer reserve. A derived mechanism
// constant of the handler's memory shape, not a policy (the policy values
// are gateway.inflight_budget and gateway.embedding_max_floats). Exported for
// the pin against the boot validation's copy (config, V-rule "inflight_budget
// holds a request at every ceiling").
const EmbedEncodeBuffer = 64 << 10

// EmbedFloatBytes is the size of one float32 component held for the answer
// (exported for the same pin).
const EmbedFloatBytes = 4

// The byte bound of one block's upstream answer (W02-09 hardening H-3,
// Fremd-Review 2 R2-9): EmbedRaw reads at most embedReadCap bytes, and the
// route reserves embedDecodeFactor times that before the block's wire call —
// the decode buffer is part of the in-flight budget, not beside it. Derived
// mechanism constants of the wire format, not policy (the policy values are
// embedding.sub_batch, embedding.native_dims, gateway.embedding_max_floats
// and gateway.inflight_budget).
const (
	// embedWireComponentBytes: the longest float64 literal in shortest
	// round-trip form (-2.2250738585072014e-308, 24 bytes) and its separator
	// — a backend that prints float64 (llama.cpp, vLLM, Ollama) stays
	// inside; one that pads every number with whitespace does not.
	embedWireComponentBytes = 25
	// embedWireItemBytes: the frame of one OpenAI data entry
	// ({"object":"embedding","index":N,"embedding":[…]}, ≈ 50 bytes) with
	// room for fields a backend adds.
	embedWireItemBytes = 256
	// embedWireAnswerBytes: the frame of the answer (the list, the model,
	// usage and fields a backend adds).
	embedWireAnswerBytes = 4 << 10
	// embedDecodeFactor: the strict decode (internal/embed rawAnswerBuffer)
	// holds the answer in one buffer that grows by doubling up to the read
	// cap — less than twice the cap in all — and strconv clones a literal it
	// refuses (at most the cap): less than three times the read cap.
	embedDecodeFactor = 3
)

// embedReadCap is the byte bound of the answer to a block of inputs at dims
// components each; dims ≤ 0 (unknown) takes the floats ceiling
// gateway.embedding_max_floats. Never above httpx.MaxResponseBytes, the
// transport cap of every embed answer.
func embedReadCap(inputs, dims int, maxFloats int64) int64 {
	floats := int64(inputs) * int64(dims)
	if dims <= 0 {
		floats = maxFloats
	}
	return min(floats*embedWireComponentBytes+int64(inputs)*embedWireItemBytes+embedWireAnswerBytes, httpx.MaxResponseBytes)
}

// The texts of the route's own refusals.
const embedKindMessage = "The model does not support embeddings."

// The route's own ends of a run (causes on the request context, like the
// watcher's and the shutdown's): verdict answers them.
var (
	// errAnswerOverloaded: the native dimension learned from the first
	// block makes the answer — or the decode share of the later blocks —
	// larger than reserved, and the in-flight budget does not hold the rest
	// ⇒ 503 server_overloaded after GPU work (only with an unknown native
	// dimension: past gateway.embedding_max_floats, resp. a later block's
	// read cap past the floats ceiling's).
	errAnswerOverloaded = fmt.Errorf("oai: the answer does not fit the in-flight budget: %w", context.Canceled)
	// errRouteRefused: the route ended the run with its own answer after an
	// acquire (c.own) — the k = 1 rule against a weight plan of more than
	// one block, a planner defect.
	errRouteRefused = fmt.Errorf("oai: the route refused the request after the acquire: %w", context.Canceled)
	// errBlockDeadline: the request deadline, not the row, ended the wire
	// call of a block ≥ 2 — the sequence of blocks and their continuation
	// waits spent the frame, the last block got its rest. A cancellation for
	// the walker: no health report against a healthy row (Fremd-Review 1
	// F-3); the client's answer stays the sticky stop (its class is the open
	// user decision N-1).
	errBlockDeadline = fmt.Errorf("oai: the request deadline ended a later block: %w", context.Canceled)
)

// Embeddings is the handler of POST /v1/embeddings.
func Embeddings(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := time.Now()
		ca, ok := d.admit(w, r)
		if !ok {
			return
		}
		if m := d.inferenceMissing(); m != "" {
			slog.Error("gateway: inference dependency missing (wiring defect)", "missing", m,
				"path", r.URL.Path, "request_id", w.Header().Get("X-Request-ID"))
			refuseInternal(w, r)
			return
		}
		c := newEmbedCall(d, w, r, ca, entry)
		defer c.close()
		c.serve()
	})
}

// embedCall is one request's state.
type embedCall struct {
	d      Deps
	w      http.ResponseWriter
	r      *http.Request // carries ctx
	client context.Context
	ctx    context.Context
	cancel context.CancelCauseFunc
	stops  []func() bool
	s      Settings
	ca     *auth.ConsumerAuth
	rec    *AccessRecord
	entry  time.Time
	dl     time.Time
	id     string

	alias    modelalias.Alias
	gen      uint64 // the alias registry generation read right before the entry Resolve (B-3)
	req      *embedRequest
	lim      embedLimits
	required backends.Sensitivity
	reserved int64 // the answer's share of the in-flight budget
	decoding int64 // the decode transient's share (one block at a time)
	limit    LimitReservation
	limitEnd bool
	// held is what the reservation holds — the tokens Keep books, read from
	// the admission (LimitReservation.Held), the ceiling of a block
	// estimate's charge (meterEnd).
	held int
	// wired: a byte of the request can have reached a backend in some try
	// of some block (wireContactTrace) — set from inside net/http, on its
	// transport goroutines. Without it the reservation goes back in full
	// (Abort); after it, never (Settle with the usage, Keep without one;
	// the chat route's rule, W06-10 hardening H-7).
	wired atomic.Bool
	// own is the answer of a route refusal after an acquire (errRouteRefused).
	own func() string
	// ledger is the request share of the reservation and what the request
	// allocated against it (FOLGE-E).
	ledger requestLedger
	// textsBooked is the texts' part of the first attempt's upstream bodies,
	// booked before the acquire (reserveUpstreamTexts); the first attempt
	// books only the rest of its bodies (frames, slack, wire calls).
	textsBooked int64
}

func newEmbedCall(d Deps, w http.ResponseWriter, r *http.Request, ca *auth.ConsumerAuth, entry time.Time) *embedCall {
	s := d.Settings()
	c := &embedCall{d: d, w: w, s: s, ca: ca, entry: entry, client: r.Context(), rec: AccessRecordFrom(r.Context())}
	if c.rec == nil {
		c.rec = &AccessRecord{} // outside a model group: filled, never read
	}
	c.rec.Op, c.rec.Stream = llmlog.GatewayOpEmbed, false
	c.rec.Hop = len(fedhop.InboundFrom(r.Context()).Hop)
	// The write deadline at entry (§4.12, B23) and the request deadline on
	// the request context before the planner (A03-W8 reads ServeBy from it):
	// all k blocks run inside it (design/02 §4.8 "Frist").
	setWriteDeadline(w, entry.Add(d.MaxWait()+s.RequestDeadline+writeMargin))
	c.dl = entry.Add(s.RequestDeadline)
	ctx, cancelDl := context.WithDeadline(r.Context(), c.dl)
	ctx, cancel := context.WithCancelCause(ctx)
	c.ctx, c.cancel = ctx, cancel
	c.stops = append(c.stops, func() bool { cancelDl(); return true })
	if d.Lifecycle != nil {
		c.stops = append(c.stops, context.AfterFunc(d.Lifecycle, func() { cancel(errShutdown) }))
		// Synchronously as well: AfterFunc runs in its own goroutine even
		// for a lifecycle that already ended (W02-07 H-1).
		if d.Lifecycle.Err() != nil {
			cancel(errShutdown)
		}
	}
	c.r = r.WithContext(ctx)
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	c.id = id.String()
	return c
}

// close ends the request's contexts and a reservation nobody ended — after
// wire contact (a panic inside EmbedRaw once a block was written, or in the
// answer path) by keeping its charge, the usage is unknown; without it by
// Abort.
func (c *embedCall) close() {
	if c.limit != nil && !c.limitEnd {
		if c.wired.Load() {
			c.limit.Keep()
		} else {
			c.limit.Abort()
		}
	}
	c.cancel(context.Canceled)
	for _, stop := range c.stops {
		stop()
	}
}

func (c *embedCall) refuse(code, message string, shouldRetry bool) {
	c.rec.Reason = code
	WriteCode(c.w, code, message, shouldRetry)
}

func (c *embedCall) internal(msg string, args ...any) {
	slog.Error("gateway: "+msg, append(args, "path", c.r.URL.Path, "request_id", c.w.Header().Get("X-Request-ID"))...)
	c.rec.Reason = CodeInternalError
	WriteInternal(c.w)
}

// refuseParam answers a parse refusal with its param, bound to the
// vocabulary like WriteCode.
func (c *embedCall) refuseParam(perr *paramError) {
	if perr == errParseOverloaded {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return
	}
	i := slices.IndexFunc(codes, func(s CodeSpec) bool { return s.Code == perr.code })
	if i < 0 {
		c.internal("parse error code outside the vocabulary", "code", perr.code)
		return
	}
	c.rec.Reason = perr.code
	spec := codes[i]
	Write(c.w, Error{Status: spec.Status, Type: spec.Type, Code: spec.Code, Param: perr.param, Message: perr.message})
}

func (c *embedCall) serve() {
	// A run that ended before the request did anything is answered at once
	// (W02-07 H-1): no parse, no grant read, no reservation.
	if ended, _ := c.verdict(); ended {
		return
	}
	body, ok := c.readBody()
	if !ok {
		return
	}
	depth := jsonDepth(body)
	if !c.ledger.cover(depth * jsonDepthStateBytes) { // the JSON check's nesting state, booked before it runs
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return
	}
	req, perr := parseEmbedTop(body, c.ledger.cover)
	if perr != nil {
		c.refuseParam(perr)
		return
	}
	if !c.resolve(req.model, req.modelOverlong) {
		return
	}
	c.lim = embedLimitsOf(c.alias, c.s)
	if perr := checkEmbedPolicy(req, c.lim); perr != nil {
		c.refuseParam(perr)
		return
	}
	// The rune limit that holds without a chain — the alias's
	// max_input_runes, else the code ceiling — before the reserve, the scan
	// and the chain build (hardening H-7): an input over it is the client's
	// 400, never a 503 of the budget. The chain's window narrows it below.
	if perr := checkInputRunes(req, embedEarlyRuneLimit(c.lim)); perr != nil {
		c.refuseParam(perr)
		return
	}
	c.req = req
	if len(req.ignored) > 0 {
		c.w.Header().Set("X-Ctx-Ignored-Params", strings.Join(req.ignored, ", "))
	}
	if !c.reserveAnswer() {
		return
	}
	required := c.sensitivity()
	if !c.coverScan(required, body, depth) {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return
	}
	c.required = c.d.Federation.Scan(required, body)
	chain, ok := c.chain()
	if !ok {
		return
	}
	if perr := checkInputRunes(req, embedRuneLimit(c.lim, chain)); perr != nil {
		c.refuseParam(perr)
		return
	}
	if !c.reserveUpstreamTexts() {
		return
	}
	if !c.admitLimiter() {
		return
	}
	c.walk(chain)
}

// reserveUpstreamTexts books the texts' part of the upstream bodies before
// the acquire (FOLGE-E, as reserveAnswer the certain decode share): every
// attempt sends every text once, so the encoded texts are certain at the
// entry, and a request whose upstream bodies the budget cannot hold is
// refused there — no queue wait, no queue place, no unit. Only the rest of
// the blocks' bodies (their number, the row's model and its wire are known
// at the attempt) follows at the first attempt.
func (c *embedCall) reserveUpstreamTexts() bool {
	c.textsBooked = c.upstreamTexts()
	if !c.ledger.cover(c.textsBooked) {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return false
	}
	return true
}

// coverScan books the heap of the sensitivity scan of the body before it
// runs, as the chat route does (FOLGE-P hardening H-8 and hardening 7,
// chatCall.coverScan; W02-09 rebase round 2, part D — measured: without it
// the route's heap passed its bookings, bau report): scanBooking —
// scanReserveFactor × the body + scanReserveBase + scanDepthStateBytes for
// each level of the body's nesting (depth, jsonDepth; FOLGE-E: the nesting
// state was not booked here, as it was not at the chat route before
// hardening 7) — through the ledger for an alias below credentials,
// nothing at credentials (federation.ScanRaise returns at once there). It
// lasts until the request ends: the scan's garbage is heap until the
// collector runs, and the promise "the heap stays within the bookings"
// holds under a set GOMEMLIMIT (scanReserveFactor). false: the budget
// cannot hold it, nothing is booked.
func (c *embedCall) coverScan(required backends.Sensitivity, body []byte, depth int64) bool {
	if required.Rank() >= backends.SensCredentials.Rank() {
		return true
	}
	return c.ledger.cover(scanBooking(body, depth))
}

// readBody reads the body under the strict cap and the in-flight budget: a
// chunked body past the budget is the budget's 503, never a 400 (§4.2.1).
// The ledger starts here (FOLGE-E, as chatCall.readBody): the stage reserved
// requestReserveFactor × the body, the read spent what readRequestBody
// allocated — one buffer of the declared length, where io.ReadAll grew its
// way there and spent about twice the body.
func (c *embedCall) readBody() ([]byte, bool) {
	body, spent, err := readRequestBody(c.r.Body, c.r.ContentLength)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		reserved := int64(len(body))
		if cl := c.r.ContentLength; cl > 0 {
			reserved = cl
		}
		c.ledger = newRequestLedger(ReservationFrom(c.ctx), reserved)
		if !c.ledger.cover(spent) { // a small undeclared body's first chunk can pass its share
			c.rec.Reason = CodeServerOverloaded
			WriteOverloaded(c.w)
			return nil, false
		}
		return body, true
	case errors.Is(err, ErrBudgetExhausted):
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
	case errors.As(err, &tooLarge):
		c.rec.Reason = CodeRequestTooLarge
		WriteRequestTooLarge(c.w, tooLarge.Limit)
	case c.client.Err() != nil:
		// the client went away while sending
	default:
		c.refuse(CodeInvalidJSON, "The request body could not be read.", false)
	}
	return nil, false
}

// resolve is §4.4 steps 1–6 with the kind embedding. A model name past
// modelRawMax (overlong, not decoded) takes the grant probe of a name that
// is no alias and gets the 404 (FOLGE-E, as chatCall.resolve).
func (c *embedCall) resolve(name string, overlong bool) bool {
	c.gen = c.d.Aliases.Generation() // before the Resolve (W02-07 B-3)
	var a modelalias.Alias
	var found bool
	var err error
	if overlong {
		err = c.d.probeNoAlias(c.ctx, c.ca)
	} else {
		a, found, err = c.d.Resolve(c.ctx, c.ca, name)
	}
	switch {
	case err != nil:
		if ended, _ := c.verdict(); ended {
			return false
		}
		grantReadFailed(c.w, c.r, err)
		return false
	case !found:
		refuseModelNotFound(c.w, c.r)
		return false
	case a.Kind != modelalias.KindEmbedding:
		c.refuse(CodeModelNotSupported, embedKindMessage, false)
		return false
	}
	c.alias = a
	c.rec.Alias = a.Name
	return true
}

// reserveAnswer takes the answer's share of the in-flight budget before any
// acquire (design/02 §4.2.1): n × native_dims × 4 B + the encoder buffer —
// the vectors are held as float32 until the streamed answer. An unknown
// native dimension reserves the ceiling gateway.embedding_max_floats × 4 B
// (lead decision O-09-3 (a)): the refusal stays immediate, no GPU work is
// lost to a late refusal.
func (c *embedCall) reserveAnswer() bool {
	res := ReservationFrom(c.ctx)
	if res == nil {
		c.internal("inference handler outside the in-flight budget stage (wiring defect)")
		return false
	}
	floats := int64(len(c.req.inputs)) * int64(c.lim.nativeDims)
	if c.lim.nativeDims <= 0 {
		if c.s.EmbeddingMaxFloats <= 0 {
			c.internal("gateway.embedding_max_floats is not positive (settings defect)", "embedding_max_floats", c.s.EmbeddingMaxFloats)
			return false
		}
		floats = c.s.EmbeddingMaxFloats
	}
	need := floats*EmbedFloatBytes + EmbedEncodeBuffer
	if !res.Grow(need) {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return false
	}
	c.reserved = need
	// The decode share that is certain before the acquire, with the answer's
	// (hardening 2, Fremd-Review 3 F3-2): a refusal that stands at the entry
	// costs no queue wait, no queue place and no unit. The rest of a larger
	// block follows at the attempt (decodeFloor).
	if !c.reserveDecode(c.decodeFloor(), c.lim.nativeDims) {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return false
	}
	return true
}

// decodeFloor is the largest block whose decode share is certain before
// the acquire. The weight plan's largest block is min(n, sub_batch, E)
// (handler.ExternalWeightPlan), and the row's capacity E is known at the
// acquire only. Without a native dimension every block is read up to the
// floats ceiling — only the 256-B frame of an entry grows with the block —
// so the plan's bound min(n, sub_batch) is booked: at most 3 × 256 B per
// input above an E-limited block, nothing where the 16-MiB clamp binds
// (the default ceiling). With one the cap grows with the block, and only a
// one-input block is certain (E ≥ 1): booking min(n, sub_batch) there would
// over-reserve by the factor sub_batch / E. The rest of a larger block is
// booked after the queue wait, at the attempt — a named limit (docs/api.md).
func (c *embedCall) decodeFloor() int {
	if c.lim.nativeDims > 0 {
		return 1
	}
	n := len(c.req.inputs)
	if c.lim.subBatch > 0 {
		n = min(n, c.lim.subBatch)
	}
	return n
}

// learnReserve grows the reserve to the answer of a native dimension the
// first block's answer named. A reserve already larger stays as it is —
// the reservation cannot shrink without a contract change (O-09-3, a
// handover).
func (c *embedCall) learnReserve(dims int) bool {
	need := int64(len(c.req.inputs))*int64(dims)*EmbedFloatBytes + EmbedEncodeBuffer
	if need <= c.reserved {
		return true
	}
	res := ReservationFrom(c.ctx)
	if res == nil || !res.Grow(need-c.reserved) {
		return false
	}
	c.reserved = need
	return true
}

// reserveDecode grows the decode transient to the bound of a block of
// inputs at dims components (embedDecodeFactor × embedReadCap; dims ≤ 0:
// the floats ceiling). It only grows — the reservation cannot shrink.
func (c *embedCall) reserveDecode(inputs, dims int) bool {
	need := embedDecodeFactor * embedReadCap(inputs, dims, c.s.EmbeddingMaxFloats)
	if need <= c.decoding {
		return true
	}
	res := ReservationFrom(c.ctx)
	if res == nil || !res.Grow(need-c.decoding) {
		return false
	}
	c.decoding = need
	return true
}

// sensitivity is RequiredSensitivity over the request's terms (K30).
func (c *embedCall) sensitivity() backends.Sensitivity {
	t := SensitivityTerms{Floor: c.d.Federation.Floor(c.ctx), Alias: c.alias, Overlay: c.d.TenantSensitivity(c.ctx)}
	if g, ok := c.d.Aliases.Snapshot().Shadowed(c.alias.Name); ok {
		t.Shadowed = &g
	}
	return RequiredSensitivity(t)
}

// chain builds and gates the chain (§4.4.3 steps 1–5) for the kind
// embedding.
func (c *embedCall) chain() ([]backends.Backend, bool) {
	chain, err := BuildChain(ChainSpec{
		Alias: c.alias, TenantScope: c.ca.TenantScope, Required: c.required, ExportableRoles: c.s.ExportableRoles,
		Chain: c.d.Pool.Chain,
		Federation: func(chain []backends.Backend, required backends.Sensitivity) ([]backends.Backend, error) {
			return c.d.Federation.Filter(c.ctx, chain, required)
		},
	})
	var ne *backends.ErrNoEligibleBackend
	switch {
	case errors.As(err, &ne):
		slog.Warn("gateway: no eligible backend", "alias", c.alias.Name, "role", ne.Role, "required", string(ne.Required),
			"excluded", fmt.Sprintf("%+v", ne.Excluded), "request_id", c.w.Header().Get("X-Request-ID"))
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return nil, false
	case err != nil:
		c.internal("chain build failed", "error", err)
		return nil, false
	}
	gated, err := c.d.Quota.GateTenant(c.ca.TenantID, chain)
	var qe *backends.ErrQuotaExceeded
	switch {
	case errors.As(err, &qe):
		writeRetryAfter(c.w.Header(), untilNextHour(time.Now()))
		c.refuse(CodeInsufficientQuota, quotaMessage, false)
		return nil, false
	case err != nil:
		c.internal("tenant quota gate refused the request (wiring defect)", "error", err)
		return nil, false
	case len(gated) == 0:
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return nil, false
	}
	return gated, true
}

// admitLimiter is design/06 §4.1 steps 7–9 with op embed (lead decision M4
// (a)): no limiter ⇒ 503 service_unavailable with the largest Retry-After
// step (L11) — never served without it.
func (c *embedCall) admitLimiter() bool {
	if c.d.Limiter == nil {
		if steps := c.d.RetryAfterSteps(); len(steps) > 0 {
			writeRetryAfter(c.w.Header(), steps[len(steps)-1])
		}
		c.refuse(CodeServiceUnavailable, limiterMissingText, true)
		return false
	}
	res, refusal := c.d.Limiter.Admit(c.ctx, c.ca, c.alias,
		Estimate{Op: llmlog.GatewayOpEmbed, PromptBytes: c.req.bytesSum, InputRunes: c.req.runesSum})
	if refusal != nil {
		// The meter's Retry-After is exact (design/06 §4.4): both headers,
		// no jitter — the jitter is the L2 contract (A03-W8).
		writeRetryAfter(c.w.Header(), refusal.RetryAfter)
		writeRetryAfterMs(c.w.Header(), refusal.RetryAfter)
		c.refuse(refusal.Code, refusal.Message, refusal.ShouldRetry)
		return false
	}
	if res == nil {
		c.internal("limiter admitted without a reservation (wiring defect)")
		return false
	}
	c.limit = res
	c.held = int(res.Held())
	return true
}

func (c *embedCall) endLimit(fn func(LimitReservation)) {
	if c.limit != nil && !c.limitEnd {
		c.limitEnd = true
		fn(c.limit)
	}
}

// embedState is what the attempts of one walk leave behind.
type embedState struct {
	n        int // attempts
	wires    int // wire calls (EmbedRaw), over every attempt — the census count (Σ acquire = Σ wire); the meter reads c.wired
	admitted time.Time
	lease    *dispatch.Lease // the First/Next lease of the latest attempt
	k        int             // blocks of the latest attempt's weight plan
	later    int             // the largest of its blocks after the first (0: one block)
	done     int             // its blocks with a result
	vecs     [][]float32
	dims     int            // the vectors' length (policy or learned)
	usage    dispatch.Usage // Σ over every block with a result, saturated (usageTotal)
	// failedBlock is the block whose wire call ended the latest attempt
	// (-1: none); ≥ 1 is the sticky stop.
	failedBlock int
	abort       string // the dispatcher abort of that block's lease, if any
	failed      []failedAttempt
	last        failedAttempt
	// unpaid are the wire calls of blocks that reported no usage (a failed
	// block, an answer without usage), over every attempt: each one whose
	// trace saw wire contact books the estimate of its inputs that no block
	// with a usage served (endAfterWire, Delta-Review 4 F4-1 and 5).
	unpaid []*blockWire
	// paid marks the inputs a block with a usage served, on any row (nil
	// until the first one): their usage is what they book, a failed try on
	// a row the request failed over from books nothing for them.
	paid []bool
}

// blockWire is one block's wire call as the meter sees it: contact is marked
// from inside net/http by the block's own trace (wireContactTrace, beside the
// request's c.wired); [lo, hi) are the block's inputs — a later row's blocks
// can be cut differently, so the meter reads inputs, not block numbers.
type blockWire struct {
	contact atomic.Bool
	lo, hi  int
}

// markPaid marks the inputs [lo, hi) of the request's n as served by a block
// with a usage.
func (st *embedState) markPaid(lo, hi, n int) {
	if st.paid == nil {
		st.paid = make([]bool, n)
	}
	for i := lo; i < hi; i++ {
		st.paid[i] = true
	}
}

// unpaidEstimate is Σ over the blocks that met a backend and reported no
// usage of the meter's estimate of their inputs that no block with a usage
// served (restEstimate), saturated (usageTotal) — each input at most once:
// an input two rows had without a usage (a failover, then a later failure on
// a row cut into other blocks) is counted by the first of those blocks. A
// flag is read here, at the end of the request: a hook that fires after
// EmbedRaw returned still counts.
func (c *embedCall) unpaidEstimate(st *embedState) int {
	sum := 0
	var counted []bool // the paid inputs, then every input an estimate counted
	for _, bw := range st.unpaid {
		if !bw.contact.Load() {
			continue
		}
		if counted == nil {
			counted = make([]bool, len(c.req.inputs))
			copy(counted, st.paid)
		}
		sum = usageTotal(sum, c.restEstimate(bw.lo, bw.hi, counted))
		for i := bw.lo; i < bw.hi; i++ {
			counted[i] = true
		}
	}
	return sum
}

// meterEnd is how a request whose blocks reported a usage ends its
// reservation: Settle with that usage plus the estimate of its blocks that
// met a backend without one (F4-1), while the sum (saturated, usageTotal)
// stays below held — what the reservation holds; at or above it Keep, which
// books exactly that (Delta-Review 5 F5-1: the estimate never charges more
// than Keep, also when the estimate of all inputs is above the bucket's
// capacity). A reported usage at or above held is booked as reported, with
// no estimate on top.
func meterEnd(usage, estimate, held int) (settle int, keep bool) {
	if estimate <= 0 || usage >= held {
		return usage, false
	}
	if charge := usageTotal(usage, estimate); charge < held {
		return charge, false
	}
	return 0, true
}

// walk runs the planner and the chain walk, then answers.
func (c *embedCall) walk(chain []backends.Backend) {
	if ended, _ := c.verdict(); ended {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		return
	}
	if c.d.Planner == nil {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return
	}
	n := len(c.req.inputs)
	plan, err := c.d.Planner.Plan(c.ctx, c.ca, AdmissionInput{Chain: chain, Units: n, SubBatch: c.lim.subBatch})
	if err != nil {
		if ended, code := c.verdict(); ended {
			c.endLimit(func(l LimitReservation) { l.Reject(code, 0) })
			return
		}
		c.admissionRefused(err, 0, false, nil)
		return
	}
	if ended, _ := c.verdict(); ended {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		return
	}
	// k = 1 without the continuation door (design/02 §4.8): more inputs than
	// one upstream request carries are refused before any acquire.
	if !plan.Continuation && n > c.lim.subBatch {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		c.refuseParam(unsupportedValue("input", fmt.Sprintf("at most %d inputs per request", c.lim.subBatch)))
		return
	}
	stop := c.d.watch(c.ctx, c.cancel, newWatchTarget(c.ca, c.req.model, c.alias.ID, c.gen))
	defer stop()

	st := embedState{failedBlock: -1}
	rules := llm.WalkRules{WindowRule: true, AllowedClasses: c.d.Planner.AllowedClasses(), Timeout: c.attemptTimeout}
	served, attempts, werr := llm.WalkChain(c.ctx, plan.Chain, c.alias.Role, plan.Acquirer, llm.PoolReporter(c.d.Pool), rules,
		func(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
			return c.attempt(&st, plan, runCtx, b, lease, timeout)
		})
	stop()
	c.answer(plan, served, attempts, werr, &st)
}

// attemptTimeout is one wire call's deadline (§4.12): the alias's
// timeout_s, else the row's role timeout over gateway.request_timeout,
// capped by the tenant ceiling for a tenant alias and by what is left of the
// request deadline — read per block, so every block lives inside it.
func (c *embedCall) attemptTimeout(b *backends.Backend) time.Duration {
	t := b.TimeoutFor(c.alias.Role, c.s.RequestTimeout)
	if c.alias.Policy.TimeoutS.Set {
		t = time.Duration(c.alias.Policy.TimeoutS.V) * time.Second
	}
	if tenantAlias(c.alias) && c.s.TenantAliasCeiling.Timeout > 0 {
		t = min(t, c.s.TenantAliasCeiling.Timeout)
	}
	return max(min(t, time.Until(c.dl)), time.Millisecond)
}

// attempt is one attempt on row b: the weight plan of the row, then its
// blocks (subBlockRun) — block one under the walker's lease.
func (c *embedCall) attempt(st *embedState, plan Plan, runCtx context.Context, b *backends.Backend,
	lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
	st.n++
	st.lease = lease
	if st.admitted.IsZero() {
		st.admitted = time.Now()
	}
	setWriteDeadline(c.w, time.Now().Add(time.Until(c.dl)+writeMargin))
	n := len(c.req.inputs)
	blocks := plan.Acquirer.Blocks()
	if err := checkBlocks(blocks, n); err != nil {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.internal("the planner's weight plan does not fit the request (planner defect)", "error", err)
			return CodeInternalError
		})
	}
	if !continuationAllowed(blocks, plan.Continuation) {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.refuseParam(unsupportedValue("input", fmt.Sprintf("at most %d inputs per request", blocks[0])))
			return CodeUnsupportedValue
		})
	}
	// The decode transient of the largest block of this row's plan, before
	// its first wire call — the blocks run one after the other, so one
	// transient is held at a time; a later attempt with larger blocks grows
	// it (H-3). The share of decodeFloor is booked before the acquire; this
	// grows it to the block the row's capacity set. Refused ⇒ 503
	// server_overloaded after the queue wait, before any wire contact.
	if !c.reserveDecode(slices.Max(blocks), c.lim.nativeDims) {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.rec.Reason = CodeServerOverloaded
			WriteOverloaded(c.w)
			return CodeServerOverloaded
		})
	}
	// The upstream bodies of this row's blocks, before the first wire call
	// (FOLGE-E): EmbedRaw encodes each block anew — every attempt's, the
	// bodies of an earlier attempt stay on the heap until the collector
	// runs. The first attempt's texts were booked before the acquire
	// (reserveUpstreamTexts). Refused ⇒ 503 server_overloaded after the
	// queue wait, before any wire contact, as the decode share above.
	model := b.ModelFor(c.alias.Role).Model
	need := c.upstreamBooking(*b, model, blocks) - c.textsBooked
	c.textsBooked = 0
	if !c.ledger.cover(need) {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.rec.Reason = CodeServerOverloaded
			WriteOverloaded(c.w)
			return CodeServerOverloaded
		})
	}
	st.k, st.done, st.failedBlock, st.abort = len(blocks), 0, -1, ""
	st.later = 0
	if len(blocks) > 1 {
		st.later = slices.Max(blocks[1:])
	}
	st.vecs = make([][]float32, n)
	st.dims = c.lim.nativeDims
	run := subBlockRun{ctx: c.ctx, acq: plan.Acquirer, role: c.alias.Role, timeout: c.attemptTimeout, alive: c.alive}
	return run.run(runCtx, b, lease, timeout, blocks,
		func(i, lo, hi int, bl *dispatch.Lease, rc context.Context, t time.Duration) (dispatch.Usage, error) {
			return c.wireBlock(st, b, model, i, lo, hi, bl, rc, t)
		})
}

// upstreamBooking is what the wire calls of an attempt of blocks (their
// sizes, in input order) on row b to model allocate at most: each block's
// body — one allocation of its exact length, embed.WireBodyLen (FOLGE-E
// hardening 1: internal/embed sizes the body before it writes it; the
// json.Encoder before it allocated up to 7.105 × the body and the route
// booked eight times it) — the allocation's rounding to a size class
// (embed.WireBodySlack), the transport's copy of the body onto the
// connection (embed.WireCopyBuffer, or the body's length when shorter) and
// embedWireCallBytes for its call — and the retry of the call: the copy once
// more and embedWireRetryBytes (FOLGE-E hardening 2, L-4).
//
// The retry is booked with the first call, for every block, whether it
// happens or not: httpx.DoRetryOnce decides it inside embed.EmbedRaw, after
// the first call died before its answer, and sends the same body once more
// on a fresh connection; the route gets control back only when the retry
// has returned, so there is no point between the two calls where it could
// book. A booking at the retry itself would take a callback through
// httpx.DoRetryOnce and embed.EmbedRaw (and a refusal between the two
// calls); the booking with the first call holds the same bound without it,
// for at most EmbedWireRetryCeiling at a block per input.
func (c *embedCall) upstreamBooking(b backends.Backend, model string, blocks []int) int64 {
	var n int64
	lo := 0
	for _, size := range blocks {
		body := embed.WireBodyLen(b, model, c.req.inputs[lo:lo+size])
		copied := int64(min(body, embed.WireCopyBuffer))
		n += int64(body) + embed.WireBodySlack + copied + embedWireCallBytes + copied + embedWireRetryBytes
		lo += size
	}
	return n
}

// upstreamTexts is the encoded size of the request's texts in the upstream
// bodies: every text as the wire encodes it (embed.WireStringLen) and its
// separator — at most what an attempt's bodies hold of them, whatever its
// blocks.
func (c *embedCall) upstreamTexts() int64 {
	var n int64
	for _, s := range c.req.inputs {
		n += int64(embed.WireStringLen(s)) + 1
	}
	return n
}

// embedWireCallBytes is what one wire call allocates beside its body and
// its copy (embed.WireCopyBuffer) — the request, its headers and traces,
// the block's contexts and lease, the answer's reader and decoder state. A
// request of k blocks makes k calls (up to embedding.max_inputs one-input
// blocks when a row's capacity E is 1). Measured over the route (TotalAlloc
// of one request of 256 three-byte texts at E = 1 … 256, the slope over
// 1 … 256 blocks, the in-process engine's server side included — an upper
// bound of the route's own): 12 943 bytes per call before FOLGE-E
// hardening 1, 12 098 … 13 241 bytes after it (18 runs; the json.Encoder's
// allocations of the tiny bodies are gone), rounded up to the next 2 KiB —
// the next 4 KiB (16 KiB) sat within the measurements' spread of the pin's
// lower bound (FOLGE-E, bau report §Härtung 1). The only term of the route
// that grows with something other than the body's bytes and its nesting
// (S-2): k, and k is capped by modelalias.CeilingEmbeddingMaxInputs (every
// block holds an input), so a request books at most EmbedWireCallCeiling
// (3.5 MiB) for its calls — for the budget floor (V39) a constant at that
// ceiling. A derived mechanism constant, not policy;
// TestEmbedWireCallReserveIsTheMeasuredOne fails when a call needs more or
// more than one 4-KiB step less.
const embedWireCallBytes = 14 << 10

// embedWireRetryBytes is what the retry of a wire call allocates beside the
// second copy of its body (embed.WireCopyBuffer, or the body's length when
// shorter): httpx.DoRetryOnce answers a transient transport error of the
// first call — the connection died before the answer — with the same body
// on a fresh connection, so the retry is a second call with the
// connection's state (dial, its buffers and the transport's read and write
// loops; the in-process engine's side of the new connection included),
// the first call's error and the retry's log line. Measured over the route
// (TotalAlloc of a request of 256 three-byte texts at E = 1 on an engine
// that drops the first call of every block, against the same request
// answered at once; FOLGE-E hardening 2, bau report §Härtung 2): 27 783 …
// 29 039 bytes per block (12 runs at load 2–5), rounded up to the next
// 2 KiB. A request of k blocks books it k times — once per block, whether
// the block is retried or not (upstreamBooking) — at most
// EmbedWireRetryCeiling with the copies. A derived mechanism constant, not
// policy; TestEmbedWireRetryReserveIsTheMeasuredOne fails when a retry
// needs more or more than one 4-KiB step less.
const embedWireRetryBytes = 30 << 10

// The terms of the embeddings route as the configuration's budget floor
// reads them (FOLGE-E, lead decision 5). Their reader is the budget
// dimensioning (V39) before the flip: V39 counts none of them yet and no
// code reads them before it, so TestEmbedExportsAreTheirDerivations holds
// each against its derivation (FOLGE-E hardening 2, L-2).
// EmbedInputSlotReserve per input (at most
// modelalias.CeilingEmbeddingMaxInputs of them), EmbedWireCallReserve, EmbedUpstreamSlack and EmbedWireCopyReserve per
// block of an attempt and their ceilings at a block per input
// (EmbedWireCallCeiling, EmbedUpstreamSlackCeiling, EmbedWireCopyCeiling;
// the copy is the body's length when that is shorter, so the three
// ceilings are an upper bound); the retry of every block's call books a
// second copy and EmbedWireRetryReserve (EmbedWireRetryCeiling: both at a
// block per input); the upstream bodies themselves are the texts as the
// wire encodes them (embed.WireStringLen, at most six bytes per decoded
// byte) and embed.WireFrameMax with the model per block.
// EmbedEncodingRawMax and EmbedDimensionsRawMax are the longest raw values
// of encoding_format and dimensions the route decodes — a longer one is
// refused before its decode, so neither books more than its own bytes.
const (
	EmbedInputSlotReserve     = embedInputSlotBytes
	EmbedWireCallReserve      = embedWireCallBytes
	EmbedWireCallCeiling      = embedWireCallBytes * modelalias.CeilingEmbeddingMaxInputs
	EmbedUpstreamSlack        = embed.WireBodySlack
	EmbedUpstreamSlackCeiling = embed.WireBodySlack * modelalias.CeilingEmbeddingMaxInputs
	EmbedWireCopyReserve      = embed.WireCopyBuffer
	EmbedWireCopyCeiling      = embed.WireCopyBuffer * modelalias.CeilingEmbeddingMaxInputs
	EmbedWireRetryReserve     = embedWireRetryBytes
	EmbedWireRetryCeiling     = (embed.WireCopyBuffer + embedWireRetryBytes) * modelalias.CeilingEmbeddingMaxInputs
	EmbedEncodingRawMax       = encodingRawMax
	EmbedDimensionsRawMax     = jsonIntMax
)

// refuseAfterAcquire ends the run with the route's own answer (written by
// verdict) and returns the cause the attempt ends with: a cancellation —
// no Classify fault, no health report against the row.
func (c *embedCall) refuseAfterAcquire(answer func() string) error {
	c.own = answer
	c.cancel(errRouteRefused)
	return context.Cause(c.ctx)
}

// alive is the run check before every continuation: an ended run asks for no
// further lease. The request deadline is an admission outcome there — the
// next block's acquire cannot happen in time, the twin of a deadline that
// ends a waiting Continue (the planner's writer, N-1) — and travels in the
// acquire hull; every other cause is the run's own end (verdict).
func (c *embedCall) alive() error {
	c.syncLifecycle()
	if c.ctx.Err() == nil {
		return nil
	}
	err := fmt.Errorf("oai: the run ended before the next block: %w", context.Cause(c.ctx))
	if errors.Is(err, context.DeadlineExceeded) {
		return &llm.AdmissionError{Err: err}
	}
	return err
}

// wireBlock is the wire call of block i over the inputs [lo, hi), under
// lease (the walker's for block one, its continuation lease for every
// further one — subBlockRun owns both).
func (c *embedCall) wireBlock(st *embedState, b *backends.Backend, model string, i, lo, hi int, _ *dispatch.Lease,
	rc context.Context, timeout time.Duration) (dispatch.Usage, error) {
	// <oaiReqID>-a<n>, fresh per upstream request (design/02 §4.5.3).
	rc = fedhop.WithRequestID(rc, attemptRequestID(c.id, st.wires+1))
	// A run that ended while the lease was acquired does not dial (W02-07
	// R4-1).
	c.syncLifecycle()
	if rc.Err() != nil {
		err := c.blockFailure(st, b, i, rc, fmt.Errorf("oai: the run ended before the wire call: %w", context.Cause(rc)))
		return dispatch.Usage{}, err
	}
	// st.wires counts the wire calls for the census; c.wired is the meter's
	// wire contact of the request, bw.contact the block's — both marked from
	// inside net/http by the block's traces (composed on its context) — on
	// every try of httpx.DoRetryOnce (wireContactTrace).
	st.wires++
	var conn atomic.Bool
	bw := &blockWire{lo: lo, hi: hi}
	tctx := httptrace.WithClientTrace(rc, wireContactTrace(&c.wired, &conn))
	tctx = httptrace.WithClientTrace(tctx, wireContactTrace(&bw.contact, &conn))
	wctx, cancel := context.WithTimeout(tctx, timeout)
	defer cancel()
	vecs, promptTokens, err := embed.EmbedRaw(wctx, *b, model, c.req.inputs[lo:hi], st.dims,
		embedReadCap(hi-lo, st.dims, c.s.EmbeddingMaxFloats))
	// A block that ended by its context with a connection in hand may still
	// write (HTTP/2 returns on the context without waiting for the stream's
	// writer): wire contact — the conservative side, as at the chat route.
	if err != nil && conn.Load() && wctx.Err() != nil {
		c.wired.Store(true)
		bw.contact.Store(true)
	}
	// A block without a usage — failed, or answered without one — books the
	// estimate of its inputs if it met a backend and no block with a usage
	// served them by the end of the request (endAfterWire); a block with a
	// usage marks its inputs paid.
	if err != nil || promptTokens <= 0 {
		st.unpaid = append(st.unpaid, bw)
	} else {
		st.markPaid(lo, hi, len(c.req.inputs))
	}
	if err != nil {
		return dispatch.Usage{}, c.blockFailure(st, b, i, rc, err)
	}
	if st.dims <= 0 {
		st.dims = len(vecs[0])
		// Every later block is read up to the cap of the learned dimension
		// (embedReadCap with st.dims), which can pass the floats ceiling's
		// the decode share was booked for: it grows with the answer's before
		// the next block is asked for (Fremd-Review 3 F3-8).
		if !c.learnReserve(st.dims) || (st.later > 0 && !c.reserveDecode(st.later, st.dims)) {
			c.cancel(errAnswerOverloaded) // verdict answers it; the block itself served
		}
	}
	copy(st.vecs[lo:hi], vecs)
	st.done++
	u := dispatch.Usage{PromptTokens: promptTokens}
	// Summed as the meter counts (usageTotal): a negative count is 0, the
	// sum saturates — an overflowed negative sum would settle as 0, a full
	// refund (Fremd-Review 3 N-2).
	st.usage.PromptTokens = usageTotal(st.usage.PromptTokens, promptTokens)
	return u, nil
}

// blockFailure records the failure of block i and returns the error the
// attempt ends with. From block two on, a failure the request deadline
// caused (the frame is spent, not the row) becomes errBlockDeadline — a
// cancellation for the walker, no health report (Fremd-Review 1 F-3) — and
// every failure of a block ≥ 2 is the sticky stop (failedBlock ≥ 1, unless
// the run itself ended: verdict answers first).
func (c *embedCall) blockFailure(st *embedState, b *backends.Backend, i int, rc context.Context, err error) error {
	if i > 0 && errors.Is(context.Cause(c.ctx), context.DeadlineExceeded) {
		err = fmt.Errorf("block %d: %w", i, errBlockDeadline)
	}
	st.failedBlock, st.abort = i, llmlog.AbortClass(rc)
	st.last = failedAttempt{b: b, err: err}
	st.failed = append(st.failed, st.last)
	return err
}

// admissionRefused answers an admission failure through the planner's
// writer (A03-W8 handover). wired: a byte of the request reached a backend
// in an earlier attempt (a failover acquire refused behind a row that had
// the request) — the reservation ends as after the wire (endAfterWire over
// st), it is not rejected.
func (c *embedCall) admissionRefused(err error, waited time.Duration, wired bool, st *embedState) {
	reason := c.d.Planner.WriteAdmissionError(c.ctx, c.w, err)
	if reason == "" {
		// The writer saw the run context canceled: the route answers its
		// own cause; only a gone client stays unanswered (W02-07 F2-1).
		if ended, code := c.verdict(); ended {
			reason = code
		} else {
			c.internal("the admission writer wrote nothing on a live request", "error", err)
			reason = CodeInternalError
		}
	}
	c.rec.Reason = reason
	if wired {
		c.endAfterWire(st)
		return
	}
	c.endLimit(func(l LimitReservation) { l.Reject(reason, waited) })
}

// endAfterWire ends the reservation of a request that made wire calls: no
// byte reached a backend in any of them (c.wired unset — a refused or
// timed-out connect, a failed handshake) ⇒ Abort, the full refund; after
// wire contact never: Keep when no block reported a usage (the chat route's
// rule, W06-10 hardening H-7), else Settle with the usage the blocks
// reported plus the estimate of every block that met a backend without
// reporting one — the client gone during it, an upstream error, an answer
// without usage: the chat route's rule per block (Delta-Review 4 F4-1) —
// counted over its inputs that no block with a usage served by the end of
// the request (unpaidEstimate). A failover follows the chat route: inputs a
// later row served with a usage book that usage once, the failed try on the
// row the walk left books nothing for them, a block served only in part the
// estimate of the rest (Delta-Review 5; the rows' blocks can be cut
// differently, the inputs decide). The estimate never books more than Keep (meterEnd): a charge that reaches
// what the reservation holds — the estimate of all inputs, at most the
// bucket's capacity the admission reported (held) — ends with Keep, unless
// the reported usage alone reaches it, which settles as reported
// (Delta-Review 5 F5-1). A block that reached no backend books nothing; the
// blocks after a failed one never reached one (the sticky stop).
func (c *embedCall) endAfterWire(st *embedState) {
	wired := c.wired.Load()
	usage := st.usage
	c.endLimit(func(l LimitReservation) {
		switch {
		case !wired:
			l.Abort()
		case usage.PromptTokens <= 0:
			l.Keep()
		default:
			n, keep := meterEnd(usage.PromptTokens, c.unpaidEstimate(st), c.held)
			if keep {
				l.Keep()
				return
			}
			usage.PromptTokens = n
			l.Settle(usage)
		}
	})
}

// answer writes the outcome of the walk and its one llmlog row.
func (c *embedCall) answer(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *embedState) {
	if st.lease != nil {
		c.rec.QueueWaitMs = st.lease.WaitDur().Milliseconds()
		c.rec.Class = st.lease.Class().String()
	}
	out := c.outcome(plan, attempts, err, st)
	// A row and the end of the reservation (endAfterWire) only for a request
	// that made wire calls: without one there is no LLM call (close() aborts
	// the reservation, or the admission refusal rejected it).
	if st.wires > 0 {
		// The access record carries the usage the backends reported, as the
		// row books it — also for an answer that is no 200 (F-9 (a)); the
		// reservation settles it plus the estimate of the blocks that met a
		// backend without one (endAfterWire).
		c.rec.PromptTokens = st.usage.PromptTokens
		if st.usage.PromptTokens > 0 {
			c.rec.Usage = llmlog.UsageBackend
		}
		c.endAfterWire(st)
		c.record(plan, served, attempts, err, st, out)
	}
}

// embedOutcome is what the client got, for the row.
type embedOutcome struct {
	finish   string
	redacted *llmlog.RedactedErr
}

// outcome answers the walk: the run's own end first (watcher, shutdown,
// client, the route's refusals), then success, the admission writer's
// domain, the sticky stop of a block ≥ 2 and the attempt mapping of a
// failed first block.
func (c *embedCall) outcome(plan Plan, attempts []llm.ChainAttempt, err error, st *embedState) embedOutcome {
	if ended, code := c.verdict(); ended {
		if cause := context.Cause(c.ctx); errors.Is(cause, errAnswerOverloaded) || errors.Is(cause, errRouteRefused) {
			return embedOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Code: code}}
		}
		return embedOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
	}
	if err == nil {
		return c.success(st)
	}
	if isAdmissionFailure(err) {
		waited := time.Duration(0)
		if st.lease == nil {
			waited = time.Since(c.entry)
		}
		c.admissionRefused(err, waited, st.wires > 0 && c.wired.Load(), st)
		return embedOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Class: "admission", Code: c.rec.Reason}}
	}
	if len(attempts) == 0 {
		slog.Warn("gateway: the walk found no row to try", "alias", c.alias.Name, "role", c.alias.Role,
			"error", err, "request_id", c.w.Header().Get("X-Request-ID"))
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return embedOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Code: CodeNoEligibleBackend}}
	}
	if st.last.err == nil {
		st.last.err = err
	}
	if st.failedBlock >= 1 && st.abort == "" {
		return c.stickyStop(st)
	}
	lastClass := attempts[len(attempts)-1].Class
	if st.abort != "" {
		lastClass = st.abort
	}
	a := mapAttemptFailure(st.last, st.failed, lastClass, answerContext{
		alias: c.alias, chain: plan.Chain, relay4xx: c.s.RelayUpstream4xxMessages, snapshot: c.d.Pool.Snapshot,
	})
	writeRetryAfter(c.w.Header(), a.retryAfter)
	c.refuse(a.code, a.message, a.shouldRetry)
	return embedOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Class: a.class, Status: a.status, Code: a.code}}
}

// stickyStop is the answer of a failed block ≥ 2 (design/02 §4.8 "Sticky
// Backend"): the attempt committed with block one, no failover may mix a
// second vector build in — 503 upstream_unavailable with Retry-After (the
// smallest upstream Retry-After of the failed attempts, else the smallest
// step of dispatch.external_retry_after_steps), whatever the block's own
// error class was.
func (c *embedCall) stickyStop(st *embedState) embedOutcome {
	ra := smallestRetryAfter(st.failed)
	if ra <= 0 {
		if steps := c.d.RetryAfterSteps(); len(steps) > 0 {
			ra = steps[0]
		}
	}
	writeRetryAfter(c.w.Header(), ra)
	c.refuse(CodeUpstreamUnavailable, upstreamUnavailableMessage, true)
	providerClass := ""
	if st.last.b != nil {
		providerClass = st.last.b.ProviderClass
	}
	status := 0
	var se *httpx.StatusError
	if errors.As(st.last.err, &se) {
		status = se.Code
	}
	return embedOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{
		Class: backends.Classify(st.last.err, providerClass).String(), Status: status, Code: CodeUpstreamUnavailable,
	}}
}

// verdict answers a run the watcher, the shutdown, the client or the route
// itself ended: ended reports one of them, code is what was written (""
// when the client is gone).
func (c *embedCall) verdict() (ended bool, code string) {
	c.syncLifecycle()
	cause := context.Cause(c.ctx)
	switch {
	case errors.Is(cause, errGatewayOff):
		c.rec.Reason = CodeNotFound
		NotFound(c.w, c.r)
		return true, CodeNotFound
	case errors.Is(cause, errModelGone):
		refuseModelNotFound(c.w, c.r)
		return true, CodeModelNotFound
	case errors.Is(cause, errShutdown):
		c.refuse(CodeInternalError, shutdownMessage, true)
		return true, CodeInternalError
	case c.client.Err() != nil:
		return true, "" // the client is gone: nothing reaches it
	case errors.Is(cause, errAnswerOverloaded):
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return true, CodeServerOverloaded
	case errors.Is(cause, errRouteRefused) && c.own != nil:
		return true, c.own()
	}
	return false, ""
}

// syncLifecycle puts an ended lifecycle on the run synchronously (W02-07
// R3-1).
func (c *embedCall) syncLifecycle() {
	if c.d.Lifecycle != nil && c.d.Lifecycle.Err() != nil {
		c.cancel(errShutdown)
	}
}

// success checks once more before the write (D8) and streams the answer.
func (c *embedCall) success(st *embedState) embedOutcome {
	t := newWatchTarget(c.ca, c.req.model, c.alias.ID, c.gen)
	switch err := c.d.stillServes(c.ctx, t); {
	case errors.Is(err, errGatewayOff), errors.Is(err, errModelGone):
		c.cancel(err)
		_, code := c.verdict()
		return embedOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
	case err != nil:
		if ended, code := c.verdict(); ended {
			return embedOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
		}
		grantReadFailed(c.w, c.r, err)
		return embedOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Code: CodeInternalError}}
	}
	dims := st.dims
	if c.req.dims > 0 {
		dims = c.req.dims
	}
	// The meter's view of the request's op bucket, on the successful answer
	// only (design/06 §4.4; lead decision M2 (c)).
	if c.limit != nil {
		for k, v := range c.limit.Headers() {
			c.w.Header()[k] = v
		}
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	if err := writeEmbeddings(c.w, embedAnswer{vecs: st.vecs, dims: dims, base64: c.req.base64,
		model: c.alias.Name, promptTokens: st.usage.PromptTokens}); err != nil {
		slog.Warn("gateway: failed to write the answer", "error", err, "request_id", c.w.Header().Get("X-Request-ID"))
		return embedOutcome{finish: llmlog.FinishAborted}
	}
	return embedOutcome{finish: llmlog.FinishStop}
}

// embedAnswer is what the streamed answer encodes.
type embedAnswer struct {
	vecs         [][]float32
	dims         int // the client's dimensions, else the native length
	base64       bool
	model        string
	promptTokens int
}

// writeEmbeddings streams the answer (design/02 §4.8 "Antwort"): the head,
// one item per vector — its float list number by number (writeFloats, the
// encoding/json form of a float32), or its base64 string through a
// streaming base64 encoder, into the encoder buffer, flushed after the item
// — and the tail. The whole JSON never exists in memory: the buffer and one
// number at a time. (Not the json.Encoder the design names: its encode state
// comes from a sync.Pool, which the race detector empties at random — the
// encoder then allocates a fresh item buffer per item; Fremd-Review 1 F-1.) A vector longer than dims is cut to v[:d]
// and L2-renormalized in place (E-02-10); base64 is the little-endian
// float32 bytes in standard base64.
func writeEmbeddings(w io.Writer, a embedAnswer) error {
	bw := bufio.NewWriterSize(w, EmbedEncodeBuffer)
	if _, err := bw.WriteString(`{"object":"list","data":[`); err != nil {
		return err
	}
	for i, v := range a.vecs {
		if a.dims > 0 && a.dims < len(v) {
			v = renormalize(v[:a.dims])
		}
		sep := `{"object":"embedding","index":`
		if i > 0 {
			sep = `,` + sep
		}
		if _, err := bw.WriteString(sep + strconv.Itoa(i) + `,"embedding":`); err != nil {
			return err
		}
		var err error
		if a.base64 {
			err = writeBase64(bw, v)
		} else {
			err = writeFloats(bw, v)
		}
		if err != nil {
			return err
		}
		if err := bw.WriteByte('}'); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
	model, err := json.Marshal(a.model)
	if err != nil {
		return err
	}
	tokens := strconv.Itoa(a.promptTokens)
	if _, err := bw.WriteString(`],"model":` + string(model) + `,"usage":{"prompt_tokens":` + tokens + `,"total_tokens":` + tokens + "}}\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// writeFloats writes v as a JSON array of float32 numbers in exactly the
// form encoding/json gives a float32 (shortest round-trip digits, 'f' between
// 1e-6 and 1e21, else 'e' with a two-digit negative exponent cut to one —
// pinned against json.Marshal in TestFloat32FormIsEncodingJSONs). A
// non-finite component (rawQuality refused those on the wire) is an error,
// like encoding/json's.
func writeFloats(w *bufio.Writer, v []float32) error {
	var num [32]byte
	if err := w.WriteByte('['); err != nil {
		return err
	}
	for j, x := range v {
		if j > 0 {
			_ = w.WriteByte(',') // a write error is sticky in the bufio.Writer
		}
		b, err := appendFloat32(num[:0], x)
		if err != nil {
			return err
		}
		_, _ = w.Write(b)
	}
	return w.WriteByte(']')
}

// appendFloat32 is encoding/json's float32 encoder (floatEncoder, bits 32).
func appendFloat32(b []byte, x float32) ([]byte, error) {
	f := float64(x)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return b, fmt.Errorf("oai: non-finite embedding component %v", f)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 32)
	if format == 'e' {
		// clean up e-09 to e-9
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b, nil
}

// base64Chunk is the number of floats writeBase64 hands the encoder at
// once (a multiple of 3: whole base64 groups per chunk).
const base64Chunk = 768

// writeBase64 writes v as a JSON string of its little-endian float32 bytes
// in standard base64 — streamed through the encoder, never the whole
// string in memory (the base64 alphabet and padding need no JSON escape).
func writeBase64(w *bufio.Writer, v []float32) error {
	if err := w.WriteByte('"'); err != nil {
		return err
	}
	be := base64.NewEncoder(base64.StdEncoding, w)
	var chunk [base64Chunk * EmbedFloatBytes]byte
	for lo := 0; lo < len(v); lo += base64Chunk {
		part := v[lo:min(lo+base64Chunk, len(v))]
		for j, x := range part {
			binary.LittleEndian.PutUint32(chunk[j*EmbedFloatBytes:], math.Float32bits(x))
		}
		if _, err := be.Write(chunk[:len(part)*EmbedFloatBytes]); err != nil {
			return err
		}
	}
	if err := be.Close(); err != nil {
		return err
	}
	return w.WriteByte('"')
}

// renormalize scales v to unit L2 norm in place (a zero vector stays as it
// is — rawQuality refused those on the wire).
func renormalize(v []float32) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if norm := math.Sqrt(sumSq); norm > 0 {
		for i, x := range v {
			v[i] = float32(float64(x) / norm)
		}
	}
	return v
}

// record writes the request's ONE llmlog row (design/06 §4.6): the walk's
// telemetry, every wire attempt in metadata.chain, metadata.sub_batches —
// the blocks of the last attempt's weight plan — and sub_batches_done — the
// blocks of it that got a result —, the tokens of every block with a result,
// a RedactedErr — never the walk's raw error (B20).
func (c *embedCall) record(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *embedState, out embedOutcome) {
	tel := llmlog.Entry{RequiredSensitivity: string(c.required), PromptTokens: st.usage.PromptTokens,
		Metadata: map[string]any{"sub_batches": st.k, "sub_batches_done": st.done}}
	llm.StampServed(&tel, c.alias.Role, served)
	var class dispatch.Class
	if plan.Acquirer != nil {
		class = plan.Acquirer.Class()
	}
	llm.ApplyDispatchOutcome(&tel, attempts, err, class)
	usage := ""
	if st.usage.PromptTokens > 0 {
		usage = llmlog.UsageBackend
	}
	request := ""
	if c.s.BodyLogging == llmlog.BodyLoggingFull || (c.s.BodyLogging == llmlog.BodyLoggingErrors && out.redacted != nil) {
		// The row's text of the input is sized, then booked, then written
		// (FOLGE-E, as the chat route since FOLGE-P hardening 6):
		// string(input) copied the whole value outside the reservation, and
		// the row's cleaning grew invalid bytes up to 8 × before its cut.
		// Under an exhausted budget the row goes without it.
		if n := bodyLogSize(c.req.input); c.ledger.cover(int64(n)) {
			request = bodyLogText(c.req.input, n)
		}
	}
	c.d.Record(llmlog.GatewayRow{
		Pipeline:   embedPipeline,
		ConsumerID: c.ca.ConsumerID,
		Gateway: llmlog.Gateway{
			Alias: c.alias.Name, Hop: c.rec.Hop, Usage: usage, ClientReqID: clientRequestID(c.r),
			Finish: out.finish, ModelKeyID: c.ca.ModelKeyID,
		},
		Telemetry:   tel,
		Attempts:    attempts,
		Err:         out.redacted,
		Request:     request,
		BodyLogging: c.s.BodyLogging,
	})
}
