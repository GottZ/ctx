package federation

import (
	"container/list"
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// PeerIngressConfig wires one PeerIngress.
type PeerIngressConfig struct {
	// Consumer reads the model consumer the door (handler.ModelAuth) put
	// into the request context — handler.ConsumerAuthFromContext in ctxd,
	// injected because this package does not import handler. Required.
	Consumer func(context.Context) *auth.ConsumerAuth
	// Logger takes the ERROR of step 5 and the WARN of step 6; nil is
	// slog.Default().
	Logger *slog.Logger

	// The peer stage (design/04 §4.6 steps 6–8, §4.8; wave F-W12).
	//
	// Binder writes the trust-on-first-use binding of a ctx-peer
	// (auth.PeerBindings). Required: a missing binder is a wiring bug and
	// panics at construction, like a missing Consumer.
	Binder PeerBinder
	// RequirePeerHop is the federation.require_peer_hop getter (hot,
	// global-only): true ⇒ a ctx-peer request without Ctx-Path is refused
	// with 400 federation_hop_required; false (and nil) ⇒ served without
	// hints, counted and warned as before.
	RequirePeerHop func() bool
	// Events receives EventPeerBound for every binding the stage created;
	// nil notes nothing.
	Events EventNoter
}

// PeerIngress is the second federation stage of the model groups (design/04
// §4.6, steps 5, 6, 9, 10; F-W9), fixed behind the LoopGuard and before the
// body cap (masterplan K14: EnabledGate → ModelAuth → LoopGuard →
// PeerIngress → body cap/parse). The LoopGuard has refused every unreadable
// chain, loop and over-long chain and handed on Ctx-Hop/Ctx-Path only; this
// stage reads the full inbound state again and decides what of it a stage
// behind may see:
//
//  5. no valid ConsumerAuth in the context ⇒ 503 internal_error with
//     x-should-retry: false and an ERROR line — a wiring defect, never a
//     pass (the door puts one on every request it lets through);
//  6. a ctx-peer without Ctx-Path (a peer before F-W6, or one that
//     registered this instance as generic) ⇒ served without hints: Class
//     and Pipeline dropped, counter federation_peer_without_path +1 on
//     every request, one WARN per consumer (a bounded set); and Ctx-Egress:
//     local — such a sender stamps no Tier F, so its egress intent is
//     unknown, and an unknown falls to the restrictive side (design/04 §5.5;
//     local is also the default of every ctx row, E-04-4; Fremd-Review
//     FR-2). With federation.require_peer_hop on it is refused instead:
//     400 federation_hop_required, Ctx-Reason: hop_required, ledger
//     federation/hop_required (counted and warned all the same);
//  7. a ctx-peer with Ctx-Path is bound to its last entry (the direct
//     sender, F-W12, design/04 §4.8): a binding the door's check returned
//     (ConsumerAuth.PeerInstanceID) is compared without a statement; an
//     unbound peer is bound by Binder — trust on first use, one statement,
//     peer_bound noted once when this request wrote it. The binding in
//     force ≠ the sender (a copied key, a lost first-contact race, a
//     vanished consumer) ⇒ 403 peer_instance_mismatch, Ctx-Reason:
//     peer_mismatch, ledger federation/peer_mismatch; a failing statement ⇒
//     503 auth_unavailable (K5). A proved peer is marked
//     (fedhop.WithPeerInstance): only then does its class count as a hint
//     (PeerHint) and its pipeline reach the request's row;
//  8. a ctx-peer that is served (proved, or pathless under step 6) gets
//     Ctx-Instance: <this instance's id> on the answer — never an app,
//     never a refusal;
//  9. an app (kind ≠ ctx-peer) ⇒ Class and Pipeline dropped: an app's
//     Ctx-Class never reaches the lane resolve (P1);
//  10. WithPolicy(ResolvePolicy(ca, in)) and fedhop.WithInbound(in), the
//     cleaned full Inbound replacing the LoopGuard's reduced one.
//
// A peer with Ctx-Path keeps Class and Pipeline for the lane resolve
// (A03-W8: hint only with kind ctx-peer ∧ Class ≠ "" ∧ len(Path) > 0).
// Ctx-Sensitivity, Ctx-Egress and Ctx-Max-Wait-Ms stay for every consumer:
// they only narrow the request itself (read the budget through
// Inbound.Wait).
//
// No context.WithTimeout: Rev.-0 step 9 (the request context capped at a
// header deadline) is gone — A's queue time must not eat B's generation
// (V1 on A, Finding 15); Ctx-Max-Wait-Ms caps A's wait only, in the
// admission.
//
// Every refusal of this stage answers before the body cap, the meter and
// any chain: it costs no quota and no statement beyond step 7's one.
//
// A header that does not parse here means the LoopGuard is not in front
// (it refuses every parse error first): the same 503 as step 5. The cost
// per request is one ParseInbound, for a pathless peer one lock on the
// WARN set, for an unbound peer's first request one statement. A missing
// Consumer accessor or Binder is a wiring bug and panics at construction.
func PeerIngress(cfg PeerIngressConfig) func(http.Handler) http.Handler {
	if cfg.Consumer == nil {
		panic("federation.PeerIngress: nil consumer accessor")
	}
	if cfg.Binder == nil {
		panic("federation.PeerIngress: nil peer binder")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	ps := &peerStage{binder: cfg.Binder, events: cfg.Events, require: cfg.RequirePeerHop, log: log}
	if ps.require == nil {
		ps.require = func() bool { return false }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			ca := cfg.Consumer(ctx)
			if ca == nil || !ca.IsValid {
				log.ErrorContext(ctx, "federation: model request without a valid consumer behind the door — refused (wiring defect)", "path", r.URL.Path)
				oai.WriteInternal(w)
				return
			}
			in, err := fedhop.ParseInbound(r.Header)
			if err != nil {
				log.ErrorContext(ctx, "federation: unreadable federation header behind the PeerIngress — the LoopGuard is not in front (wiring defect)", "path", r.URL.Path)
				oai.WriteInternal(w)
				return
			}
			if ca.Kind != auth.KindCtxPeer {
				in.Class, in.Pipeline = "", ""
			} else {
				var ok bool
				if ctx, ok = ps.serve(ctx, w, ca, &in); !ok {
					return
				}
			}
			ctx = WithPolicy(ctx, ResolvePolicy(ca, in))
			next.ServeHTTP(w, r.WithContext(fedhop.WithInbound(ctx, in)))
		})
	}
}

