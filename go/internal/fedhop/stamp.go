package fedhop

// The stamp of outgoing wire calls (design/04 §4.1 "Stamp (Algorithmus,
// normativ)"), its context carriers and the wait budget. Order at every wire
// site: row ExtraHeaders → Stamp → core headers (Content-Type,
// Authorization), so the stamp sits between row data and core and neither
// can be forged by a row.

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Tier is the header tier of a wire target (design/04 Begriffe). The zero
// value is TierLoop: a target whose tier was never set still carries the
// loop marker — the fail-closed direction for the loop protection — and no
// federation context, which only an explicit TierFederation sends.
type Tier uint8

const (
	// TierLoop sends Ctx-Hop alone: every target except openrouter.
	TierLoop Tier = iota
	// TierNone sends nothing and actively removes every Ctx-/X-Ctx- header:
	// openrouter only, the one third party that cannot forward to a ctx.
	TierNone
	// TierFederation sends Ctx-Hop and the Tier-F context: ctx rows only.
	TierFederation
)

// WireKind is the kind of wire call a target is derived for (HopTarget,
// F-W4): only an embed wire carries the pinned embed space.
type WireKind uint8

// The wire kinds.
const (
	WireChat WireKind = iota
	WireEmbed
	WireRerank
	WireProbe
)

// WireMargin is what WireTimeout adds on top of attempt timeout and wait
// budget: network round trip and answer path (design/04 §4.13, a mechanism
// constant shared with design/03 m — not a policy value).
const WireMargin = 500 * time.Millisecond

// Target is the hop descriptor of one wire site, derived from the backend
// row alone (backends.(*Backend).HopTarget, F-W4) — never from request data.
type Target struct {
	BackendID string
	Tier      Tier
	// Required is the sensitivity under which Pool.Chain admitted the row;
	// "" (a row not obtained through Chain) is sent as credentials.
	Required string
	// EgressLocal: a ctx row without metadata.federation_transit.
	EgressLocal bool
	// EmbedSpace is the pinned peer embed space (16 hex), embed wires only.
	EmbedSpace string
}

// Call is the call meta of the sender (design/04 §4.3): the effective lease
// class, the pipeline, and LocalOnly (G41) for unclassified content that must
// not leave the locality boundary, not even through a peer.
type Call struct {
	Class, Pipeline string
	LocalOnly       bool
}

type (
	inboundKey   struct{}
	callKey      struct{}
	requestIDKey struct{}
)

// WithInbound returns ctx carrying the parsed federation state of the
// incoming request — set only by the ingress (LoopGuard/PeerIngress, F-W8/
// F-W9); internal pipelines have none.
func WithInbound(ctx context.Context, in Inbound) context.Context {
	return context.WithValue(ctx, inboundKey{}, in)
}

func inboundFrom(ctx context.Context) Inbound {
	in, _ := ctx.Value(inboundKey{}).(Inbound)
	return in
}

// WithCall returns ctx carrying c merged into the call meta already there:
// LocalOnly sticks once set, a non-empty Class or Pipeline replaces the
// previous one, an empty one keeps it (ChainCall.Do sets the pipeline and
// LocalOnly, the admission then the lease class — neither wipes the other).
func WithCall(ctx context.Context, c Call) context.Context {
	prev := callFrom(ctx)
	if c.Class == "" {
		c.Class = prev.Class
	}
	if c.Pipeline == "" {
		c.Pipeline = prev.Pipeline
	}
	c.LocalOnly = c.LocalOnly || prev.LocalOnly
	return context.WithValue(ctx, callKey{}, c)
}

func callFrom(ctx context.Context) Call {
	c, _ := ctx.Value(callKey{}).(Call)
	return c
}

// WithRequestID returns ctx carrying the request id Stamp sends to ctx rows
// as X-Request-ID (masterplan K28: the server-side id).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// The peer wait budgets in nanoseconds, by urgency of the lease class: set by
// the settings reload (federation.peer_max_wait_{interactive,background},
// registered by F-W6). 0 = not configured: no Ctx-Max-Wait-Ms of our own.
var peerWaitInteractive, peerWaitBackground atomic.Int64

// SetPeerMaxWait installs the wait budgets Stamp sends to ctx rows, by
// urgency of the lease class (interactive and external use the first,
// background and external_bulk the second). A value ≤ 0 is "not
// configured"; a value above MaxWaitCeiling is sent as the ceiling.
func SetPeerMaxWait(interactive, background time.Duration) {
	peerWaitInteractive.Store(int64(max(interactive, 0)))
	peerWaitBackground.Store(int64(max(background, 0)))
}

func peerMaxWait(class string) time.Duration {
	if classRank(normalizeClass(class)) > 0 {
		return time.Duration(peerWaitInteractive.Load())
	}
	return time.Duration(peerWaitBackground.Load())
}

// maxWaitFor is the Ctx-Max-Wait-Ms of one call: min(own budget by lease
// class, inbound budget − time since receipt), floor 0, at most the ceiling,
// whole milliseconds; ok false when neither term exists.
func maxWaitFor(call Call, in Inbound) (time.Duration, bool) {
	w := peerMaxWait(call.Class)
	ok := w > 0
	if budget, has := in.Wait(); has {
		rem := max(budget-time.Since(in.ReceivedAt), 0)
		if !ok || rem < w {
			w, ok = rem, true
		}
	}
	if !ok {
		return 0, false
	}
	return min(w, MaxWaitCeiling).Truncate(time.Millisecond), true
}

