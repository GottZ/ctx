package modelalias

// Effective vision of an alias (FOLGE-Z Z2): policy.vision set ⇒ the policy
// decides (an explicit false stays a hard refusal, an explicit true forces
// images through); unset ⇒ the alias accepts images exactly when its chain
// holds a row whose capability probe reported vision=true — no extra step
// for the operator, and an unknown probe never opens the door.

import "github.com/GottZ/ctx/internal/backends"

// VisionSource names what decided an alias's effective vision.
type VisionSource string

const (
	// VisionPolicy: policy.vision is set and decides.
	VisionPolicy VisionSource = "policy"
	// VisionBackend: policy.vision is unset and a row of the chain reported
	// vision=true.
	VisionBackend VisionSource = "backend"
	// VisionNone: policy.vision is unset and no row of the chain reported
	// vision=true (unknown counts as not capable).
	VisionNone VisionSource = "none"
)

// VisionView is the pool's answer the effective vision reads:
// (*backends.Pool).ImageCapable.
type VisionView interface {
	ImageCapable(role string, sens backends.Sensitivity, scope string) bool
}

// EffectiveVision resolves the vision of an alias with policy p, pool role,
// sensitivity floor sens and scope against view (nil view = no row known
// capable).
func EffectiveVision(p Policy, role string, sens backends.Sensitivity, scope string, view VisionView) (bool, VisionSource) {
	if p.Vision.Set {
		return p.Vision.V, VisionPolicy
	}
	if view != nil && view.ImageCapable(role, sens, scope) {
		return true, VisionBackend
	}
	return false, VisionNone
}

// SetVisionView installs the pool view every Reload resolves the aliases'
// effective vision against; it takes effect from the next Reload (boot
// installs it after the registry's first load and triggers one at once —
// cmd/ctxd bootCapabilityProbe). The pool's change hook triggers a reload
// whenever a row or a capability moves.
func (r *Registry) SetVisionView(v VisionView) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vision = v
}

// DoorVision is the vision the /v1 door applies to a: policy.vision when set
// (VisionPolicy), else the effective vision resolved at load (VisionBackend
// when true, VisionNone otherwise).
func DoorVision(a Alias) (bool, VisionSource) {
	if a.Policy.Vision.Set {
		return a.Policy.Vision.V, VisionPolicy
	}
	if a.EffectiveVision && a.VisionSource == VisionBackend {
		return true, VisionBackend
	}
	return false, VisionNone
}
