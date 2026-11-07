package oai

import (
	"bufio"
	"context"
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
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/rerank"
)

// POST /v1/rerank (Jina's form) and POST /v2/rerank (Cohere v2's form), one
// handler for both dialects (wave W02-10; design/02 §4.9, §4.2.1, §4.12;
// design/06 §4.1). The order of one request is the embeddings route's
// (embeddings.go; the stages are its free functions copied, chat.go and
// embeddings.go untouched — the extraction wave after W02-10 joins the
// three):
//
//	invariants (admit, inference dependencies) → write deadline at entry →
//	request deadline on the request context → body → parse (model, query,
//	documents, top_n, return_documents, max_tokens_per_doc) → Resolve →
//	kind rerank → policy (max_documents) → answer reserve →
//	RequiredSensitivity + scan → chain build (gateway wire rerank: every
//	protocol) → GateTenant → upstream texts → limiter (op rerank) → planner
//	(Units n, SubBatch) → k = 1 rule → watcher → WalkChain(attempt = the
//	sub-blocks of subblock.go: block one under First/Next, blocks 2…k under
//	Continue on the request context, sticky) → check before the write →
//	sorted answer → one llmlog row, one access record.
//
// Nothing is written before the last block returned: every failure is a
// real status. Every refusal names its code as the access record's reason.
//
// The route holds on the heap only what its in-flight reservation covers
// (the ledger of FOLGE-E at this route, requestLedger): every term is booked
// before its allocation, in the order the request allocates it — the body
// read into one buffer of its declared length (readRequestBody), the nesting
// state of the JSON check (jsonDepthStateBytes per level), the decoded model,
// query and documents (their exact decoded length and rerankDocSlotBytes
// each, sized before the decode — rerank_parse.go), the answer
// (rerankResultBytes per document and the encoder buffer), the sensitivity
// scan below credentials (scanBooking), the wire calls of every block of an
// attempt before its first one (rerankUpstreamBooking, rerank_wire.go: the
// body in one allocation of its length, the transport's copy, the answer's
// bounded read, the decode, the call and its retry) — their certain part before the
// acquire (reserveUpstreamTexts) — and the log row's text (bodyLogSize). A
// booking the budget cannot hold is 503 server_overloaded before the
// allocation — nothing booked, nothing allocated, no wire; the log row goes
// without its text.

// rerankPipeline is the llmlog pipeline of the route's row (a local name
// for the llmlog constant, the shape the promptguard census resolves).
const rerankPipeline = llmlog.PipelineGatewayRerank

// RerankDialect is the wire form of a rerank path: the path decides the
// dialect (design/02 §4.9), and the dialect only the default of
// return_documents — both share the request and the answer form, a
// superset of Jina's and Cohere v2's.
type RerankDialect int

const (
	// RerankJina is POST /v1/rerank: return_documents defaults to true.
	RerankJina RerankDialect = iota
	// RerankCohere is POST /v2/rerank: return_documents defaults to false.
	RerankCohere
)

// returnDocuments is the dialect's default of return_documents.
func (d RerankDialect) returnDocuments() bool { return d == RerankJina }

// rerankResultBytes is the answer's share per document (design/02 §4.2.1:
// "n_docs × 64 B"): the document's logit, turned into its relevance score in
// place (8 bytes), its slot in the sort order (8 bytes) and the meter's two
// flags of the block walk (rerankState.paid, unpaidEstimate's counted: a
// byte each) — 18 bytes, the rest is margin; the result's frame goes
// through the fixed encoder buffer (rerankEncodeBuffer). The document echo of
// return_documents is no part of it: the echo is the decoded document the
// parse booked (rerank_parse.go), written straight from it through the
// encoder buffer — never copied, never booked twice. A derived mechanism
// constant of the design, not policy (the policy values are
// rerank.max_documents and gateway.inflight_budget).
const rerankResultBytes = 64

// rerankEncodeBuffer is the encoder buffer of the answer: the results are
// written into it one by one and it is flushed when full, so the answer —
// with its echoes — never exists as one JSON in memory. Part of the answer
// reserve. A mechanism constant, not policy.
const rerankEncodeBuffer = 16 << 10

// The texts of the route's own refusals.
const rerankKindMessage = "The model does not support rerank."

