package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

// The /v1 admission contract (wave A03-W8; design/03 §4.4, §4.7, §4.9,
// design/02 §4.11): V1Admission is the planner every /v1 inference handler
// runs before its walk (oai.AdmissionPlanner). Per request it
//
//  1. resolves the lane (ResolveLane): urgent ⇒ the external class XU, bulk
//     ⇒ the external bulk class XB (A03-W9; until then bulk was 503
//     model_unavailable without an acquire, masterplan §3.2 duty 6);
//  2. filters the chain to the rows whose origin offers the external class
//     (ExternalAdmissible, read with b.Host per row); none ⇒ 503
//     model_unavailable without an acquire;
//  3. hands out the request's acquirer (llm.Acquirer): First is the ONE
//     blocking wait point — llm.Admission.AcquireWith with the request's
//     wait budget (MaxWait from Ctx-Max-Wait-Ms, only shortening; ServeBy
//     from the request context's deadline, masterplan K8, no new header)
//     and the weight of the first block; Next (the failover attempt) is the
//     non-blocking TryAcquire; Continue serves the further sub-blocks of the
//     attempt's row and exists only after a lease of the same request. The
//     weight of every block comes from ExternalWeightPlan with the clamp
//     min(sub_batch, ExternalWeightCap(b.Host)) (design/03 §4.7);
//  4. answers every failure of the plan or of the walk's acquires through
//     WriteAdmissionError(r.Context(), w, err) — before any byte: the
//     handler gets a lease or an error from First, and only then writes
//     (design/03 §4.9 step 6).
//
// The handler is no WithScheduler mount (design/03 §4.8a: a /v1 request
// never raises the herald), the planner writes no llmlog row (an external
// refusal has none, design/03 §3.4) and holds no limiter, ledger or access
// log — WriteAdmissionError returns the code it wrote as the refusal reason
// for those (W06-9, W06-12). Every origin argument it passes on is a chain
// row's b.Host, never a spelling built from the request (A03-W17 review 2
// F-5: the dispatcher caches each raw spelling).

// The lanes of a model consumer (context_model_consumers.admission_lane,
// design/03 §4.4): ordered urgent ≻ bulk.
const (
	LaneUrgent = "urgent"
	LaneBulk   = "bulk"
)

// serviceTierFlex is the OpenAI service_tier that asks for the cheap,
// patient lane: it lowers the request to bulk (design/03 §4.4 table).
const serviceTierFlex = "flex"

// The planner's own refusals.
var (
	// ErrLaneNotOffered refuses a request whose lane has no admission class
	// (laneClass) — 503 model_unavailable, fail-closed, before any acquire.
	// Unreachable since A03-W9 (ResolveLane yields urgent or bulk, both
	// bound); it stays the fail-closed default of a lane outside the table.
	// The value is oai's (W05-04): the chain dry run tells the refusals
	// apart without importing this package.
	ErrLaneNotOffered = oai.ErrLaneNotOffered
	// ErrChainNotOffered refuses a request none of whose chain rows offers
	// the external class — 503 model_unavailable before any acquire.
	ErrChainNotOffered = oai.ErrChainNotOffered
	// ErrAcquireOrder is a door of the request acquirer used out of order —
	// a second First (a second wait point), Next or Continue before a lease
	// of the request, Continue on another row than the attempt's or past
	// its block plan. A planner or walker bug: 503 internal_error.
	ErrAcquireOrder = errors.New("handler: admission door used out of order")
)

// The texts of the admission answers: generic, nothing of the target, the
// queue or the request in them (B6).
const (
	busyMessage        = "The server is busy; retry shortly."
	unavailableMessage = "The model is temporarily unavailable; retry later."
)

// The Retry-After headers of a /v1 refusal: delta-seconds, and the
// millisecond form both official SDKs read first.
const (
	headerRetryAfter   = "Retry-After"
	headerRetryAfterMs = "retry-after-ms"
)

