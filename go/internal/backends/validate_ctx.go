package backends

import (
	"fmt"
	"math"
	"strings"
)

// ctxSlotsRoles are the roles that make limits.slots mandatory on a ctx row:
// the background roles the dispatcher's Enforcing coverage term derives
// reachability from (dispatch.backgroundRoles, enforcing.go). A ctx row
// counts as a local target there whatever its locality (F-W7), so a ctx row
// carrying one of them without slots keeps Enforcing off for the whole
// instance. backends does not import dispatch; the owner side pins the pair
// (events TestCtxSlotsRolesMatchDispatchBackgroundRoles).
var ctxSlotsRoles = []string{RoleDream, RoleDreamEmbed, RoleEmbed, RoleDigest, RoleClassify}

// validateCtx covers the row shape of a peer ctx instance (design/04 §4.12,
// wave F-W5) — the class needs no cooperation of the backend (DECISIONS
// A-E5), so every rule is a runtime failure on this side turned into a 422:
//   - protocol openai: the peer serves chat, embeddings and rerank on one
//     authenticated /v1 surface; the ollama, rerank and anthropic wires do
//     not exist there.
//   - api_key_ref: /v1 answers a keyless call with 401 — the ref names a
//     model key (ctxm_) of the peer.
//   - no embed role, whatever the locality and whatever proof the metadata
//     carries: the vector space of a peer is pinned by nothing yet (F-W14),
//     and a foreign quantization corrupts this instance's index silently.
//   - limits.slots (an integer 1..math.MaxInt32) as soon as a background role
//     is carried — without it Enforcing stays off (F-W7) and the background
//     calls fill the peer's concurrency limit of this instance's consumer.
//
// slots above the peer's declared limit (metadata.peer_limits.max_concurrency,
// written by the peer-limits probe of F-W16) is a warning, not a refusal: the
// peer answers the surplus with 429, and the value is read only when present.
func validateCtx(b *Backend) (warnings []string, errs []FieldError) {
	if b.ProviderClass != ProviderCtx {
		return nil, nil
	}
	if b.Protocol != ProtocolOpenAI {
		errs = append(errs, FieldError{"protocol",
			"provider_class ctx requires protocol 'openai' (a peer ctx instance serves chat, embeddings and rerank on its /v1 surface)"})
	}
	if strings.TrimSpace(b.APIKeyRef) == "" {
		errs = append(errs, FieldError{"api_key_ref",
			"provider_class ctx requires api_key_ref — the peer's /v1 surface is authenticated: name the secret holding a model key of the peer (create the secret first)"})
	}
	for _, r := range b.Roles {
		if embedRoles[r] {
			errs = append(errs, FieldError{"roles", fmt.Sprintf(
				"role %q is refused on provider_class ctx: vector-space pin not available for ctx rows yet (embeddings of a peer would write a foreign vector space into this instance's index)", r)})
		}
	}
	slots, hasSlots := b.Limits["slots"]
	if ctxCarriesBackgroundRole(b) && (!hasSlots || !positiveInteger(slots)) {
		errs = append(errs, FieldError{"limits.slots",
			"provider_class ctx with a background role requires limits.slots — an integer from 1 to 2147483647, at most the concurrency the peer grants this instance's consumer (without it the dispatcher cannot hold this instance's order up to the peer's door)"})
	}
	if hasSlots && positiveInteger(slots) {
		if peer, ok := peerMaxConcurrency(b); ok && limitValue(slots) > peer {
			warnings = append(warnings, fmt.Sprintf(
				"limits.slots %d exceeds metadata.peer_limits.max_concurrency %d of the peer — the peer answers the surplus with 429",
				limitValue(slots), peer))
		}
	}
	return warnings, errs
}

func ctxCarriesBackgroundRole(b *Backend) bool {
	for _, r := range ctxSlotsRoles {
		if b.HasRole(r) {
			return true
		}
	}
	return false
}

// peerMaxConcurrency reads metadata.peer_limits.max_concurrency when it is a
// positive integer; anything else counts as absent (no warning).
func peerMaxConcurrency(b *Backend) (int64, bool) {
	limits, ok := b.Metadata["peer_limits"].(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := limits["max_concurrency"]
	if !ok || !positiveInteger(v) {
		return 0, false
	}
	return limitValue(v), true
}

// limitValue converts a value positiveInteger accepted.
func limitValue(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(math.Trunc(n))
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}
