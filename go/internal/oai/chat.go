package oai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
)

// POST /v1/chat/completions, non-stream (wave W02-07; design/02 §4.5, §4.6,
// §4.10–§4.12; design/06 §4.1 steps 4–12). The order of one request:
//
//	invariants (admit, inference dependencies) → write deadline at entry →
//	request deadline on the request context → body → top-level parse →
//	Resolve → kind → policy parse (messages, vision, caps) → effective
//	max_tokens → answer reserve → RequiredSensitivity + scan → chain build
//	(Pool.Chain, gateway wire, federation, export) → GateTenant → limiter →
//	planner → watcher → WalkChain(BuildUpstreamBody, ChatRaw) → check before
//	the write → answer rewrite → one llmlog row, one access record.
//
// Memory (FOLGE-P): what the request allocates in proportion to its body —
// the body, a rewritten message array, every attempt's upstream body — is
// booked against the request share of its reservation (cover) before it is
// allocated; past that share the reservation grows, and a budget that cannot
// hold it is the 503 server_overloaded of §4.10.2.
//
// Nothing is written before the planner's First acquire returned (design/03
// §4.9 step 6). Every refusal names its code as the access record's reason.

// writeMargin is the margin of every write deadline over the request's own
// frame (design/02 §4.12: "Eintritt + MaxWait + request_deadline + 30 s",
// "jetzt + Restfrist + 30 s"): the time an answer that is ready at the end of
// the frame has to reach the socket. A derived mechanism constant, not a
// policy (the policy values are MaxWait and gateway.request_deadline).
const writeMargin = 30 * time.Second

// chatPipeline is the llmlog pipeline of the route's row — a local name for
// the llmlog constant, the shape the promptguard census resolves
// (constPipelineSites, one alias level).
const chatPipeline = llmlog.PipelineGatewayChat

// chatUnits is the engine work of one chat request for the weight plan: one
// choice (n > 1 is RC-F, E4).
const chatUnits = 1

// The texts of the route's own refusals.
const (
	noEligibleMessage    = "No backend is currently eligible to serve this model."
	limiterMissingText   = "The service is temporarily unavailable; retry shortly."
	quotaMessage         = "The tenant's request quota is exhausted."
	modelKindMessage     = "The model does not support chat completions."
	shutdownMessage      = "The server is shutting down; retry."
	internalErrorMessage = "The server could not complete the request."
)

// ChatCompletions is the handler of POST /v1/chat/completions (non-stream).
func ChatCompletions(d Deps) http.Handler {
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
		c := newChatCall(d, w, r, ca, entry)
		defer c.close()
		c.serve()
	})
}

// chatCall is one request's state.
type chatCall struct {
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
	req      *chatRequest
	required backends.Sensitivity
	limit    LimitReservation
	limitEnd bool
	// wired: a byte of the request can have reached a backend in some
	// attempt (wireTrace) — set from inside net/http, on its transport
	// goroutines. Without it the reservation goes back in full (Abort);
	// after it, never (Keep without a usage).
	wired atomic.Bool

	// held is the request share of the reservation (the stage's
	// requestReserveFactor × body, grown by cover), spent what the request
	// has allocated against it (cover).
	held, spent int64
}