// V1Dispatch is what the planner reads from the dispatcher: the two doors,
// the chain filter, the weight clamp and the resolved Retry-After steps.
// *dispatch.Dispatcher is one; the census (v1census.Recorder) wraps one.
type V1Dispatch interface {
	dispatch.Admitter
	dispatch.TryAdmitter
	ExternalAdmissible(origin string) bool
	ExternalWeightCap(origin string) int
	ExternalRetryAfterSteps() []time.Duration
}

var _ V1Dispatch = (*dispatch.Dispatcher)(nil)

// V1AdmissionConfig builds a V1Admission.
type V1AdmissionConfig struct {
	// Dispatch is the process dispatcher (required).
	Dispatch V1Dispatch
	// Settings is the gateway settings getter (required): the jitter of the
	// Retry-After headers, gateway.retry_after_jitter, read per refusal.
	Settings func() oai.Settings
	// Jitter draws from U[0; 1); nil ⇒ math/rand/v2 (a test seam).
	Jitter func() float64
	// Logger takes the ERROR of a code-bug answer; nil ⇒ slog.Default().
	Logger *slog.Logger
}

// V1Admission is the planner of the /v1 admission contract.
type V1Admission struct {
	dispatch V1Dispatch
	settings func() oai.Settings
	jitter   func() float64
	logger   *slog.Logger
}

var _ oai.AdmissionPlanner = (*V1Admission)(nil)

// NewV1Admission returns the planner; a missing dispatcher or settings
// getter is a wiring bug and panics at construction.
func NewV1Admission(cfg V1AdmissionConfig) *V1Admission {
	if cfg.Dispatch == nil || cfg.Settings == nil {
		panic("handler.NewV1Admission: the dispatcher and the gateway settings getter are required")
	}
	j := cfg.Jitter
	if j == nil {
		j = rand.Float64 //nolint:gosec // retry jitter, not security material
	}
	return &V1Admission{dispatch: cfg.Dispatch, settings: cfg.Settings, jitter: j, logger: cfg.Logger}
}

func (a *V1Admission) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

// AllowedClasses is the ONE source of WalkRules.AllowedClasses of the /v1
// routes: the external classes the planner binds — XU (lane urgent) and XB
// (lane bulk, A03-W9; Index §4). No route writes a class list of its own.
func (a *V1Admission) AllowedClasses() []dispatch.Class {
	return []dispatch.Class{dispatch.ClassExternal, dispatch.ClassExternalBulk}
}

// laneClass is the admission class of a lane (design/03 §4.4): urgent ⇒ XU,
// bulk ⇒ XB (A03-W9). Any other value has none — the planner refuses it
// fail-closed (ErrLaneNotOffered).
func laneClass(lane string) (dispatch.Class, bool) {
	switch lane {
	case LaneUrgent:
		return dispatch.ClassExternal, true
	case LaneBulk:
		return dispatch.ClassExternalBulk, true
	}
	return 0, false
}

// ResolveLane is the lane of a request (design/03 §4.4, design/04 §4.9
// I-H1…I-H3): the minimum of the consumer's ceiling (admission_lane — an
// unknown or empty value counts as bulk), the peer hint and service_tier.
// The hint counts only for a ctx-peer consumer with Ctx-Class set AND a
// Ctx-Path (an app's Ctx-Class is ignored — PeerIngress already clears it,
// F-W9, and this resolve does not trust that alone); a peer without a hint
// is bulk. service_tier flex lowers to bulk. No input raises the ceiling.
func ResolveLane(ca *auth.ConsumerAuth, in fedhop.Inbound, serviceTier string) string {
	if ca == nil || ca.AdmissionLane != LaneUrgent {
		return LaneBulk
	}
	if ca.Kind == auth.KindCtxPeer && !peerHintUrgent(in) {
		return LaneBulk
	}
	if serviceTier == serviceTierFlex {
		return LaneBulk
	}
	return LaneUrgent
}

