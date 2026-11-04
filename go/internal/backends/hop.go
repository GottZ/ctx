package backends

// The backend side of the hop contract (design/04 §4.3, §4.4; F-W4): the
// hop target of a row — derived from the row alone, never from request data —
// and the pool as the fedhop.Observer of the boot.

import (
	"log/slog"

	"github.com/GottZ/ctx/internal/fedhop"
)

// HopTarget is the hop descriptor of a wire call to b. The tier hangs on the
// provider class, like the ZDR force (precedent client.go): openrouter is
// TierNone, the one third party that cannot forward to a ctx instance, so it
// gets no hop header at all; every other class — generic, llamacpp, ctx
// until its federation tier lands, an unknown or empty one, any protocol
// including anthropic — is TierLoop and carries one opaque Ctx-Hop token.
// provider_class is an unverified declaration (Finding 6): a ctx instance
// registered as generic or llamacpp must still see the loop marker. The tier
// is always set explicitly — fedhop's zero Tier is TierLoop, not TierNone.
//
// kind selects the embed-space pin of a federation embed target; no tier
// derived here uses it yet.
func (b *Backend) HopTarget(kind fedhop.WireKind) fedhop.Target {
	t := fedhop.Target{BackendID: b.ID}
	if b.ProviderClass == ProviderOpenRouter {
		t.Tier = fedhop.TierNone
	} else {
		t.Tier = fedhop.TierLoop
	}
	return t
}

// HintAnswersAsCtxInstance is the BackendStatus hint of a row that answered
// with Ctx-Instance (design/04 §4.4): the backend is a ctx instance, but the
// row does not say so, so its calls carry the loop marker only and none of
// the federation context. A machine code — the admin UI words it.
const HintAnswersAsCtxInstance = "answers_as_ctx_instance"

// NotePeerInstance is the fedhop.Observer half that learns from answers: a
// row whose backend answered with a valid Ctx-Instance carries the
// HintAnswersAsCtxInstance status hint from then on (until the process
// restarts — the learned state with its reset rules is F-W11's). The first
// such answer of a row logs one WARN; later answers stay silent, so a load
// balancer over several instances cannot turn the hint into a log line per
// request. The instance id appears in that log line only, never in the
// status. A backend id that names no row (empty) is ignored.
func (p *Pool) NotePeerInstance(backendID, instance string) {
	if backendID == "" {
		return
	}
	p.healthM.Lock()
	h, ok := p.health[backendID]
	if !ok {
		h = &healthState{}
		p.health[backendID] = h
	}
	first := h.peerInstance == ""
	h.peerInstance = instance
	p.healthM.Unlock()
	if first {
		slog.Warn("backends: row answers as a ctx instance, but its provider_class does not say so — its calls carry no federation context",
			"backend", backendID, "instance", instance)
	}
}

// MarkSelfLoop is the fedhop.Observer half that reports a row leading back
// to this instance. Without effect until the self-loop exclusion (F-W11):
// the hint above already shows a row that answers as a ctx instance.
func (p *Pool) MarkSelfLoop(string) {}