// peerWithoutPathWarnSetSize bounds the set behind "at most one WARN per
// consumer and minute" (step 6): a mechanism bound on memory, not a policy
// value (A-E2). At the target scale (dozens of consumers per instance, ≥ 10
// instances) every peer consumer fits; beyond it the least recently seen
// consumer falls out and may warn once more — the log stays bounded either
// way.
const peerWithoutPathWarnSetSize = 1024

// peerWithoutPathWarnEvery is the WARN throttle of step 6: at most one line
// per consumer and minute, so a peer that stays misconfigured for weeks is
// still visible in the log without flooding it. Hardcoded on purpose, like
// the door's log throttle: it bounds the process's log about itself, not
// anybody's policy; the counter carries the volume.
const peerWithoutPathWarnEvery = time.Minute

var (
	peerWithoutPath atomic.Uint64
	peerWarned      = newWarnOnce(peerWithoutPathWarnSetSize, peerWithoutPathWarnEvery, time.Now)
	// bindErrorWarn throttles the ERROR of a failing bind statement to one
	// line per peerWithoutPathWarnEvery for the process: a database outage
	// under load must not become one line per request.
	bindErrorWarn atomic.Int64
)

// The fixed texts of the peer stage's refusals: they never name an
// instance, a chain or a binding (design/04 §4.6, oracle doctrine).
const (
	msgHopRequired  = "This consumer is a ctx peer and must send its federation path (Ctx-Path)."
	msgPeerMismatch = "This model key is bound to another ctx instance."
	msgBindFailed   = "The peer binding could not be checked; try again later."
)

// peerStage is steps 6–8 for a ctx-peer consumer (design/04 §4.6, §4.8).
type peerStage struct {
	binder  PeerBinder
	events  EventNoter
	require func() bool
	log     *slog.Logger
}