// peerHintUrgent reports whether a peer's Ctx-Class hint is an urgent class
// — ranked by the federation grammar's own class table (fedhop.MergeClass
// keeps the urgent external class only against an urgent hint: interactive
// and external read urgent, background, external_bulk and anything unknown
// bulk). No class or no Ctx-Path is no hint.
func peerHintUrgent(in fedhop.Inbound) bool {
	if in.Class == "" || len(in.Path) == 0 {
		return false
	}
	urgent := dispatch.ClassExternal.String()
	return fedhop.MergeClass(urgent, in.Class) == urgent
}

// RequestLane is the lane of the request in ctx: ResolveLane over the
// request's inbound federation state, whose Ctx-Class counts only when
// federation.PeerHint confirms it — a ctx-peer the PeerIngress proved
// against its binding (F-W12, design/04 §4.9). An unproved peer (a stage
// injected past the PeerIngress, a router without it) keeps its ceiling
// rule without the hint: bulk. The planner and the meter both take the
// lane from here, so the slot and the bucket can never disagree.
func RequestLane(ctx context.Context, ca *auth.ConsumerAuth, serviceTier string) string {
	in := fedhop.InboundFrom(ctx)
	if _, ok := federation.PeerHint(ctx); !ok {
		in.Class = ""
	}
	return ResolveLane(ca, in, serviceTier)
}

// Plan implements oai.AdmissionPlanner: lane, chain filter, acquirer — the
// acquirer binds the lane's class (urgent ⇒ XU, bulk ⇒ XB). A lane without
// a class and an empty filtered chain are refusals before any acquire.
func (a *V1Admission) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	lane := RequestLane(ctx, ca, in.ServiceTier)
	class, ok := laneClass(lane)
	if !ok {
		return oai.Plan{Lane: lane}, ErrLaneNotOffered
	}
	chain := make([]backends.Backend, 0, len(in.Chain))
	for i := range in.Chain {
		if a.dispatch.ExternalAdmissible(in.Chain[i].Host) {
			chain = append(chain, in.Chain[i])
		}
	}
	if len(chain) == 0 {
		return oai.Plan{Lane: lane}, ErrChainNotOffered
	}
	acq := &v1Acquirer{
		adm:      llm.Admission{Admitter: a.dispatch, Class: class},
		capOf:    a.dispatch.ExternalWeightCap,
		units:    in.Units,
		subBatch: in.SubBatch,
		stream:   in.Stream,
	}
	// Continue is llm.Admission.Continue — the dispatcher's continuation
	// door (A03-W17): a sub-block route may walk every block of the plan.
	return oai.Plan{Acquirer: acq, Chain: chain, Lane: lane, Continuation: true}, nil
}

// ExternalWeightPlan is the weight plan of one request on one target
// (design/03 §4.7): units (0 reads as one) split into blocks of
// min(subBatch, capacity) units — subBatch ≤ 0 is no own cap, capacity ≤ 0
// (the target does not offer: the acquire answers not-offered) no clamp —
// the last block carrying the rest. Block i is the weight of lease i.
// A pure function: W02-09/W02-10 split their inputs by it.
func ExternalWeightPlan(units, subBatch, capacity int) []int {
	units = max(units, 1)
	size := units
	if subBatch > 0 {
		size = min(size, subBatch)
	}
	if capacity > 0 {
		size = min(size, capacity)
	}
	plan := make([]int, 0, (units+size-1)/size)
	for left := units; left > 0; left -= size {
		plan = append(plan, min(size, left))
	}
	return plan
}