func newChatCall(d Deps, w http.ResponseWriter, r *http.Request, ca *auth.ConsumerAuth, entry time.Time) *chatCall {
	s := d.Settings()
	c := &chatCall{d: d, w: w, s: s, ca: ca, entry: entry, client: r.Context(), rec: AccessRecordFrom(r.Context())}
	if c.rec == nil {
		c.rec = &AccessRecord{} // outside a model group: filled, never read
	}
	c.rec.Op, c.rec.Stream = llmlog.GatewayOpChat, false
	c.rec.Hop = len(fedhop.InboundFrom(r.Context()).Hop)
	// The write deadline at entry (§4.12, B23): the server's WriteTimeout
	// counts from the end of the header read, and an answer written after
	// it — a 429 after the queue wait — would reach the client as a broken
	// connection. Entry + the longest wait + the whole request frame.
	setWriteDeadline(w, entry.Add(d.MaxWait()+s.RequestDeadline+writeMargin))
	// The request deadline lives on the request context BEFORE the planner
	// (W02-07 (k), lead decision 5): A03-W8 reads Request.ServeBy from it.
	c.dl = entry.Add(s.RequestDeadline)
	ctx, cancelDl := context.WithDeadline(r.Context(), c.dl)
	ctx, cancel := context.WithCancelCause(ctx)
	c.ctx, c.cancel = ctx, cancel
	c.stops = append(c.stops, func() bool { cancelDl(); return true })
	if d.Lifecycle != nil {
		c.stops = append(c.stops, context.AfterFunc(d.Lifecycle, func() { cancel(errShutdown) }))
		// AfterFunc runs f in its own goroutine even when the lifecycle has
		// already ended: a request that enters during the shutdown would
		// otherwise race that goroutine through plan, acquire and wire
		// (Fremd-Review 2 F2-1). Synchronously, so the run is ended before
		// serve looks at it.
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
// wire contact (a panic inside ChatRaw once the request was written, or in
// the answer path) by keeping its charge, the usage is unknown; without it
// by Abort.
func (c *chatCall) close() {
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

// refuse answers code and names it as the access record's reason.
func (c *chatCall) refuse(code, message string, shouldRetry bool) {
	c.rec.Reason = code
	WriteCode(c.w, code, message, shouldRetry)
}

func (c *chatCall) internal(msg string, args ...any) {
	slog.Error("gateway: "+msg, append(args, "path", c.r.URL.Path, "request_id", c.w.Header().Get("X-Request-ID"))...)
	c.rec.Reason = CodeInternalError
	WriteInternal(c.w)
}

func (c *chatCall) serve() {
	// A run that ended before the request did anything (the lifecycle was
	// already done at entry) is answered at once: no parse, no grant read
	// on a canceled context, no meter reservation (H-1).
	if ended, _ := c.verdict(); ended {
		return
	}
	body, ok := c.readBody()
	if !ok {
		return
	}
	if !c.cover(jsonStateBytes(body)) { // the JSON check's nesting state, booked before it runs
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return
	}
	req, perr := parseTopLevel(body)
	if perr != nil {
		c.refuseParam(perr)
		return
	}
	if !c.resolve(req.model) {
		return
	}
	if perr := req.parsePolicy(limitsOf(c.alias, c.s), c.cover); perr != nil {
		c.refuseParam(perr)
		return
	}
	c.req = req
	if len(req.ignored) > 0 {
		c.w.Header().Set("X-Ctx-Ignored-Params", strings.Join(req.ignored, ", "))
	}
	maxTokens := effectiveMaxTokens(req.maxTokens, c.alias, c.s)
	if maxTokens <= 0 {
		c.internal("effective max_tokens is not positive (settings defect)", "max_tokens", maxTokens)
		return
	}
	if !c.reserveAnswer() {
		return
	}
	required := c.sensitivity()
	if !c.coverScan(required, body) {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return
	}
	c.required = c.d.Federation.Scan(required, body)
	chain, ok := c.chain()
	if !ok {
		return
	}
	if !c.admitLimiter(maxTokens) {
		return
	}
	c.walk(chain, llm.UpstreamSpec{
		Client: req.pass, Messages: req.messages, Role: c.alias.Role, Stream: false, MaxTokens: maxTokens,
		ReasoningOff: reasoningOff(c.alias), Reserve: c.reserveUpstream,
	})
}

// reasoningOff is the alias's reasoning policy (design/02 §3.2: default off).
func reasoningOff(a modelalias.Alias) bool {
	return !a.Policy.Reasoning.Set || a.Policy.Reasoning.V != modelalias.ReasoningPassthrough
}

// readBody reads the body under the strict cap and the in-flight budget: a
// chunked body past the budget is the budget's 503, never a 400 (§4.2.1).
// The ledger starts here: the stage reserved requestReserveFactor × the
// body, the read spent what readRequestBody allocated.
func (c *chatCall) readBody() ([]byte, bool) {
	body, spent, err := readRequestBody(c.r.Body, c.r.ContentLength)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		reserved := int64(len(body))
		if cl := c.r.ContentLength; cl > 0 {
			reserved = cl
		}
		c.held, c.spent = requestReserveFactor*reserved, 0
		if !c.cover(spent) { // a small undeclared body's first chunk can pass its share
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

// readRequestBody reads the body with what it costs on the heap: a declared
// length is read into one buffer of that size (io.ReadAll grows its way
// there and spends about twice the body), an undeclared one in chunks that
// double up to readChunkMax and are joined into one buffer at the end.
func readRequestBody(r io.Reader, declared int64) ([]byte, int64, error) {
	if declared > 0 {
		buf := make([]byte, declared)
		n, err := io.ReadFull(r, buf)
		if err == nil {
			var probe [1]byte
			if k, _ := r.Read(probe[:]); k > 0 { // more than declared: never from net/http
				rest, spent, err := readRequestBody(io.MultiReader(bytes.NewReader(probe[:k]), r), -1)
				return append(buf, rest...), declared + spent + int64(len(buf)+len(rest)), err
			}
			return buf, declared, nil
		}
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return buf[:n], declared, err
	}
	var chunks [][]byte
	var spent int64
	total, size := 0, readChunkMin
	for {
		chunk := make([]byte, size)
		spent += int64(size)
		n := 0
		var err error
		for n < size && err == nil {
			var k int
			k, err = r.Read(chunk[n:])
			n += k
		}
		total += n
		chunks = append(chunks, chunk[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, spent, err
		}
		size = min(2*size, readChunkMax)
	}
	if len(chunks) == 1 {
		return chunks[0], spent, nil
	}
	body := make([]byte, 0, total)
	for _, ch := range chunks {
		body = append(body, ch...)
	}
	return body, spent + int64(total), nil
}

// The chunk sizes of an undeclared body: the first chunk holds a small
// request whole, the largest bounds what is allocated ahead of the bytes the
// budget stage reserves while they are read.
const (
	readChunkMin = 512
	readChunkMax = 1 << 20
)

// cover books n more bytes against the request share before they are
// allocated (FOLGE-P) and grows the reservation by what the bookings pass
// the share; false when the budget cannot hold them — nothing is reserved
// then, and the caller answers 503 server_overloaded.
func (c *chatCall) cover(n int64) bool {
	if c.spent+n <= c.held {
		c.spent += n
		return true
	}
	res := ReservationFrom(c.ctx)
	if res == nil || !res.Grow(c.spent+n-c.held) {
		return false
	}
	c.spent += n
	c.held = c.spent
	return true
}

// coverScan books the heap of the sensitivity scan of the body before it
// runs (FOLGE-P hardening H-8): federation.ScanRaise decodes every token of
// the body into a string and runs the scanner's patterns over each — up to
// scanReserveFactor × the body + scanReserveBase, what one scan call holds at
// once. One form is scanned twice in a row — a key of the whole body with a
// scalar value, as a key and in the probe "key: value" (Delta-Review 2 F-1):
// it allocates about two bookings in total, but never more than one at once
// (the first call's garbage is dead before the second begins; pinned by
// TestScanTwiceHoldsOneScanAtOnce, see scanReserveFactor). At credentials the
// scan returns at once (its short circuit, pinned) and nothing is booked. The
// booking stays until the request ends: the scan's garbage is heap until the
// collector runs, and a reservation has no partial release.
func (c *chatCall) coverScan(required backends.Sensitivity, body []byte) bool {
	if required.Rank() >= backends.SensCredentials.Rank() {
		return true
	}
	return c.cover(scanReserveFactor*int64(len(body)) + scanReserveBase)
}

// reserveUpstream is UpstreamSpec.Reserve: an attempt's upstream body is
// booked before it is built — every attempt's, the earlier ones stay on the
// heap until the collector runs. A refusal ends the walk as an admission
// outcome (no health report for the row) and is answered 503 (outcome).
func (c *chatCall) reserveUpstream(n int) error {
	if !c.cover(int64(n)) {
		return ErrBudgetExhausted
	}
	return nil
}

// refuseParam answers a parse refusal with its param (the envelope's
// "param" names the field, §4.10.1), bound to the vocabulary like WriteCode.
// errParseOverloaded is the in-flight budget's 503.
func (c *chatCall) refuseParam(perr *paramError) {
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

// resolve is §4.4 steps 1–6: the alias in the consumer's scope, released by
// all_models or a grant (the one 404 for every reason), then the kind.
func (c *chatCall) resolve(name string) bool {
	// The generation the watcher starts from is the one this Resolve saw at
	// most: read BEFORE it, a change between the Resolve and the watcher
	// start (limiter, plan, the queue) moves the generation past it and the
	// first tick resolves again (Copilot review on PR #144). Read after, the
	// watcher would start from a generation that already holds the change
	// and skip every Resolve until the check before the write.
	c.gen = c.d.Aliases.Generation()
	a, found, err := c.d.Resolve(c.ctx, c.ca, name)
	switch {
	case err != nil:
		// A grant read the shutdown or the watcher ended is their answer
		// (503 with x-should-retry true, resp. 404), not a read fault.
		if ended, _ := c.verdict(); ended {
			return false
		}
		grantReadFailed(c.w, c.r, err)
		return false
	case !found:
		refuseModelNotFound(c.w, c.r)
		return false
	case a.Kind != modelalias.KindChat:
		c.refuse(CodeModelNotSupported, modelKindMessage, false)
		return false
	}
	c.alias = a
	c.rec.Alias = a.Name
	return true
}

// reserveAnswer takes the answer's share of the in-flight budget (§4.2.1):
// the upstream body and the rewritten one exist at the same time.
func (c *chatCall) reserveAnswer() bool {
	res := ReservationFrom(c.ctx)
	if res == nil {
		c.internal("inference handler outside the in-flight budget stage (wiring defect)")
		return false
	}
	if !res.Grow(2 * c.s.MaxResponseBytes) {
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		return false
	}
	return true
}

// sensitivity is RequiredSensitivity over the request's terms (K30).
func (c *chatCall) sensitivity() backends.Sensitivity {
	t := SensitivityTerms{Floor: c.d.Federation.Floor(c.ctx), Alias: c.alias, Overlay: c.d.TenantSensitivity(c.ctx)}
	if g, ok := c.d.Aliases.Snapshot().Shadowed(c.alias.Name); ok {
		t.Shadowed = &g
	}
	return RequiredSensitivity(t)
}

// chain builds and gates the chain (§4.4.3 steps 1–5).
func (c *chatCall) chain() ([]backends.Backend, bool) {
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

// untilNextHour is the Retry-After of the tenant quota (design/02 §4.10.2:
// "bis zur nächsten vollen Stunde"); at least one second.
func untilNextHour(now time.Time) time.Duration {
	return max(now.Truncate(time.Hour).Add(time.Hour).Sub(now), time.Second)
}

// admitLimiter is design/06 §4.1 steps 7–9. No limiter ⇒ 503
// service_unavailable with the largest Retry-After step (L11, lead decision
// O5 (b)): never served without it.
func (c *chatCall) admitLimiter(maxTokens int) bool {
	if c.d.Limiter == nil {
		if steps := c.d.RetryAfterSteps(); len(steps) > 0 {
			writeRetryAfter(c.w.Header(), steps[len(steps)-1])
		}
		c.refuse(CodeServiceUnavailable, limiterMissingText, true)
		return false
	}
	res, refusal := c.d.Limiter.Admit(c.ctx, c.ca, c.alias, Estimate{
		Op: llmlog.GatewayOpChat, MaxTokens: maxTokens, ClientMaxTokens: c.req.maxTokens > 0,
		PromptBytes: len(c.req.messages), PromptRunes: utf8.RuneCount(c.req.messages), ServiceTier: c.req.serviceTier,
	})
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
	return true
}

func (c *chatCall) endLimit(fn func(LimitReservation)) {
	if c.limit != nil && !c.limitEnd {
		c.limitEnd = true
		fn(c.limit)
	}
}

// walkState is what the attempts of one walk leave behind.
type walkState struct {
	n        int
	wired    int // attempts that called the wire (ChatRaw)
	admitted time.Time
	result   *llm.RawChatResult
	failed   []failedAttempt
	last     failedAttempt
	lease    *dispatch.Lease
}

// walk runs the planner and the chain walk, then answers.
func (c *chatCall) walk(chain []backends.Backend, spec llm.UpstreamSpec) {
	// A run ended while the request was parsed and its chain built never
	// reaches the planner, an acquire or the wire; the reservation the
	// limiter just took ends with Abort (no wire call, design/06 §4.2).
	if ended, _ := c.verdict(); ended {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		return
	}
	if c.d.Planner == nil {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return
	}
	plan, err := c.d.Planner.Plan(c.ctx, c.ca, AdmissionInput{Chain: chain, ServiceTier: c.req.serviceTier, Units: chatUnits})
	if err != nil {
		// A run the route itself ended (shutdown, watcher) is answered
		// first: the writer stays silent on a canceled request context,
		// which would leave an empty 200 (Fremd-Review 1 F-1).
		if ended, code := c.verdict(); ended {
			c.endLimit(func(l LimitReservation) { l.Reject(code, 0) })
			return
		}
		c.admissionRefused(err, 0, false)
		return
	}
	// A run ended while the planner built the plan never reaches an acquire
	// (Fremd-Review 4 R4-1: a shutdown here used to book an attempt on the
	// ended run — no wire contact, but a row and Settle).
	if ended, _ := c.verdict(); ended {
		c.endLimit(func(l LimitReservation) { l.Abort() })
		return
	}
	stop := c.d.watch(c.ctx, c.cancel, watchTarget{ca: c.ca, name: c.req.model, aliasID: c.alias.ID, gen: c.gen})
	defer stop()

	var st walkState
	rules := llm.WalkRules{WindowRule: true, AllowedClasses: c.d.Planner.AllowedClasses(), Timeout: c.attemptTimeout}
	served, attempts, werr := llm.WalkChain(c.ctx, plan.Chain, c.alias.Role, plan.Acquirer, llm.PoolReporter(c.d.Pool), rules,
		func(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
			return c.attempt(&st, spec, runCtx, b, lease, timeout)
		})
	stop()
	c.answer(plan, served, attempts, werr, &st)
}

// attemptTimeout is one attempt's wire deadline (§4.12): the alias's
// timeout_s, else the row's role timeout over gateway.request_timeout,
// capped by the tenant ceiling for a tenant alias and by what is left of the
// request deadline (non-stream: every attempt lives inside it).
func (c *chatCall) attemptTimeout(b *backends.Backend) time.Duration {
	t := b.TimeoutFor(c.alias.Role, c.s.RequestTimeout)
	if c.alias.Policy.TimeoutS.Set {
		t = time.Duration(c.alias.Policy.TimeoutS.V) * time.Second
	}
	if tenantAlias(c.alias) && c.s.TenantAliasCeiling.Timeout > 0 {
		t = min(t, c.s.TenantAliasCeiling.Timeout)
	}
	// ≥ 1 ms: a non-positive value would make the walker fall back to the
	// house chat timeout; an expired deadline ends the acquire anyway.
	return max(min(t, time.Until(c.dl)), time.Millisecond)
}

// attempt is one wire attempt under its lease: the body for this row, the
// server-side request id of the attempt, the one non-stream wire site.
func (c *chatCall) attempt(st *walkState, spec llm.UpstreamSpec, runCtx context.Context, b *backends.Backend,
	lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
	st.n++
	st.lease = lease
	if st.admitted.IsZero() {
		st.admitted = time.Now()
	}
	// Admitted: the write deadline follows the rest of the request frame
	// (§4.12 "bei jeder Admission jetzt + Restfrist + 30 s").
	setWriteDeadline(c.w, time.Now().Add(time.Until(c.dl)+writeMargin))
	body, err := llm.BuildUpstreamBody(spec, b)
	if errors.Is(err, ErrBudgetExhausted) {
		err = &llm.AdmissionError{Err: err, Backend: b.Name, Host: b.Host}
	}
	if err != nil {
		st.last = failedAttempt{b: b, err: err}
		st.failed = append(st.failed, st.last)
		return dispatch.Usage{}, false, err
	}
	// <oaiReqID>-a<n>: hex and "-", ≤ 64 — the grammar fedhop and the
	// house middleware check (FR2-4); sent to ctx rows only (K28, tier F).
	runCtx = fedhop.WithRequestID(runCtx, attemptRequestID(c.id, st.n))
	// A run that ended while the lease was acquired does not dial: the
	// attempt fails with the run's cause (a cancellation, no health report)
	// and, without any wire call, the request books no row and aborts its
	// reservation (answer; Fremd-Review 4 R4-1, the window inside the walk).
	c.syncLifecycle()
	if runCtx.Err() != nil {
		err := fmt.Errorf("oai: the run ended before the wire call: %w", context.Cause(runCtx))
		st.last = failedAttempt{b: b, err: err}
		st.failed = append(st.failed, st.last)
		return dispatch.Usage{}, false, err
	}
	// st.wired counts the wire calls for the census (Σ acquire = Σ wire);
	// c.wired is the meter's wire contact, marked from inside net/http by
	// the attempt's trace (W06-10 hardening 3, H3-1). The trace rides on the
	// request context, and req.Clone carries it into httpx.DoRetryOnce's
	// retry: every try of the attempt reports.
	st.wired++
	// The attempt's deadline (ChatRaw sets the same one inside) is held here
	// too, so its end is read off the context, not off the error.
	var conn atomic.Bool
	actx, cancel := context.WithTimeout(httptrace.WithClientTrace(runCtx, c.wireTrace(&conn)), timeout)
	res, err := llm.ChatRaw(actx, *b, body, timeout, c.s.MaxResponseBytes)
	ended := actx.Err() != nil
	cancel()
	// An attempt that ended by its context with a connection in hand may
	// still write: HTTP/2 returns on the context without waiting for the
	// stream's writer. Counted as wire contact — the conservative side.
	if err != nil && conn.Load() && ended {
		c.wired.Store(true)
	}
	if err != nil {
		st.last = failedAttempt{b: b, err: err}
		st.failed = append(st.failed, st.last)
		return dispatch.Usage{}, false, err
	}
	st.result = res
	return dispatch.Usage{PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens}, false, nil
}

// attemptRequestID is the server-side id of attempt n of request id
// (design/02 §4.5.3: "<oaiReqID>-a<n>", fresh per attempt): the UUIDv7 is
// hex and "-", "a" and the digits are hex too, so the id stays inside the
// grammar fedhop.validRequestID (internal/fedhop/grammar.go) and the house
// RequestID middleware (internal/handler/middleware.go isValidRequestID)
// share: [0-9A-Fa-f-]{1,64} (F-W3 review FR2-4). 36 + 2 + digits ≤ 64.
func attemptRequestID(id string, n int) string { return fmt.Sprintf("%s-a%d", id, n) }

// wireTrace marks wire contact where a byte of the request can leave for a
// backend, in any try, instead of reading it off the error of the last try
// (Fremd-Review 4 F4-1, F4-2, F4-7). conn records a connection in hand.
//
//   - WroteHeaderField, WroteHeaders, WroteRequest (any Err — a partly
//     written body reached the backend): the request is being written on an
//     established connection. HTTP/1.1 writes every request byte inside
//     Request.write, which calls WroteHeaderField after the request line
//     and the Host line, and WroteRequest when it returns, failed writes
//     included; HTTP/2 calls WroteHeaderField while it encodes the HEADERS
//     frame, before any of it is written. A dial, a refused or timed-out
//     connect and a failed TLS handshake reach none of them. Before an error
//     returns, HTTP/1.1 waits for its write loop, HTTP/2 for the stream's
//     writer unless the context ended (attempt), so the mark is visible when
//     ChatRaw returns.
//   - GotFirstResponseByte: the backend answered — wire contact by
//     definition; the hook runs before net/http hands the response over.
func (c *chatCall) wireTrace(conn *atomic.Bool) *httptrace.ClientTrace {
	mark := func() { c.wired.Store(true) }
	return &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { conn.Store(true) },
		WroteHeaderField:     func(string, []string) { mark() },
		WroteHeaders:         mark,
		WroteRequest:         func(httptrace.WroteRequestInfo) { mark() },
		GotFirstResponseByte: mark,
	}
}

// admissionRefused answers an admission failure through the planner's
// writer — only these (A03-W8 handover: plan errors, acquire errors, the
// class assert); waited is the time the request queued.
func (c *chatCall) admissionRefused(err error, waited time.Duration, wired bool) {
	reason := c.d.Planner.WriteAdmissionError(c.ctx, c.w, err)
	if reason == "" {
		// The writer wrote nothing: it saw the run context canceled. The
		// route answers its own cause (shutdown 503, watcher 404) — the
		// cause can land after the route's own verdict() and before the
		// writer's context check (Fremd-Review 2 F2-1). Only a gone client
		// stays unanswered; anything else is a defect and gets the 503.
		if ended, code := c.verdict(); ended {
			reason = code
		} else {
			c.internal("the admission writer wrote nothing on a live request", "error", err)
			reason = CodeInternalError
		}
	}
	c.rec.Reason = reason
	// After a wire call (a failover acquire refused behind a row that
	// answered) the reservation keeps its charge: the first row did the
	// work (W06-10 hardening 2, H2-3; the ledger reason is W06-12's).
	c.endLimit(func(l LimitReservation) {
		if wired {
			l.Keep()
			return
		}
		l.Reject(reason, waited)
	})
}

// isAdmissionFailure is the writer's domain (A03-W8 §Übergaben): an acquire
// error of the walk and the walk's class assert.
func isAdmissionFailure(err error) bool {
	return llm.IsAdmissionError(err) || errors.Is(err, llm.ErrWalkClass)
}

// answer writes the outcome of the walk and its one llmlog row.
func (c *chatCall) answer(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *walkState) {
	var usage dispatch.Usage
	if st.result != nil {
		usage = dispatch.Usage{PromptTokens: st.result.PromptTokens, CompletionTokens: st.result.CompletionTokens}
	}
	if st.lease != nil {
		c.rec.QueueWaitMs = st.lease.WaitDur().Milliseconds()
		c.rec.Class = st.lease.Class().String()
	}
	out := c.outcome(plan, attempts, err, st)
	// A row and Settle only for a request that reached the wire, or whose
	// walk failed on its own: a run that ended before any wire call
	// (aborted, no ChatRaw) is no LLM call — close() aborts its reservation.
	// After wire contact without a usage (an upstream error, an answer
	// without usage, the client gone during the call) the reservation keeps
	// its charge: never a full refund after the wire (W06-10 hardening H-7).
	// Wire calls of which no byte reached a backend (wireTrace) give it back:
	// Abort (H2-4, H3-1). The counters are tested one by one — their int sum
	// can overflow (H2-5).
	if len(attempts) > 0 && (st.wired > 0 || out.finish != llmlog.FinishAborted) {
		wired := c.wired.Load()
		c.endLimit(func(l LimitReservation) {
			switch {
			case st.wired > 0 && !wired:
				l.Abort()
			case wired && usage.PromptTokens <= 0 && usage.CompletionTokens <= 0:
				l.Keep()
			default:
				l.Settle(usage)
			}
		})
		c.record(plan, served, attempts, err, st, out)
	}
}

// chatOutcome is what the client got, for the row.
type chatOutcome struct {
	finish   string
	redacted *llmlog.RedactedErr
	response string
}

// outcome answers the walk: the watcher's verdict and a gone client first,
// then success (after the check before the write), the admission writer's
// domain, and the route's own attempt mapping.
func (c *chatCall) outcome(plan Plan, attempts []llm.ChainAttempt, err error, st *walkState) chatOutcome {
	if ended, code := c.verdict(); ended {
		return chatOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
	}
	if err == nil {
		return c.success(plan, st)
	}
	if errors.Is(err, ErrBudgetExhausted) {
		// The budget refused an attempt's upstream body (reserveUpstream):
		// the stage's 503. Without a wire call no row, and the meter's
		// reservation goes back in full (close: Abort); on a failover attempt
		// after a wire call the earlier attempt's row stands and the
		// reservation settles or keeps its charge (answer) — never a full
		// refund after the wire (W06-10).
		c.rec.Reason = CodeServerOverloaded
		WriteOverloaded(c.w)
		finish := llmlog.FinishError
		if st.wired == 0 {
			finish = llmlog.FinishAborted
		}
		return chatOutcome{finish: finish, redacted: &llmlog.RedactedErr{Class: "admission", Code: CodeServerOverloaded}}
	}
	if isAdmissionFailure(err) {
		waited := time.Duration(0)
		if st.lease == nil {
			waited = time.Since(c.entry)
		}
		c.admissionRefused(err, waited, c.wired.Load())
		return chatOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Class: "admission", Code: c.rec.Reason}}
	}
	if len(attempts) == 0 {
		// The walk ended without one wire attempt and without an admission
		// failure: no row of the chain serves the role (no model for it, or
		// every row window-skipped) — nothing eligible, no upstream fault.
		slog.Warn("gateway: the walk found no row to try", "alias", c.alias.Name, "role", c.alias.Role,
			"error", err, "request_id", c.w.Header().Get("X-Request-ID"))
		c.refuse(CodeNoEligibleBackend, noEligibleMessage, false)
		return chatOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Code: CodeNoEligibleBackend}}
	}
	lastClass := attempts[len(attempts)-1].Class
	if st.last.err == nil {
		st.last.err = err
	}
	a := mapAttemptFailure(st.last, st.failed, lastClass, answerContext{
		alias: c.alias, chain: plan.Chain, relay4xx: c.s.RelayUpstream4xxMessages, snapshot: c.d.Pool.Snapshot,
	})
	writeRetryAfter(c.w.Header(), a.retryAfter)
	c.refuse(a.code, a.message, a.shouldRetry)
	return chatOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Class: a.class, Status: a.status, Code: a.code}}
}

// verdict answers a run the watcher, the shutdown or the client ended:
// ended reports one of them, code is what was written ("" when the client
// is gone and nothing could be).
func (c *chatCall) verdict() (ended bool, code string) {
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
		// The client is gone: nothing reaches it (design/02 §4.10.2 "Client
		// weg"); the row says aborted.
		return true, ""
	}
	return false, ""
}

