package handler

// LoopGuard: the federation loop and depth guard at the ingress (wave F-W8,
// design/04 §4.6, masterplan K14). It sits directly behind the
// authentication of every group with model egress — /api, /mcp
// (handler.Auth) and /v1, /v2 (ModelAuth, fixed in cmd/ctxd mountModelGroup)
// — and before body cap, body parse and admission, so a loop costs one auth
// round trip and nothing else, and an anonymous probe never learns which
// token this instance recognises (no instance oracle).

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/oai"
)

// LoopGuardEnvelope is the error form a LoopGuard answers in.
type LoopGuardEnvelope uint8

const (
	// LoopGuardHouse is the house envelope {"success":false,"error":…,"code":…}
	// of /api and /mcp.
	LoopGuardHouse LoopGuardEnvelope = iota
	// LoopGuardOpenAI is the OpenAI envelope (oai.Write) of /v1 and /v2.
	LoopGuardOpenAI
)

// LoopGuardConfig wires one LoopGuard.
type LoopGuardConfig struct {
	// MaxDepth is federation.max_depth, read on every request (hot): a chain
	// that already holds MaxDepth entries is refused — this instance would be
	// hop MaxDepth+1. Required.
	MaxDepth func() int
	// Envelope is the error form; the zero value is the house envelope.
	Envelope LoopGuardEnvelope
}

// The refusal texts. Fixed bytes: a refusal names no chain entry, no token,
// no instance id, no depth and no limit (design/04 §4.6, the oracle doctrine
// of design/03 C2) — the code and Ctx-Reason carry the class, the rest
// would map the federation for whoever sends a header.
const (
	loopGuardTextInvalid  = "The request was refused: a federation header cannot be read."
	loopGuardTextLoop     = "The request was refused: its federation chain is a loop."
	loopGuardTextDepth    = "The request was refused: its federation chain is too long."
	loopGuardTextInternal = "The server could not complete the request."
)

// loopGuard is one configured guard: the policy getter, the envelope and the
// four vocabulary entries it answers with.
type loopGuard struct {
	maxDepth                       func() int
	envelope                       LoopGuardEnvelope
	invalid, loop, depth, internal oai.CodeSpec
}

// LoopGuard refuses a request whose federation chain is unreadable, a loop
// or too long (design/04 §4.6, steps 1–4):
//
//  1. fedhop.ParseInbound: an unreadable hop header ⇒ 400
//     federation_header_invalid; a chain that repeats an entry ⇒ 508
//     federation_loop (every 400 before any 508: garbage beats duplicate);
//  2. a chain that carries one of this instance's own tokens or its instance
//     id in Ctx-Path ⇒ 508 federation_loop with Ctx-Reason: loop, and for
//     an own token Ctx-Loop-Token: <token> — the echo the sender learns its
//     self-loop from (F-W11);
//  3. len(Ctx-Hop) ≥ federation.max_depth ⇒ 508 federation_depth with
//     Ctx-Reason: depth;
//  4. otherwise the request goes on with the chain in its context, reduced to
//     Ctx-Hop and Ctx-Path (fedhop.WithInbound): the stamps of the internal
//     pipelines behind it continue the chain, and nothing else a foreign
//     request sent — sensitivity, egress, class, wait budget, pipeline —
//     reaches any stage (on /v1 and /v2 F-W9's PeerIngress replaces it by
//     the cleaned full Inbound).
//
// Every refusal says x-should-retry: false; the OpenAI envelope takes status
// and type from oai.Codes, the house envelope the same status and code.
//
// A router without instance identity (only a test router: ctxd refuses to
// boot without one, F-W2) cannot tell its own token: a request with
// Ctx-Hop or Ctx-Path is refused with the A-internal 503 internal_error and
// an ERROR line (fail-closed), one without them passes. Steps 1's refusals
// need no identity and come first.
//
// No body read, no database, no lock: without federation headers the cost
// is one ParseInbound over empty headers. A missing MaxDepth is a wiring bug
// and panics at construction.
func LoopGuard(cfg LoopGuardConfig) func(http.Handler) http.Handler {
	if cfg.MaxDepth == nil {
		panic("handler.LoopGuard: nil federation.max_depth getter")
	}
	g := &loopGuard{
		maxDepth: cfg.MaxDepth,
		envelope: cfg.Envelope,
		invalid:  loopGuardSpec(oai.CodeFederationHeaderInvalid),
		loop:     loopGuardSpec(oai.CodeFederationLoop),
		depth:    loopGuardSpec(oai.CodeFederationDepth),
		internal: loopGuardSpec(oai.CodeInternalError),
	}
	return g.middleware
}

// loopGuardSpec is the vocabulary entry of code; a code oai.Codes does not
// hold is a wiring bug of this file and panics at construction.
func loopGuardSpec(code string) oai.CodeSpec {
	for _, s := range oai.Codes() {
		if s.Code == code {
			return s
		}
	}
	panic("handler.LoopGuard: code " + code + " is not in oai.Codes")
}

func (g *loopGuard) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in, err := fedhop.ParseInbound(r.Header)
		if err != nil {
			var dup *fedhop.DuplicateEntryError
			if errors.As(err, &dup) {
				g.refuse(w, g.loop, loopGuardTextLoop, fedhop.ReasonLoop)
				return
			}
			g.refuse(w, g.invalid, loopGuardTextInvalid, "")
			return
		}
		ctx := r.Context()
		id := fedhop.IdentityFrom(ctx)
		if id.ID == "" {
			if len(in.Hop) == 0 && len(in.Path) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			slog.ErrorContext(ctx, "federation: hop chain on a router without instance identity — refused, own tokens cannot be recognised (wiring defect)",
				"path", r.URL.Path, "request_id", RequestIDFromContext(ctx))
			g.refuse(w, g.internal, loopGuardTextInternal, "")
			return
		}
		if tok, loop := in.LoopFor(id); loop {
			if tok != "" {
				w.Header().Set(fedhop.HeaderLoopToken, tok)
			}
			g.refuse(w, g.loop, loopGuardTextLoop, fedhop.ReasonLoop)
			return
		}
		// A request without chain is no federated request: the depth limit
		// does not apply to it, whatever the getter says (a hand-built config
		// without the federation group reads 0 — V43 refuses that value, a
		// test router might not), and it costs no snapshot read.
		if len(in.Hop) > 0 && len(in.Hop) >= g.maxDepth() {
			g.refuse(w, g.depth, loopGuardTextDepth, fedhop.ReasonDepth)
			return
		}
		next.ServeHTTP(w, r.WithContext(fedhop.WithInbound(ctx, fedhop.Inbound{Hop: in.Hop, Path: in.Path})))
	})
}

// refuse writes one refusal in the guard's envelope. reason "" sets no
// Ctx-Reason (fedhop.SetReason sets only vocabulary values).
func (g *loopGuard) refuse(w http.ResponseWriter, spec oai.CodeSpec, text string, reason fedhop.Reason) {
	fedhop.SetReason(w.Header(), reason)
	if g.envelope == LoopGuardOpenAI {
		oai.Write(w, oai.Error{Status: spec.Status, Type: spec.Type, Code: spec.Code, Message: text})
		return
	}
	w.Header().Set("x-should-retry", "false")
	writeJSONCode(w, spec.Status, spec.Code, map[string]any{"success": false, "error": text})
}