// waitOptions are the wait budget of the First acquire (masterplan K8,
// design/03 §4.9): ServeBy = the request context's deadline, when it has
// one; MaxWait = the Ctx-Max-Wait-Ms budget of the inbound state (read
// through Inbound.Wait, charged with the time since the ingress parsed it),
// only shortening — the dispatcher takes min(setting, MaxWait). An invalid
// header never got here (the grammar ignores it, X14). A budget of zero —
// a sent 0, "do not wait at all", or one already spent — cannot travel as
// MaxWait (0 is "not set" there), so it sets ServeBy to now: the
// dispatcher's own serve-by term is then ≤ 0, and an acquire that would
// have to wait is refused at once as predicted, with the Retry-After of its
// own position; an eligible one is still admitted.
func waitOptions(ctx context.Context) llm.AdmitOptions {
	var o llm.AdmitOptions
	if dl, ok := ctx.Deadline(); ok {
		o.ServeBy = dl
	}
	in := fedhop.InboundFrom(ctx)
	wait, has := in.Wait()
	if !has {
		return o
	}
	now := time.Now()
	if !in.ReceivedAt.IsZero() {
		wait -= now.Sub(in.ReceivedAt)
	}
	if wait > 0 {
		o.MaxWait = wait
		return o
	}
	if o.ServeBy.IsZero() || now.Before(o.ServeBy) {
		o.ServeBy = now
	}
	return o
}

// v1Acquirer is the acquirer of one request (llm.Acquirer plus Blocks): the
// door state that makes Continue a door of an admitted request only.
type v1Acquirer struct {
	adm             llm.Admission
	capOf           func(origin string) int
	units, subBatch int
	// stream is AdmissionInput.Stream: every door passes it on
	// (llm.AdmitOptions.Stream, A03-W10 — the settle field of the lease).
	stream bool

	mu      sync.Mutex
	started bool   // First was called
	leased  bool   // a First or Next lease was handed out
	host    string // the row of the current attempt
	blocks  []int  // its weight plan
	next    int    // the next block a Continue serves
}

// Class is the lane's admission class.
func (q *v1Acquirer) Class() dispatch.Class { return q.adm.Class }

// First is the request's one blocking acquire, on the first chain row,
// with the wait budget of the request and the weight of block one.
func (q *v1Acquirer) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	q.mu.Lock()
	again := q.started
	q.started = true
	q.mu.Unlock()
	if again {
		return nil, nil, orderError(b, "a second first acquire (a second wait point)")
	}
	blocks := ExternalWeightPlan(q.units, q.subBatch, q.capOf(b.Host))
	opts := waitOptions(ctx)
	opts.Weight, opts.Stream = blocks[0], q.stream
	lease, runCtx, err := q.adm.AcquireWith(ctx, b, role, timeout, opts)
	if err == nil {
		q.attempt(b.Host, blocks)
	}
	return lease, runCtx, err
}

// Next is the failover acquire on a later row: never blocking (TryAcquire,
// design/02 §4.11.1 L5), only after a lease of the request.
func (q *v1Acquirer) Next(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	q.mu.Lock()
	leased := q.leased
	q.mu.Unlock()
	if !leased {
		return nil, nil, orderError(b, "a failover acquire before the first lease")
	}
	blocks := ExternalWeightPlan(q.units, q.subBatch, q.capOf(b.Host))
	lease, runCtx, err := q.adm.TryAcquire(ctx, b, role, timeout, llm.AdmitOptions{Weight: blocks[0], Stream: q.stream})
	if err == nil {
		q.attempt(b.Host, blocks)
	}
	return lease, runCtx, err
}

// Continue is the next sub-block of the current attempt (masterplan K21):
// only after a lease of the request, on the attempt's row, within its block
// plan — with that block's weight.
func (q *v1Acquirer) Continue(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	q.mu.Lock()
	ok := q.leased && b.Host == q.host && q.next < len(q.blocks)
	var weight int
	if ok {
		weight = q.blocks[q.next]
		q.next++
	}
	q.mu.Unlock()
	if !ok {
		return nil, nil, orderError(b, "a continuation without a lease of the request on its row, or past its block plan")
	}
	return q.adm.Continue(ctx, b, role, timeout, llm.AdmitOptions{Weight: weight, Stream: q.stream})
}

// orderError is the refusal of a door used out of order, inside the acquire
// hull like every other door failure (dispatch.AdmissionError): a Continue
// is called from inside the route's attempt, where no acquireFor wraps it,
// and the route's error split and the walker read the hull (PR #142 bot
// review 4477069015).
func orderError(b *backends.Backend, what string) error {
	return &llm.AdmissionError{Err: fmt.Errorf("%w: %s", ErrAcquireOrder, what), Backend: b.Name, Host: b.Host}
}