// Rerank is the handler of POST /v1/rerank (RerankJina) and POST /v2/rerank
// (RerankCohere).
func Rerank(d Deps, dialect RerankDialect) http.Handler {
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
		c := newRerankCall(d, w, r, ca, entry, dialect)
		defer c.close()
		c.serve()
	})
}

// rerankCall is one request's state.
type rerankCall struct {
	d       Deps
	w       http.ResponseWriter
	r       *http.Request // carries ctx
	client  context.Context
	ctx     context.Context
	cancel  context.CancelCauseFunc
	stops   []func() bool
	s       Settings
	ca      *auth.ConsumerAuth
	rec     *AccessRecord
	entry   time.Time
	dl      time.Time
	id      string
	dialect RerankDialect

	alias    modelalias.Alias
	gen      uint64 // the alias registry generation read right before the entry Resolve (B-3)
	req      *rerankRequest
	lim      rerankLimits
	required backends.Sensitivity
	limit    LimitReservation
	limitEnd bool
	// held is what the reservation holds — the tokens Keep books
	// (LimitReservation.Held), the ceiling of a block estimate's charge.
	held int
	// wired: a byte of the request can have reached a backend in some try
	// of some block (wireContactTrace). Without it the reservation goes
	// back in full (Abort); after it, never (Settle with the usage, Keep
	// without one; the chat route's rule).
	wired atomic.Bool
	// own is the answer of a route refusal after an acquire (errRouteRefused).
	own func() string
	// ledger is the request share of the reservation and what the request
	// allocated against it.
	ledger requestLedger
	// textsBooked is the certain part of the first attempt's wire calls,
	// booked before the acquire (reserveUpstreamTexts).
	textsBooked int64
	// logits are the documents' logits in input order, then their relevance
	// scores (sigmoid, in place) — one slice of the answer reserve, made
	// once per request.
	logits []float64
}

