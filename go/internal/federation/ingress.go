package federation

import (
	"container/list"
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/fedhop"
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

	// The peer stage (design/04 §4.6 steps 6–8, §4.8; wave F-W12). RC-D S1:
	// declared, not read yet — S2 makes Binder required (nil panics at
	// construction like Consumer) and wires all three in cmd/ctxd.
	//
	// Binder writes the trust-on-first-use binding of a ctx-peer
	// (auth.PeerBindings).
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
//     FR-2). federation.require_peer_hop (400) is F-W12's;
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
// admission. No binding, no Ctx-Instance to peers (steps 7/8, F-W12).
//
// A header that does not parse here means the LoopGuard is not in front
// (it refuses every parse error first): the same 503 as step 5. The cost
// per request is one ParseInbound and, for a pathless peer, one lock on the
// WARN set. A missing Consumer accessor is a wiring bug and panics at
// construction.
func PeerIngress(cfg PeerIngressConfig) func(http.Handler) http.Handler {
	if cfg.Consumer == nil {
		panic("federation.PeerIngress: nil consumer accessor")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
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
			switch {
			case ca.Kind != auth.KindCtxPeer:
				in.Class, in.Pipeline = "", ""
			case len(in.Path) == 0:
				in.Class, in.Pipeline = "", ""
				in.EgressLocal = true
				peerWithoutPath.Add(1)
				if peerWarned.first(ca.ConsumerID) {
					log.WarnContext(ctx, "federation: ctx-peer request without Ctx-Path — served without class or pipeline hints (the peer predates F-W6 or registered this instance as generic)",
						"consumer_id", ca.ConsumerID)
				}
			}
			ctx = WithPolicy(ctx, ResolvePolicy(ca, in))
			next.ServeHTTP(w, r.WithContext(fedhop.WithInbound(ctx, in)))
		})
	}
}

// peerWithoutPathWarnSetSize bounds the set behind "one WARN per consumer"
// (step 6): a mechanism bound on memory, not a policy value (A-E2). At the
// target scale (dozens of consumers per instance, ≥ 10 instances) every
// peer consumer fits; beyond it the least recently seen consumer falls out
// and would warn once more — the log stays bounded either way.
const peerWithoutPathWarnSetSize = 1024

var (
	peerWithoutPath atomic.Uint64
	peerWarned      = newWarnOnce(peerWithoutPathWarnSetSize)
)

// Counters is a snapshot of the ingress counters.
type Counters struct {
	// PeerWithoutPath counts ctx-peer requests without Ctx-Path (step 6) —
	// the runbook ground for federation.require_peer_hop (F-W12): flip it
	// only after this stayed 0 over the observation window.
	PeerWithoutPath uint64 `json:"federation_peer_without_path"`
}

// CounterSnapshot returns the counters since process start (W06-16 shows
// them in the status).
func CounterSnapshot() Counters {
	return Counters{PeerWithoutPath: peerWithoutPath.Load()}
}

// warnOnce is a bounded least-recently-seen set of consumer ids.
type warnOnce struct {
	mu    sync.Mutex
	size  int
	order *list.List // front = most recently seen
	index map[string]*list.Element
}

func newWarnOnce(size int) *warnOnce {
	return &warnOnce{size: size, order: list.New(), index: make(map[string]*list.Element, size)}
}

// first reports whether id was not in the set, and records it as most
// recently seen; a full set evicts its least recently seen id.
func (s *warnOnce) first(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.index[id]; ok {
		s.order.MoveToFront(e)
		return false
	}
	s.index[id] = s.order.PushFront(id)
	if s.order.Len() > s.size {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.index, oldest.Value.(string))
	}
	return true
}