// Blocks is the weight plan of the current attempt's row (nil before a
// lease): block i carries Blocks()[i] units.
func (q *v1Acquirer) Blocks() []int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.blocks)
}

// attempt records the row and the weight plan of a granted First or Next.
func (q *v1Acquirer) attempt(host string, blocks []int) {
	q.mu.Lock()
	q.leased, q.host, q.blocks, q.next = true, host, blocks, 1
	q.mu.Unlock()
}

// WriteAdmissionError implements oai.AdmissionPlanner (design/03 §4.9
// error contract, masterplan K6, K15, K29) and returns the code it wrote.
// Its domain is the ADMISSION failure: an error of Plan, an acquire error of
// the walk (inside the dispatch.AdmissionError hull every acquire failure
// comes back in, llm.IsAdmissionError) and the walk's class assert
// (llm.ErrWalkClass) — never an attempt error (an upstream status, a wire
// timeout: the route's own mapping, W02-07, K29):
//
//   - a dispatcher rejection (IsRejection) or an external would-block ⇒ 429
//     server_busy, x-should-retry true; Retry-After and retry-after-ms from
//     the RetryAfter of the RejectionError — never from the class-wide
//     RetryAfterHint (A03-W6) — times one draw u ∈ U[1; 1 + j], j =
//     gateway.retry_after_jitter: Retry-After = ⌈RA·u⌉ s, retry-after-ms =
//     round(RA·u) ms. A rejection without a RejectionError (an admitter
//     that is no dispatcher) gets no header: no fabricated value;
//   - ErrExternalNotOffered, a lane without a class (fail-closed, A03-W9:
//     unreachable) and an empty filtered chain ⇒ 503 model_unavailable,
//     x-should-retry true, Retry-After = the largest step of
//     dispatch.external_retry_after_steps;
//   - everything else — the code-bug sentinels ErrClassUnauthorized,
//     ErrPrincipalConflict, ErrInvalidClass, llm.ErrWalkClass,
//     ErrAcquireOrder, llm.ErrNoTryAdmitter, and any error outside the
//     domain above (a route that hands in an attempt error) ⇒ 503
//     internal_error, x-should-retry false, one ERROR line naming which of
//     the two it was — never an empty answer;
//   - a context error inside the acquire hull while the client still
//     waits — the route's own deadline fired during an acquire (a Continue
//     has no wait budget), or an acquire on a context the request did not
//     end (the run context of a lease the route already released) ⇒ the
//     same 503 internal_error, x-should-retry false (an A-internal fault,
//     K5/K29), with a WARN for the route deadline and an ERROR for the
//     foreign context — never an empty 200 (A03-W8 review 2 F-1);
//   - the request's own context was CANCELED (ctx = r.Context()): the client
//     is gone — a client can only cancel the server's request context
//     (connection closed, stream reset), nothing reaches it ⇒ nothing is
//     written, "" returned. A request context that ended by its deadline
//     (gateway.request_deadline on r.Context(), W02-07 (k), or any other
//     deadline) is a live client and gets the answer of its error
//     (A03-W8 review 3 FR3-1).
//
// Bodies are generic: no origin, no number, no row name (B6).
func (a *V1Admission) WriteAdmissionError(ctx context.Context, w http.ResponseWriter, err error) string {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return ""
	}
	acquire := llm.IsAdmissionError(err)
	busy := dispatch.IsRejection(err) || errors.Is(err, dispatch.ErrWouldBlock)
	var re *dispatch.RejectionError
	switch {
	case busy && errors.As(err, &re):
		noteAdmissionRefusal(ctx, busyDetail(err), err)
		a.setRetryAfter(w.Header(), re.RetryAfter)
		oai.WriteCode(w, oai.CodeServerBusy, busyMessage, true)
		return oai.CodeServerBusy
	case busy:
		noteAdmissionRefusal(ctx, busyDetail(err), err)
		oai.WriteCode(w, oai.CodeServerBusy, busyMessage, true)
		return oai.CodeServerBusy
	case errors.Is(err, dispatch.ErrExternalNotOffered), errors.Is(err, ErrLaneNotOffered), errors.Is(err, ErrChainNotOffered):
		noteAdmissionRefusal(ctx, keyquota.DetailNotOffered, err)
		if steps := a.dispatch.ExternalRetryAfterSteps(); len(steps) > 0 {
			w.Header().Set(headerRetryAfter, strconv.Itoa(int(math.Ceil(steps[len(steps)-1].Seconds()))))
		}
		oai.WriteCode(w, oai.CodeModelUnavailable, unavailableMessage, true)
		return oai.CodeModelUnavailable
	}
	a.logInternal(err, acquire)
	oai.WriteInternal(w)
	return oai.CodeInternalError
}

