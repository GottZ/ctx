package fedhop

// Monotony of the hop headers (design/04 §1.2): a restrictive header holds
// for every caller, a widening one for nobody. Ctx-Sensitivity folds by rank
// maximum and can only raise; Ctx-Class folds to the lower class and can
// only lower. A missing header means the sender's own value (Finding 25);
// the restrictive default applies only when the own value is missing.

// Sensitivity levels, the vocabulary of backends.Sensitivity — fedhop is a
// leaf and cannot import it; internal/backends pins this copy to sensRank.
const (
	sensPublic      = "public"
	sensInternal    = "internal"
	sensPersonal    = "personal"
	sensCredentials = "credentials"
)

// sensitivityTable is the Ctx-Sensitivity vocabulary in rank order: the
// index is the rank of backends.sensRank (public 0 … credentials 3).
var sensitivityTable = [...]string{sensPublic, sensInternal, sensPersonal, sensCredentials}

// Class names, the String() vocabulary of dispatch.Class including design/03's
// external classes (Finding 30.2); internal/dispatch pins this copy.
const (
	classInteractive  = "interactive"
	classExternal     = "external"
	classBackground   = "background"
	classExternalBulk = "external_bulk"
)

// classTable is the Ctx-Class vocabulary with its urgency. The order across
// the hop is two-level — urgent (a caller waits) above bulk (nobody waits) —
// and nothing finer: whether external urgent work ranks above in-house
// background work is the receiver's policy (A-E2 dispatch.external_order),
// not part of the hop contract.
var classTable = [...]struct {
	name   string
	urgent bool
}{
	{classInteractive, true},
	{classExternal, true},
	{classBackground, false},
	{classExternalBulk, false},
}

// SensitivityVocabulary returns the Ctx-Sensitivity values, lowest rank
// first (a copy).
func SensitivityVocabulary() []string { return append([]string(nil), sensitivityTable[:]...) }

// ClassVocabulary returns the Ctx-Class values (a copy).
func ClassVocabulary() []string {
	out := make([]string, 0, len(classTable))
	for _, c := range classTable {
		out = append(out, c.name)
	}
	return out
}

func knownSensitivity(s string) bool {
	for _, v := range sensitivityTable {
		if v == s {
			return true
		}
	}
	return false
}

// normalizeSensitivity: a known value as it is, anything else (empty
// included) credentials — the doctrine of backends.sensRank.
func normalizeSensitivity(s string) string {
	if knownSensitivity(s) {
		return s
	}
	return sensCredentials
}

// sensitivityRank: the table index, credentials for anything unknown.
func sensitivityRank(s string) int {
	for i, v := range sensitivityTable {
		if v == s {
			return i
		}
	}
	return len(sensitivityTable) - 1
}

func knownClass(c string) bool {
	for _, v := range classTable {
		if v.name == c {
			return true
		}
	}
	return false
}

// normalizeClass: a known class as it is, anything else (empty included)
// background.
func normalizeClass(c string) string {
	if knownClass(c) {
		return c
	}
	return classBackground
}

// classRank: 1 for an urgent class, 0 for a bulk or unknown one.
func classRank(c string) int {
	for _, v := range classTable {
		if v.name == c && v.urgent {
			return 1
		}
	}
	return 0
}

// MergeSensitivity folds an inbound Ctx-Sensitivity into the sender's own
// requirement: hdr "" (absent) ⇒ base; otherwise the higher-ranked of the
// two. A base outside the vocabulary — "" included, the requirement of a row
// not obtained through Pool.Chain — is credentials, as is an unknown hdr.
func MergeSensitivity(base, hdr string) string {
	b := normalizeSensitivity(base)
	if hdr == "" {
		return b
	}
	if h := normalizeSensitivity(hdr); sensitivityRank(h) > sensitivityRank(b) {
		return h
	}
	return b
}

// MergeClass folds an inbound Ctx-Class into the lease class: hdr "" (absent)
// ⇒ lease; otherwise the lower of the two, the lease on a tie. A lease
// outside the vocabulary — "" included, forgotten call meta — is background,
// as is an unknown hdr.
func MergeClass(lease, hdr string) string {
	l := normalizeClass(lease)
	if hdr == "" {
		return l
	}
	if h := normalizeClass(hdr); classRank(h) < classRank(l) {
		return h
	}
	return l
}