// syncLifecycle puts an ended lifecycle on the run synchronously: AfterFunc
// delivers its cause from a goroutine of its own, so a shutdown that landed
// after the entry need not be on the run yet when the route asks
// (Fremd-Review 3 R3-1). A cause already set (client gone, watcher) stays
// the cause.
func (c *chatCall) syncLifecycle() {
	if c.d.Lifecycle != nil && c.d.Lifecycle.Err() != nil {
		c.cancel(errShutdown)
	}
}

// success checks once more before the write (D8), rewrites and writes.
func (c *chatCall) success(plan Plan, st *walkState) chatOutcome {
	t := watchTarget{ca: c.ca, name: c.req.model, aliasID: c.alias.ID, gen: c.gen}
	switch err := c.d.stillServes(c.ctx, t); {
	case errors.Is(err, errGatewayOff), errors.Is(err, errModelGone):
		c.cancel(err)
		_, code := c.verdict()
		return chatOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
	case err != nil:
		if ended, code := c.verdict(); ended {
			return chatOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
		}
		grantReadFailed(c.w, c.r, err)
		return chatOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Code: CodeInternalError}}
	}
	body, err := rewriteChat(st.result.Body, "chatcmpl-"+c.id, st.admitted.Unix(), c.alias.Name, plan.Lane, reasoningOff(c.alias))
	if err != nil {
		slog.Warn("gateway: upstream answer could not be rewritten", "error", err, "request_id", c.w.Header().Get("X-Request-ID"))
		c.refuse(CodeUpstreamError, upstreamErrorMessage, false)
		return chatOutcome{finish: llmlog.FinishError, redacted: &llmlog.RedactedErr{Class: "server_fault", Code: CodeUpstreamError}}
	}
	c.rec.PromptTokens, c.rec.CompletionTokens = st.result.PromptTokens, st.result.CompletionTokens
	if usageTotal(st.result.PromptTokens, st.result.CompletionTokens) > 0 {
		c.rec.Usage = llmlog.UsageBackend
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
	if _, err := c.w.Write(append(body, '\n')); err != nil {
		slog.Warn("gateway: failed to write the answer", "error", err, "request_id", c.w.Header().Get("X-Request-ID"))
	}
	return chatOutcome{finish: finishOf(st.result.FinishReason), response: string(body)}
}

