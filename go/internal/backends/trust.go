package backends

// Trust is the per-backend trust level: the maximum content sensitivity a
// backend is allowed to receive. Names are FIX (user decision, F3 §5a).
type Trust string

// Trust levels, most to least trusted.
const (
	TrustFull          Trust = "full-trust"     // local providers + ZDR cloud
	TrustNoCredentials Trust = "no-credentials" // passwords never leave the trusted zone
	TrustNonPersonal   Trust = "non-personal"   // additionally no personal data
	TrustPublic        Trust = "public"         // public content only
)

// Sensitivity is the per-content classification (block, query, operation).
type Sensitivity string

// Sensitivity levels, rank 3 (most sensitive) to rank 0.
const (
	SensCredentials Sensitivity = "credentials" // rank 3
	SensPersonal    Sensitivity = "personal"    // rank 2
	SensInternal    Sensitivity = "internal"    // rank 1
	SensPublic      Sensitivity = "public"      // rank 0
)

// sensRank maps a sensitivity to its numeric rank. Empty/unknown values are
// NORMATIVELY rank 3 (credentials, fail-closed): a forgotten assignment at a
// future call site (zero value) must never act as a silent public downgrade
// inside the security gate (F3 §2.3a).
func sensRank(s Sensitivity) int {
	switch s {
	case SensPublic:
		return 0
	case SensInternal:
		return 1
	case SensPersonal:
		return 2
	default: // SensCredentials, "", unknown
		return 3
	}
}

// maxRank returns the highest sensitivity rank a trust level may receive.
// Unknown trust admits NOTHING (-1): the DDL CHECK makes this unreachable
// from the database, but a zero-value Trust in Go stays fail-closed.
func maxRank(t Trust) int {
	switch t {
	case TrustFull:
		return 3
	case TrustNoCredentials:
		return 2
	case TrustNonPersonal:
		return 1
	case TrustPublic:
		return 0
	default:
		return -1
	}
}

// Allows implements the permission matrix: a backend with trust t may receive
// content of sensitivity s iff rank(s) <= maxRank(t). Empty/unknown
// sensitivity counts as credentials, empty/unknown trust admits nothing.
func (t Trust) Allows(s Sensitivity) bool {
	return sensRank(s) <= maxRank(t)
}

// MaxSensitivity folds the operation requirement over all prompt parts
// (query + every block in the final prompt set). Empty input is credentials:
// an operation that declares no parts gets the fail-closed default, never a
// free pass.
func MaxSensitivity(parts ...Sensitivity) Sensitivity {
	max := -1
	for _, p := range parts {
		if r := sensRank(p); r > max {
			max = r
		}
	}
	switch max {
	case 0:
		return SensPublic
	case 1:
		return SensInternal
	case 2:
		return SensPersonal
	default: // 3 or empty parts list
		return SensCredentials
	}
}

// ValidTrust reports whether t is one of the four defined levels (API
// validation; the DB CHECK is the second line of defense).
func ValidTrust(t Trust) bool {
	return maxRank(t) >= 0
}

// Rank exposes the admittance rank for elevation comparisons (a rising rank
// = more content may flow = the confirm-gated direction). Unknown trust is
// -1, below every defined level.
func (t Trust) Rank() int {
	return maxRank(t)
}

// ValidSensitivity reports whether s is one of the four defined levels.
// Note the asymmetry to sensRank: unknown values FAIL validation at the API
// boundary but act as credentials once inside the gate.
func ValidSensitivity(s Sensitivity) bool {
	switch s {
	case SensCredentials, SensPersonal, SensInternal, SensPublic:
		return true
	default:
		return false
	}
}