// noteAdmissionRefusal declares an L2 refusal on the request's access
// record for the refusal ledger (W06-12, design/06 §4.1 step 10): reason
// capacity, detail the refusing rule, target the dispatcher target of the
// refused acquire (dispatch.AdmissionError.Host; "" for a refusal of the
// plan, before any acquire). The route hands the queue wait over with the
// meter reservation's Reject (oai.AccessRecord.NoteRejectionWait). An
// internal_error is no refusal by a rule and declares nothing.
func noteAdmissionRefusal(ctx context.Context, detail string, err error) {
	if ctx == nil {
		return
	}
	var ae *dispatch.AdmissionError
	target := ""
	if errors.As(err, &ae) {
		target = ae.Host
	}
	oai.NoteRejectionAt(ctx, keyquota.RejectCapacity, detail, target)
}

// busyDetail is the ledger detail of a busy answer: wait_expired when the
// external wait budget ran out or was predicted out (dispatch.ErrWaitExpired
// — the two are one sentinel; the dispatcher's own counters keep the split),
// server_busy for the cap ladder, a full queue and a busy failover row.
func busyDetail(err error) string {
	if errors.Is(err, dispatch.ErrWaitExpired) {
		return keyquota.DetailWaitExpired
	}
	return keyquota.DetailServerBusy
}

// logInternal names what an internal_error answer of the writer stands for:
// the route's deadline during an acquire (WARN — capacity under a deadline
// of the route, not a code bug), an acquire on a foreign context, a planner
// or wiring defect, or an error outside the writer's domain (each ERROR).
func (a *V1Admission) logInternal(err error, acquire bool) {
	switch {
	case acquire && errors.Is(err, context.DeadlineExceeded):
		a.log().Warn("gateway: an admission ended with a deadline of the route while the client waited — answered as internal_error",
			"error", err)
	case acquire && errors.Is(err, context.Canceled):
		a.log().Error("gateway: an admission ended on a context the request did not end (the run context of a released lease?) — answered as internal_error",
			"error", err)
	case acquire || errors.Is(err, llm.ErrWalkClass):
		a.log().Error("gateway: admission failed on a path that only a planner or wiring defect reaches — answered as internal_error",
			"error", err)
	default:
		a.log().Error("gateway: the admission writer got an error that is no admission failure (the route must map attempt errors itself) — answered as internal_error",
			"error", err)
	}
}

// setRetryAfter sets both Retry-After headers of a saturation answer from
// one jitter draw (K6: the estimate is the minimum, the jitter only adds).
func (a *V1Admission) setRetryAfter(h http.Header, ra time.Duration) {
	if ra <= 0 {
		return
	}
	j := a.settings().RetryAfterJitter
	if !(j > 0) { // NaN or negative: no jitter
		j = 0
	}
	u := 1 + j*a.jitter()
	h.Set(headerRetryAfter, strconv.Itoa(int(math.Ceil(ra.Seconds()*u))))
	h.Set(headerRetryAfterMs, strconv.Itoa(int(math.Round(float64(ra)/float64(time.Millisecond)*u))))
}