// WireTimeout is the wire timeout of a site for target t (design/04 §4.13,
// K8): a Tier-F target waits base + the wait budget it sends + WireMargin,
// because the peer's queue time must not eat the attempt time. Every other
// tier, a base ≤ 0 (no timeout) and a call without wait budget keep base.
func WireTimeout(ctx context.Context, t Target, base time.Duration) time.Duration {
	if t.Tier != TierFederation || base <= 0 {
		return base
	}
	w, ok := maxWaitFor(callFrom(ctx), inboundFrom(ctx))
	if !ok {
		return base
	}
	return base + w + WireMargin
}

// Stamp writes the hop headers of one outgoing wire call to h (design/04
// §4.1, steps 1–6):
//
//  1. every header named Ctx-… or X-Ctx-… (any case) is removed — row data
//     (an extra_headers edit by psql) can neither set nor forge a marker;
//  2. TierNone: done;
//  3. no identity (or one without key): slog.Error, counter
//     federation_stamp_without_identity, done — no marker at all. ctxd
//     refuses to boot without an identity, so in ctxd this is a miswired
//     test; a tool that links the wire packages without loading the
//     identity hits it on every call, visibly;
//  4. Ctx-Hop = the inbound chain + a fresh own token;
//  5. TierLoop: done;
//  6. TierFederation: Ctx-Path = inbound path + own id; Ctx-Sensitivity =
//     MergeSensitivity(t.Required, inbound); Ctx-Egress = local if the row,
//     the inbound or the call says so, else any; Ctx-Class =
//     MergeClass(lease, inbound); Ctx-Max-Wait-Ms when a budget exists;
//     Ctx-Pipeline (the call's, else the inbound's) when valid; X-Request-ID
//     when the context carries one the receiver's middleware accepts; Ctx-Embed-Space-Require when the
//     target pins a valid embed space.
func Stamp(ctx context.Context, h http.Header, t Target) {
	for k := range h {
		if hasPrefixFold(k, "ctx-") || hasPrefixFold(k, "x-ctx-") {
			delete(h, k)
		}
	}
	if t.Tier == TierNone {
		return
	}
	id := IdentityFrom(ctx)
	tok := ""
	if id.ID != "" {
		tok = id.MintToken()
	}
	if tok == "" {
		stampWithoutIdentity.Add(1)
		slog.ErrorContext(ctx, "federation: stamp without instance identity, no hop marker sent", "backend", t.BackendID)
		return
	}
	in := inboundFrom(ctx)
	h.Set(HeaderHop, appendList(in.Hop, tok))
	if t.Tier != TierFederation {
		return
	}
	call := callFrom(ctx)
	h.Set(HeaderPath, appendList(in.Path, id.ID))
	h.Set(HeaderSensitivity, MergeSensitivity(t.Required, in.Sensitivity))
	egress := egressAny
	if t.EgressLocal || in.EgressLocal || call.LocalOnly {
		egress = egressLocal
	}
	h.Set(HeaderEgress, egress)
	h.Set(HeaderClass, MergeClass(call.Class, in.Class))
	if w, ok := maxWaitFor(call, in); ok {
		h.Set(HeaderMaxWait, formatMillis(w))
	}
	switch {
	case validPipeline(call.Pipeline):
		h.Set(HeaderPipeline, call.Pipeline)
	case validPipeline(in.Pipeline):
		h.Set(HeaderPipeline, in.Pipeline)
	}
	if rid := requestIDFrom(ctx); validRequestID(rid) {
		h.Set(HeaderRequestID, rid)
	}
	if validEmbedSpace(t.EmbedSpace) {
		h.Set(HeaderEmbedSpaceRequire, t.EmbedSpace)
	}
}

// Ingress is the registration of the federation ingress of ONE router
// (design/04 §4.6 "Registrierung als Mechanismus", §7.2 no. 1): the model
// groups (/v1, /v2) mark it when they are mounted with the LoopGuard fixed
// in front of their handlers (cmd/ctxd mountModelGroup, F-W8; F-W9 adds the
// PeerIngress to the condition), and the two assertions "no /v1 without its
// ingress protection" read it — the boot (gateway.enabled effectively true ∧
// not registered ⇒ no boot) and the settings write (gateway.enabled → true
// ⇒ 409).
//
// It is a value per router, not process state: two routers in one test
// process — one with a model group, one without — must not see each other's
// registration, and a process-wide flag would let the first mounted group
// open every later router. The zero value is "not registered"; so is a nil
// *Ingress — a reader that was never wired refuses (fail-closed).
type Ingress struct {
	loopGuard atomic.Bool
}

// MarkLoopGuard records that a model group of this router is mounted with
// the LoopGuard in front of it.
func (i *Ingress) MarkLoopGuard() { i.loopGuard.Store(true) }

// Registered reports whether the federation ingress of this router is
// mounted in front of the model API. A nil *Ingress is not registered.
func (i *Ingress) Registered() bool { return i != nil && i.loopGuard.Load() }

// Counters of the hop contract.
var (
	stampWithoutIdentity atomic.Uint64
	headerIgnored        atomic.Uint64
)

// Counters is a snapshot of the hop contract's counters.
type Counters struct {
	// StampWithoutIdentity counts Stamp calls without an identity (step 3).
	StampWithoutIdentity uint64 `json:"federation_stamp_without_identity"`
	// HeaderIgnored counts inbound header values ParseInbound did not take as
	// sent: ignored (Ctx-Max-Wait-Ms out of range, invalid Ctx-Pipeline) or
	// replaced by the fail-closed value (unknown Ctx-Sensitivity, Ctx-Egress,
	// Ctx-Class) — a peer of another version shows up here.
	HeaderIgnored uint64 `json:"federation_header_ignored"`
}

// CounterSnapshot returns the counters since process start.
func CounterSnapshot() Counters {
	return Counters{StampWithoutIdentity: stampWithoutIdentity.Load(), HeaderIgnored: headerIgnored.Load()}
}