// serve runs steps 6–8 for the ctx-peer ca and cleans in. It returns the
// request context (marked when the peer proved its binding) and false
// when it has written a refusal.
func (ps *peerStage) serve(ctx context.Context, w http.ResponseWriter, ca *auth.ConsumerAuth, in *fedhop.Inbound) (context.Context, bool) {
	if len(in.Path) == 0 {
		in.Class, in.Pipeline = "", ""
		in.EgressLocal = true
		peerWithoutPath.Add(1)
		refuse := ps.require()
		if peerWarned.first(ca.ConsumerID) {
			ps.log.WarnContext(ctx, "federation: ctx-peer request without Ctx-Path — the peer predates F-W6 or registered this instance as generic; served without class or pipeline hints unless federation.require_peer_hop refuses it (at most one line per consumer and minute)",
				"consumer_id", ca.ConsumerID, "refused", refuse)
		}
		if refuse {
			refusePeer(ctx, w, oai.CodeFederationHopRequired, msgHopRequired, fedhop.ReasonHopRequired, keyquota.DetailHopRequired)
			return ctx, false
		}
		setInstance(ctx, w)
		return ctx, true
	}
	last := in.Path[len(in.Path)-1]
	bound := ca.PeerInstanceID
	if bound == "" {
		b, err := ps.binder.BindPeer(ctx, ca.ConsumerID, last)
		if err != nil {
			ps.bindFailed(ctx, ca, err)
			refusePeer(ctx, w, oai.CodeAuthUnavailable, msgBindFailed, "", "")
			return ctx, false
		}
		bound = b.Instance
		if b.Created && ps.events != nil {
			ps.events.Note(ctx, EventPeerBound, ca.ConsumerID)
		}
	}
	if bound != last {
		refusePeer(ctx, w, oai.CodePeerInstanceMismatch, msgPeerMismatch, fedhop.ReasonPeerMismatch, keyquota.DetailPeerMismatch)
		return ctx, false
	}
	ctx = fedhop.WithPeerInstance(ctx, last)
	setInstance(ctx, w)
	return ctx, true
}

// bindFailed logs a failing bind statement at ERROR, throttled for the
// process (bindErrorWarn).
func (ps *peerStage) bindFailed(ctx context.Context, ca *auth.ConsumerAuth, err error) {
	now := time.Now().UnixNano()
	last := bindErrorWarn.Load()
	if now-last < int64(peerWithoutPathWarnEvery) || !bindErrorWarn.CompareAndSwap(last, now) {
		return
	}
	ps.log.ErrorContext(ctx, "federation: peer binding statement failed — the peer request is refused with 503 auth_unavailable (throttled to one line per minute)",
		"consumer_id", ca.ConsumerID, "error", err)
}

// setInstance answers a served ctx-peer with this instance's id (step 8).
// A context without identity (a test router) sets nothing.
func setInstance(ctx context.Context, w http.ResponseWriter) {
	if id := fedhop.IdentityFrom(ctx).ID; id != "" {
		w.Header().Set(fedhop.HeaderInstance, id)
	}
}

// refusePeer writes one refusal of the peer stage: the code's status and
// type from the vocabulary, x-should-retry: false (the same request is
// refused again), Ctx-Reason when there is one, the code as the access
// record's reason and — for a rule's refusal (detail ≠ "") — the ledger
// entry federation/<detail>. Nothing behind ran: no chain, no meter.
func refusePeer(ctx context.Context, w http.ResponseWriter, code, msg string, reason fedhop.Reason, detail string) {
	if rec := oai.AccessRecordFrom(ctx); rec != nil {
		rec.Reason = code
		if detail != "" {
			rec.NoteRejection(keyquota.RejectFederation, detail)
		}
	}
	fedhop.SetReason(w.Header(), reason)
	oai.WriteCode(w, code, msg, false)
}

// Counters is a snapshot of the ingress counters.
type Counters struct {
	// PeerWithoutPath counts ctx-peer requests without Ctx-Path (step 6),
	// served or — with federation.require_peer_hop on — refused: the
	// runbook ground for the switch (F-W12): flip it only after this stayed
	// 0 over the observation window.
	PeerWithoutPath uint64 `json:"federation_peer_without_path"`
}

// CounterSnapshot returns the counters since process start (W06-16 shows
// them in the status).
func CounterSnapshot() Counters {
	return Counters{PeerWithoutPath: peerWithoutPath.Load()}
}

// warnOnce is a bounded least-recently-seen set of consumer ids with the
// time of their last WARN: at most one WARN per id and window.
type warnOnce struct {
	mu     sync.Mutex
	size   int
	window time.Duration
	now    func() time.Time
	order  *list.List // front = most recently seen; values *warnEntry
	index  map[string]*list.Element
}

type warnEntry struct {
	id     string
	warned time.Time
}

func newWarnOnce(size int, window time.Duration, now func() time.Time) *warnOnce {
	return &warnOnce{size: size, window: window, now: now, order: list.New(), index: make(map[string]*list.Element, size)}
}

// first reports whether id is due for a WARN — not in the set, or its last
// WARN is at least the window ago — and records the sighting as most
// recent; a full set evicts its least recently seen id.
func (s *warnOnce) first(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if e, ok := s.index[id]; ok {
		s.order.MoveToFront(e)
		ent := e.Value.(*warnEntry)
		if now.Sub(ent.warned) < s.window {
			return false
		}
		ent.warned = now
		return true
	}
	s.index[id] = s.order.PushFront(&warnEntry{id: id, warned: now})
	if s.order.Len() > s.size {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.index, oldest.Value.(*warnEntry).id)
	}
	return true
}