func newRerankCall(d Deps, w http.ResponseWriter, r *http.Request, ca *auth.ConsumerAuth, entry time.Time, dialect RerankDialect) *rerankCall {
	s := d.Settings()
	c := &rerankCall{d: d, w: w, s: s, ca: ca, entry: entry, client: r.Context(), rec: AccessRecordFrom(r.Context()), dialect: dialect}
	if c.rec == nil {
		c.rec = &AccessRecord{} // outside a model group: filled, never read
	}
	c.rec.Op, c.rec.Stream = llmlog.GatewayOpRerank, false
	c.rec.Hop = len(fedhop.InboundFrom(r.Context()).Hop)
	setWriteDeadline(w, entry.Add(d.MaxWait()+s.RequestDeadline+writeMargin))
	c.dl = entry.Add(s.RequestDeadline)
	ctx, cancelDl := context.WithDeadline(r.Context(), c.dl)
	ctx, cancel := context.WithCancelCause(ctx)
	c.ctx, c.cancel = ctx, cancel
	c.stops = append(c.stops, func() bool { cancelDl(); return true })
	if d.Lifecycle != nil {
		c.stops = append(c.stops, context.AfterFunc(d.Lifecycle, func() { cancel(errShutdown) }))
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
// wire contact by keeping its charge (the usage is unknown), without it by
// Abort.
func (c *rerankCall) close() {
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

func (c *rerankCall) refuse(code, message string, shouldRetry bool) {
	c.rec.Reason = code
	WriteCode(c.w, code, message, shouldRetry)
}

func (c *rerankCall) overloaded() {
	refuseOverloaded(c.rec, c.w)
}

func (c *rerankCall) internal(msg string, args ...any) {
	slog.Error("gateway: "+msg, append(args, "path", c.r.URL.Path, "request_id", c.w.Header().Get("X-Request-ID"))...)
	c.rec.Reason = CodeInternalError
	WriteInternal(c.w)
}

// refuseParam answers a parse refusal with its param, bound to the
// vocabulary like WriteCode.
func (c *rerankCall) refuseParam(perr *paramError) {
	if perr == errParseOverloaded {
		c.overloaded()
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

func (c *rerankCall) serve() {
	if ended, _ := c.verdict(); ended {
		return
	}
	body, ok := c.readBody()
	if !ok {
		return
	}
	depth := jsonDepth(body)
	if !c.ledger.cover(depth * jsonDepthStateBytes) { // the JSON check's nesting state, booked before it runs
		c.overloaded()
		return
	}
	req, perr := parseRerankTop(body, c.ledger.cover)
	if perr != nil {
		c.refuseParam(perr)
		return
	}
	if !c.resolve(req.model, req.modelOverlong) {
		return
	}
	c.lim = rerankLimitsOf(c.alias, c.s)
	if perr := checkRerankPolicy(req, c.lim); perr != nil {
		c.refuseParam(perr)
		return
	}
	c.req = req
	if !c.reserveAnswer() {
		return
	}
	required := c.sensitivity()
	if !c.coverScan(required, body, depth) {
		c.overloaded()
		return
	}
	c.required = c.d.Federation.Scan(required, body)
	chain, ok := c.chain()
	if !ok {
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

// readBody reads the body under the strict cap and the in-flight budget
// (embedCall.readBody): the ledger starts here.
func (c *rerankCall) readBody() ([]byte, bool) {
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
			c.overloaded()
			return nil, false
		}
		return body, true
	case errors.Is(err, ErrBudgetExhausted):
		c.overloaded()
	case errors.As(err, &tooLarge):
		refuseTooLarge(c.rec, c.w, tooLarge.Limit)
	case c.client.Err() != nil:
		// the client went away while sending
	default:
		c.refuse(CodeInvalidJSON, "The request body could not be read.", false)
	}
	return nil, false
}

// resolve is §4.4 steps 1–6 with the kind rerank.
func (c *rerankCall) resolve(name string, overlong bool) bool {
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
		c.rec.NoteRejection(keyquota.RejectModel, keyquota.DetailNotFound)
		refuseModelNotFound(c.w, c.r)
		return false
	case a.Kind != modelalias.KindRerank:
		c.rec.NoteRejection(keyquota.RejectModel, keyquota.DetailNotSupported)
		c.refuse(CodeModelNotSupported, rerankKindMessage, false)
		return false
	}
	c.alias = a
	c.rec.Alias = a.Name
	return true
}

// reserveAnswer books the answer before any acquire (design/02 §4.2.1: n ×
// 64 B): rerankResultBytes per document and the encoder buffer.
func (c *rerankCall) reserveAnswer() bool {
	if ReservationFrom(c.ctx) == nil {
		c.internal("inference handler outside the in-flight budget stage (wiring defect)")
		return false
	}
	if !c.ledger.cover(rerankAnswerBooking(len(c.req.docs))) {
		c.overloaded()
		return false
	}
	return true
}

// rerankAnswerBooking is the answer's share for n documents.
func rerankAnswerBooking(n int) int64 {
	return int64(n)*rerankResultBytes + rerankEncodeBuffer
}

// sensitivity is RequiredSensitivity over the request's terms (K30).
func (c *rerankCall) sensitivity() backends.Sensitivity {
	t := SensitivityTerms{Floor: c.d.Federation.Floor(c.ctx), Alias: c.alias, Overlay: c.d.TenantSensitivity(c.ctx)}
	if g, ok := c.d.Aliases.Snapshot().Shadowed(c.alias.Name); ok {
		t.Shadowed = &g
	}
	return RequiredSensitivity(t)
}

// coverScan books the heap of the sensitivity scan of the body before it
// runs (embedCall.coverScan): scanBooking through the ledger for an alias
// below credentials, nothing at credentials (federation.ScanRaise returns at
// once there).
func (c *rerankCall) coverScan(required backends.Sensitivity, body []byte, depth int64) bool {
	if required.Rank() >= backends.SensCredentials.Rank() {
		return true
	}
	return c.ledger.cover(scanBooking(body, depth))
}

// chain builds and gates the chain (§4.4.3 steps 1–5) for the kind rerank.
func (c *rerankCall) chain() ([]backends.Backend, bool) {
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
		c.rec.NoteRejection(keyquota.RejectCapacity, keyquota.DetailEgressDenied)
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return nil, false
	case err != nil:
		c.internal("chain build failed", "error", err)
		return nil, false
	}
	gated, err := c.d.gateChain(c.ctx, c.ca, llmlog.GatewayOpRerank, chain)
	var qe *backends.ErrQuotaExceeded
	switch {
	case errors.As(err, &qe):
		writeRetryAfter(c.w.Header(), tenantQuotaRetryAfter(qe, time.Now()))
		c.rec.NoteRejection(keyquota.RejectQuota, tenantQuotaDetail(qe))
		c.refuse(CodeInsufficientQuota, quotaMessage, false)
		return nil, false
	case err != nil:
		c.internal("tenant quota gate refused the request (wiring defect)", "error", err)
		return nil, false
	case len(gated) == 0:
		c.rec.NoteRejection(keyquota.RejectCapacity, keyquota.DetailEgressDenied)
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return nil, false
	}
	return gated, true
}

// reserveUpstreamTexts books the certain part of the wire calls before the
// acquire (embedCall.reserveUpstreamTexts): every attempt sends every
// document once and the query at least once, so that part is certain at the
// entry, and a request whose wire calls the budget cannot hold is refused
// there — no queue wait, no queue place, no unit.
func (c *rerankCall) reserveUpstreamTexts() bool {
	c.textsBooked = rerankUpstreamTexts(c.req)
	if !c.ledger.cover(c.textsBooked) {
		c.overloaded()
		return false
	}
	return true
}

// admitLimiter is design/06 §4.1 steps 7–9 with op rerank: no limiter ⇒ 503
// service_unavailable with the largest Retry-After step (L11).
func (c *rerankCall) admitLimiter() bool {
	if c.d.Limiter == nil {
		if steps := c.d.RetryAfterSteps(); len(steps) > 0 {
			writeRetryAfter(c.w.Header(), steps[len(steps)-1])
		}
		c.refuse(CodeServiceUnavailable, limiterMissingText, true)
		return false
	}
	res, refusal := c.d.Limiter.Admit(c.ctx, c.ca, c.alias,
		Estimate{Op: llmlog.GatewayOpRerank, PromptBytes: c.req.bytesSum, InputRunes: c.req.runesSum})
	if refusal != nil {
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
	// The row the shutdown's forced settle writes should the request still
	// be open after the HTTP drain (W06-13 (e)).
	registerForcedRow(res, c.d.Record, func() llmlog.GatewayRow {
		return llmlog.GatewayRow{Pipeline: rerankPipeline, ConsumerID: c.ca.ConsumerID,
			Gateway: llmlog.Gateway{Alias: c.alias.Name, Hop: c.rec.Hop, ClientReqID: clientRequestID(c.r), ModelKeyID: c.ca.ModelKeyID}}
	}, string(c.required))
	return true
}

func (c *rerankCall) endLimit(fn func(LimitReservation)) {
	if c.limit != nil && !c.limitEnd {
		c.limitEnd = true
		fn(c.limit)
	}
}

// rerankState is what the attempts of one walk leave behind (embedState
// with the documents' logits in place of the vectors).
type rerankState struct {
	n           int // attempts
	wires       int // wire calls (rerank.Score), over every attempt — the census count
	admitted    time.Time
	lease       *dispatch.Lease
	k           int // blocks of the latest attempt's weight plan
	done        int // its blocks with a result
	usage       dispatch.Usage
	failedBlock int
	abort       string
	failed      []failedAttempt
	last        failedAttempt
	unpaid      []*blockWire
	paid        []bool
}

// markPaid marks the documents [lo, hi) of the request's n as served by a
// block with a usage.
func (st *rerankState) markPaid(lo, hi, n int) {
	if st.paid == nil {
		st.paid = make([]bool, n)
	}
	for i := lo; i < hi; i++ {
		st.paid[i] = true
	}
}

// unpaidEstimate is embedCall.unpaidEstimate over documents: Σ over the
// blocks that met a backend and reported no usage of the meter's estimate of
// their documents no block with a usage served, each document at most once.
func (c *rerankCall) unpaidEstimate(st *rerankState) int {
	sum := 0
	var counted []bool
	for _, bw := range st.unpaid {
		if !bw.contact.Load() {
			continue
		}
		if counted == nil {
			counted = make([]bool, len(c.req.docs))
			copy(counted, st.paid)
		}
		sum = usageTotal(sum, c.restEstimate(bw.lo, bw.hi, counted))
		for i := bw.lo; i < bw.hi; i++ {
			counted[i] = true
		}
	}
	return sum
}

// walk runs the planner and the chain walk, then answers.
func (c *rerankCall) walk(chain []backends.Backend) {
	if ended, _ := c.verdict(); ended {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		return
	}
	if c.d.Planner == nil {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return
	}
	n := len(c.req.docs)
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
	c.rec.Class = planClass(plan) // the class of an L2 refusal (W06-12); a lease names it again
	// k = 1 without the continuation door (design/02 §4.8): more documents
	// than one upstream request carries are refused before any acquire.
	if !plan.Continuation && n > c.lim.subBatch {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		c.refuseParam(unsupportedValue("documents", fmt.Sprintf("at most %d documents per request", c.lim.subBatch)))
		return
	}
	stop := c.d.watch(c.ctx, c.cancel, newWatchTarget(c.ca, c.req.model, c.alias.ID, c.gen))
	defer stop()

	c.logits = make([]float64, n) // booked with the answer (rerankResultBytes)
	st := rerankState{failedBlock: -1}
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
// capped by the tenant ceiling for a tenant alias, by rerank.Timeout (the
// rerank wire's own cap, which rerank.Score applies in any case: "Rerank
// zusätzlich min(·, 180 s)") and by what is left of the request deadline —
// read per block.
func (c *rerankCall) attemptTimeout(b *backends.Backend) time.Duration {
	t := b.TimeoutFor(c.alias.Role, c.s.RequestTimeout)
	if c.alias.Policy.TimeoutS.Set {
		t = time.Duration(c.alias.Policy.TimeoutS.V) * time.Second
	}
	if tenantAlias(c.alias) && c.s.TenantAliasCeiling.Timeout > 0 {
		t = min(t, c.s.TenantAliasCeiling.Timeout)
	}
	return max(min(t, rerank.Timeout, time.Until(c.dl)), time.Millisecond)
}

// attempt is one attempt on row b: the weight plan of the row, then its
// blocks (subBlockRun) — block one under the walker's lease.
func (c *rerankCall) attempt(st *rerankState, plan Plan, runCtx context.Context, b *backends.Backend,
	lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
	st.n++
	st.lease = lease
	if st.admitted.IsZero() {
		st.admitted = time.Now()
	}
	setWriteDeadline(c.w, time.Now().Add(time.Until(c.dl)+writeMargin))
	n := len(c.req.docs)
	blocks := plan.Acquirer.Blocks()
	if err := checkBlocks(blocks, n); err != nil {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.internal("the planner's weight plan does not fit the request (planner defect)", "error", err)
			return CodeInternalError
		})
	}
	if !continuationAllowed(blocks, plan.Continuation) {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.refuseParam(unsupportedValue("documents", fmt.Sprintf("at most %d documents per request", blocks[0])))
			return CodeUnsupportedValue
		})
	}
	// The wire calls of this row's blocks, before the first one (FOLGE-E's
	// upstreamBooking at this route): rerank.Score builds each block's body
	// anew — every attempt's; the calls of an earlier attempt stay on the
	// heap until the collector runs. The first attempt's certain part was
	// booked before the acquire (reserveUpstreamTexts). Refused ⇒ 503
	// server_overloaded after the queue wait, before any wire contact.
	model := b.ModelFor(c.alias.Role).Model
	need := rerankUpstreamBooking(c.req, model, blocks) - c.textsBooked
	c.textsBooked = 0
	if !c.ledger.cover(need) {
		return dispatch.Usage{}, false, c.refuseAfterAcquire(func() string {
			c.overloaded()
			return CodeServerOverloaded
		})
	}
	st.k, st.done, st.failedBlock, st.abort = len(blocks), 0, -1, ""
	run := subBlockRun{ctx: c.ctx, acq: plan.Acquirer, role: c.alias.Role, timeout: c.attemptTimeout, alive: c.alive}
	return run.run(runCtx, b, lease, timeout, blocks,
		func(i, lo, hi int, bl *dispatch.Lease, rc context.Context, t time.Duration) (dispatch.Usage, error) {
			return c.wireBlock(st, b, model, i, lo, hi, bl, rc, t)
		})
}

// refuseAfterAcquire ends the run with the route's own answer (written by
// verdict) and returns the cause the attempt ends with: a cancellation.
func (c *rerankCall) refuseAfterAcquire(answer func() string) error {
	c.own = answer
	c.cancel(errRouteRefused)
	return context.Cause(c.ctx)
}

// alive is the run check before every continuation (embedCall.alive).
func (c *rerankCall) alive() error {
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

// wireBlock is the wire call of block i over the documents [lo, hi), under
// lease (the walker's for block one, its continuation lease for every
// further one — subBlockRun owns both): rerank.Score with the row's score
// domain, the logits in input order.
func (c *rerankCall) wireBlock(st *rerankState, b *backends.Backend, model string, i, lo, hi int, _ *dispatch.Lease,
	rc context.Context, timeout time.Duration) (dispatch.Usage, error) {
	rc = fedhop.WithRequestID(rc, attemptRequestID(c.id, st.wires+1))
	c.syncLifecycle()
	if rc.Err() != nil {
		err := c.blockFailure(st, b, i, rc, fmt.Errorf("oai: the run ended before the wire call: %w", context.Cause(rc)))
		return dispatch.Usage{}, err
	}
	st.wires++
	var conn atomic.Bool
	bw := &blockWire{lo: lo, hi: hi}
	tctx := httptrace.WithClientTrace(rc, wireContactTrace(&c.wired, &conn))
	tctx = httptrace.WithClientTrace(tctx, wireContactTrace(&bw.contact, &conn))
	wctx, cancel := context.WithTimeout(tctx, timeout)
	defer cancel()
	scores, promptTokens, err := rerank.Score(wctx, *b, model, c.req.query, c.req.wire[lo:hi], b.ScoreDomain())
	// A block that ended by its context with a connection in hand may still
	// write: wire contact — the conservative side, as at the chat route.
	if err != nil && conn.Load() && wctx.Err() != nil {
		c.wired.Store(true)
		bw.contact.Store(true)
	}
	if err != nil || promptTokens <= 0 {
		st.unpaid = append(st.unpaid, bw)
	} else {
		st.markPaid(lo, hi, len(c.req.docs))
	}
	if err != nil {
		return dispatch.Usage{}, c.blockFailure(st, b, i, rc, err)
	}
	copy(c.logits[lo:hi], scores)
	st.done++
	st.usage.PromptTokens = usageTotal(st.usage.PromptTokens, promptTokens)
	return dispatch.Usage{PromptTokens: promptTokens}, nil
}

// blockFailure records the failure of block i (embedCall.blockFailure).
func (c *rerankCall) blockFailure(st *rerankState, b *backends.Backend, i int, rc context.Context, err error) error {
	if i > 0 && errors.Is(context.Cause(c.ctx), context.DeadlineExceeded) {
		err = fmt.Errorf("block %d: %w", i, errBlockDeadline)
	}
	st.failedBlock, st.abort = i, llmlog.AbortClass(rc)
	st.last = failedAttempt{b: b, err: err}
	st.failed = append(st.failed, st.last)
	return err
}

// admissionRefused answers an admission failure through the planner's
// writer (embedCall.admissionRefused).
func (c *rerankCall) admissionRefused(err error, waited time.Duration, wired bool, st *rerankState) {
	reason := c.d.Planner.WriteAdmissionError(c.ctx, c.w, err)
	if reason == "" {
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

// endAfterWire ends the reservation of a request that made wire calls — the
// embeddings route's rule over documents (embedCall.endAfterWire): no byte
// reached a backend ⇒ Abort; after wire contact Keep when no block reported
// a usage, else Settle with the reported usage plus the estimate of every
// block that met a backend without one (its documents no block with a usage
// served), Keep when that reaches what the reservation holds (meterEnd).
func (c *rerankCall) endAfterWire(st *rerankState) {
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
func (c *rerankCall) answer(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *rerankState) {
	if st.lease != nil {
		c.rec.QueueWaitMs = st.lease.WaitDur().Milliseconds()
		c.rec.Class = st.lease.Class().String()
	}
	out := c.outcome(plan, attempts, err, st)
	if st.wires > 0 {
		c.rec.PromptTokens = st.usage.PromptTokens
		if st.usage.PromptTokens > 0 {
			c.rec.Usage = llmlog.UsageBackend
		}
		c.endAfterWire(st)
		if forcedEnded(c.limit) {
			return // the shutdown's forced settle wrote the request's row (W06-13)
		}
		c.record(plan, served, attempts, err, st, out)
	}
}

// outcome answers the walk (embedCall.outcome).
func (c *rerankCall) outcome(plan Plan, attempts []llm.ChainAttempt, err error, st *rerankState) embedOutcome {
	if ended, code := c.verdict(); ended {
		if errors.Is(context.Cause(c.ctx), errRouteRefused) {
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
		c.rec.NoteRejection(keyquota.RejectCapacity, keyquota.DetailNotOffered)
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

// stickyStop is the answer of a failed block ≥ 2 (embedCall.stickyStop):
// 503 upstream_unavailable with Retry-After, no failover mixes a second
// scoring build into one ranking.
func (c *rerankCall) stickyStop(st *rerankState) embedOutcome {
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
// itself ended (embedCall.verdict).
func (c *rerankCall) verdict() (ended bool, code string) {
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
	case errors.Is(cause, auth.ErrCredentialRevoked):
		// The door's in-flight registry ended the request: its key stopped
		// authenticating (W01-8). Before the client check — the request's
		// context is the one the registry canceled.
		c.rec.Reason = CodeInvalidAPIKey
		WriteInvalidAPIKey(c.w)
		return true, CodeInvalidAPIKey
	case c.client.Err() != nil:
		return true, ""
	case errors.Is(cause, errRouteRefused) && c.own != nil:
		return true, c.own()
	}
	return false, ""
}

// syncLifecycle puts an ended lifecycle on the run synchronously.
func (c *rerankCall) syncLifecycle() {
	if c.d.Lifecycle != nil && c.d.Lifecycle.Err() != nil {
		c.cancel(errShutdown)
	}
}

// success checks once more before the write (D8) and writes the answer.
func (c *rerankCall) success(st *rerankState) embedOutcome {
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
	returnDocs := c.dialect.returnDocuments()
	if c.req.returnDocsSet {
		returnDocs = c.req.returnDocs
	}
	order := rankDocuments(c.logits, c.req.topN)
	if c.limit != nil {
		for k, v := range c.limit.Headers() {
			c.w.Header()[k] = v
		}
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	a := rerankAnswer{id: "rerank-" + c.id, model: c.alias.Name, scores: c.logits, order: order, promptTokens: st.usage.PromptTokens}
	if returnDocs {
		a.docs = c.req.docs
	}
	if err := writeRerank(c.w, a); err != nil {
		slog.Warn("gateway: failed to write the answer", "error", err, "request_id", c.w.Header().Get("X-Request-ID"))
		return embedOutcome{finish: llmlog.FinishAborted}
	}
	return embedOutcome{finish: llmlog.FinishStop}
}

// sigmoid is the relevance score of a logit, 1/(1+e^(−x)) ∈ [0, 1]
// (design/02 §4.9: Cohere and Jina clients read [0, 1]).
func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

// rankDocuments returns the documents' order — descending by logit, which
// is the order of their relevance scores and finer where the sigmoid
// saturates (past a logit of about 36.7 the score is 1.0 in float64;
// lead review 1 R-5), equal logits in index order (a stable sort of the
// input order) — cut to topN (0: all; more than n: all), and turns the
// logits into relevance scores in place.
func rankDocuments(logits []float64, topN int) []int {
	order := make([]int, len(logits)) // booked with the answer (rerankResultBytes)
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case logits[a] > logits[b]:
			return -1
		case logits[a] < logits[b]:
			return 1
		}
		return 0
	})
	for i, x := range logits {
		logits[i] = sigmoid(x)
	}
	if topN > 0 && topN < len(order) {
		order = order[:topN]
	}
	return order
}

// rerankAnswer is what the answer encodes.
type rerankAnswer struct {
	id, model    string
	scores       []float64 // relevance scores in input order
	order        []int
	docs         []string // the echo; nil without return_documents
	promptTokens int
}

// writeRerank writes the answer (design/02 §4.9 "Antwort"): {id, model,
// results:[{index, relevance_score, document?:{text}}], usage} — the
// results in order, each through the encoder buffer, the echo escaped
// straight from the decoded document (writeJSONText). The whole JSON never
// exists in memory.
func writeRerank(w io.Writer, a rerankAnswer) error {
	bw := bufio.NewWriterSize(w, rerankEncodeBuffer)
	_, _ = bw.WriteString(`{"id":`)
	writeJSONText(bw, a.id)
	_, _ = bw.WriteString(`,"model":`)
	writeJSONText(bw, a.model)
	_, _ = bw.WriteString(`,"results":[`)
	var num [32]byte
	for k, i := range a.order {
		if k > 0 {
			_ = bw.WriteByte(',')
		}
		_, _ = bw.WriteString(`{"index":`)
		_, _ = bw.Write(strconv.AppendInt(num[:0], int64(i), 10))
		_, _ = bw.WriteString(`,"relevance_score":`)
		_, _ = bw.Write(appendScore(num[:0], a.scores[i]))
		if a.docs != nil {
			_, _ = bw.WriteString(`,"document":{"text":`)
			writeJSONText(bw, a.docs[i])
			_ = bw.WriteByte('}')
		}
		_ = bw.WriteByte('}')
	}
	tokens := strconv.Itoa(a.promptTokens)
	_, _ = bw.WriteString(`],"usage":{"prompt_tokens":` + tokens + `,"total_tokens":` + tokens + "}}\n")
	return bw.Flush() // a write error is sticky in the bufio.Writer
}

// appendScore is encoding/json's float64 encoder (floatEncoder, bits 64)
// for a relevance score — finite by construction (sigmoid of a finite
// logit).
func appendScore(b []byte, x float64) []byte {
	format := byte('f')
	if abs := math.Abs(x); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, x, format, -1, 64)
	if format == 'e' {
		// clean up e-09 to e-9
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

// writeJSONText writes s as a JSON string the way encoding/json writes a
// valid UTF-8 string without HTML escaping (s is decoded text: invalid bytes
// became U+FFFD in the parse) — runs of plain bytes as they are, a quote and
// a backslash escaped, \b \f \n \r \t as two bytes, every other control
// character and U+2028/U+2029 as \u escapes. It allocates nothing.
func writeJSONText(w *bufio.Writer, s string) {
	const hex = "0123456789abcdef"
	_ = w.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			if strings.HasPrefix(s[i:], "\u2028") || strings.HasPrefix(s[i:], "\u2029") {
				_, _ = w.WriteString(s[start:i])
				_, _ = w.WriteString(`\u202`)
				_ = w.WriteByte(hex[s[i+2]&0xf])
				i += len("\u2028")
				start = i
				continue
			}
			i++
			continue
		}
		if c >= ' ' && c != '"' && c != '\\' {
			i++
			continue
		}
		_, _ = w.WriteString(s[start:i])
		switch c {
		case '"', '\\':
			_ = w.WriteByte('\\')
			_ = w.WriteByte(c)
		case '\b':
			_, _ = w.WriteString(`\b`)
		case '\f':
			_, _ = w.WriteString(`\f`)
		case '\n':
			_, _ = w.WriteString(`\n`)
		case '\r':
			_, _ = w.WriteString(`\r`)
		case '\t':
			_, _ = w.WriteString(`\t`)
		default:
			_, _ = w.WriteString(`\u00`)
			_ = w.WriteByte(hex[c>>4])
			_ = w.WriteByte(hex[c&0xf])
		}
		i++
		start = i
	}
	_, _ = w.WriteString(s[start:])
	_ = w.WriteByte('"')
}

// record writes the request's ONE llmlog row (design/06 §4.6): the walk's
// telemetry, every wire attempt in metadata.chain, metadata.sub_batches and
// sub_batches_done, the tokens of every block with a result, a RedactedErr
// — never the walk's raw error (B20).
func (c *rerankCall) record(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *rerankState, out embedOutcome) {
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
		// The row's text of the query and the documents is sized, then
		// booked, then written (the embeddings route's rule).
		if n := rerankLogSize(c.req); c.ledger.cover(int64(n)) {
			request = rerankLogText(c.req, n)
		}
	}
	c.d.Record(llmlog.GatewayRow{
		Pipeline:   rerankPipeline,
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

// rerankLogSize is the length of rerankLogText: the query's text, a line
// break and the documents' text (bodyLogSize of each raw value).
func rerankLogSize(req *rerankRequest) int {
	return bodyLogSize(req.queryRaw) + 1 + bodyLogSize(req.docsRaw)
}

// rerankLogText is the log row's text of the request, written once into
// size bytes (bodyLogText's form over two values).
func rerankLogText(req *rerankRequest, size int) string {
	var b strings.Builder
	b.Grow(size)
	bodyLogWalk(req.queryRaw, func(p []byte) { b.Write(p) })
	b.WriteByte('\n')
	bodyLogWalk(req.docsRaw, func(p []byte) { b.Write(p) })
	return b.String()
}