// finishOf maps an upstream finish_reason into the row vocabulary.
func finishOf(f string) string {
	switch f {
	case llmlog.FinishStop, llmlog.FinishLength, llmlog.FinishToolCalls, llmlog.FinishContentFilter:
		return f
	}
	return ""
}

// usageTotal is prompt + completion as the meter counts them (keyquota
// Settle): a negative counter counts 0, the sum saturates at the int range
// — an int sum of two reported counters can overflow and drop the usage
// marker of the access record and the row (Fremd-Review 4 F4-6).
func usageTotal(prompt, completion int) int {
	p, q := max(prompt, 0), max(completion, 0)
	if p > math.MaxInt-q {
		return math.MaxInt
	}
	return p + q
}

// record writes the request's ONE llmlog row (design/06 §4.6): the walk's
// telemetry, every wire attempt in metadata.chain, a RedactedErr — never the
// walk's raw error, which can carry the upstream body (B20).
func (c *chatCall) record(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *walkState, out chatOutcome) {
	tel := llmlog.Entry{RequiredSensitivity: string(c.required), Metadata: map[string]any{}}
	if st.result != nil {
		tel.PromptTokens, tel.CompletionTokens = st.result.PromptTokens, st.result.CompletionTokens
		tel.CostUSD = st.result.CostUSD
	}
	llm.StampServed(&tel, c.alias.Role, served)
	var class dispatch.Class
	if plan.Acquirer != nil {
		class = plan.Acquirer.Class()
	}
	llm.ApplyDispatchOutcome(&tel, attempts, err, class)
	usage := ""
	if st.result != nil && usageTotal(st.result.PromptTokens, st.result.CompletionTokens) > 0 {
		usage = llmlog.UsageBackend
	}
	request := ""
	if c.s.BodyLogging == llmlog.BodyLoggingFull || (c.s.BodyLogging == llmlog.BodyLoggingErrors && out.redacted != nil) {
		request = string(c.req.messages)
	}
	c.d.Record(llmlog.GatewayRow{
		Pipeline:   chatPipeline,
		ConsumerID: c.ca.ConsumerID,
		Gateway: llmlog.Gateway{
			Alias: c.alias.Name, Hop: c.rec.Hop, Usage: usage, ClientReqID: clientRequestID(c.r),
			Finish: out.finish, ModelKeyID: c.ca.ModelKeyID,
		},
		Telemetry:   tel,
		Attempts:    attempts,
		Err:         out.redacted,
		Request:     request,
		Response:    out.response,
		BodyLogging: c.s.BodyLogging,
	})
}

// clientRequestID is the client's own request id (X-Request-ID, else
// X-Client-Request-Id): stored in metadata.gw.client_req_id only, never
// sent upstream and never the answer's id (B16).
func clientRequestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	return r.Header.Get("X-Client-Request-Id")
}

// setWriteDeadline moves the connection's write deadline (a writer without
// deadline support — a test recorder — ignores it).
func setWriteDeadline(w http.ResponseWriter, t time.Time) {
	_ = http.NewResponseController(w).SetWriteDeadline(t)
}
