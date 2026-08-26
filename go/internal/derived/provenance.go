package derived

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MetadataKey is the ONE metadata key the whole derived layer occupies (§3.2).
// One key, so the collision surface against the 30 metadata keys already in
// the corpus is exactly one line wide and source_block_ids on the metadata
// root (319 checkpoint manifests) stays untouched.
const MetadataKey = "provenance"

// Sensitivity levels, as strings. Deliberately NOT backends.Sensitivity:
// derived is a leaf package (see the package doc) and backends is not on its
// import list. The four literals are the same ones backends/trust.go:20-23
// defines, and the golden test pins them so a rename there turns this red.
const (
	SensitivityCredentials = "credentials"
	SensitivityPersonal    = "personal"
	SensitivityInternal    = "internal"
	SensitivityPublic      = "public"
)

// sensitivityRank orders the four levels; higher is more confidential. Same
// ranking as backends/trust.go.
var sensitivityRank = map[string]int{
	SensitivityPublic:      0,
	SensitivityInternal:    1,
	SensitivityPersonal:    2,
	SensitivityCredentials: 3,
}

// Anchor kinds — WHAT a derived block hangs on. Exactly one form per block.
const (
	AnchorClusterTopic = "cluster_topic"
	AnchorRootSession  = "root_session"
	AnchorBlock        = "block"
)

// Anchor is the "what does this block hang on" half of the provenance (§3.2).
// Exactly one form is populated, and V7 enforces it: the kind and the set
// fields have to agree, otherwise a regeneration picker cannot tell which
// query finds the block again.
type Anchor struct {
	Kind string `json:"kind"`

	// cluster_topic
	TopicID string `json:"topic_id"`

	// CoreHash is the drift anchor and it belongs to THIS arm — never
	// graph_cluster_topic.label_stale, which the label arm owns and clears
	// (§4.7.3: "der Anker gehört dem Arm, der ihn löscht").
	CoreHash string `json:"core_hash"`

	// Attempts and NextAttemptAt are the termination pair (§4.7.4). The
	// counter alone must not be the termination condition: live, 13 of 52
	// topics burned label_attempts to 3 within a single microsecond-identical
	// tick, so one systemic outage can exhaust a whole population. The backoff
	// on the time axis is what a single outage cannot burn; the counter is the
	// emergency brake behind it.
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`

	// block (level-2 anchor onto a level-1 block)
	BlockID string `json:"block_id"`

	// root_session
	RootSessionID string `json:"root_session_id"`
	ManifestID    string `json:"manifest_id"` // NULL allowed (§0/K2)
	WatermarkFrom *int64 `json:"watermark_from"`
}

// Generator records WHO produced the content, so I1 (regenerable) is a
// verifiable statement and not a promise: content is a function of source set,
// generator version and model.
type Generator struct {
	// Model is who ANSWERED (the served model), not who was selected.
	Model string `json:"model"`

	// PromptVersion is a Go constant; changing a prompt is a deploy.
	PromptVersion string `json:"prompt_version"`

	// GateVersion is the version of G0–G7 that admitted the lines.
	GateVersion int `json:"gate_version"`
}

// Coverage is the machine-readable half of I6. The same numbers appear in the
// block TEXT (RenderBlock), because the most important consumer of a block —
// /api/query → sources[] — never sees metadata (llm/synthesize.go:221-251).
//
// ClaimsOffered and ClaimsKept are BLOCK sums over all map steps, not the
// values of a single call (§4.4.2).
type Coverage struct {
	ClaimsOffered  int            `json:"claims_offered"`
	ClaimsKept     int            `json:"claims_kept"`
	Rejects        map[string]int `json:"rejects"`
	SourcesCovered int            `json:"sources_covered"`
	Truncated      bool           `json:"truncated"`
}

// Provenance is metadata.provenance (§3.2) — the whole contract of a derived
// block in one namespaced key.
type Provenance struct {
	V                int       `json:"v"`
	Stratum          Stratum   `json:"stratum"`
	Arm              string    `json:"arm"`
	SourceBlockIDs   []string  `json:"source_block_ids"`
	SourceCount      int       `json:"source_count"`
	SourceDigest     string    `json:"source_digest"`
	Anchor           Anchor    `json:"anchor"`
	GeneratedAt      time.Time `json:"generated_at"`
	Generator        Generator `json:"generator"`
	Coverage         Coverage  `json:"coverage"`
	UntrustedSources int       `json:"untrusted_sources"`
	SensitivityMax   string    `json:"sensitivity_max"`
}

// provenanceWire avoids the infinite recursion an alias-free MarshalJSON on
// Provenance would cause.
type provenanceWire Provenance

// ErrContractVersion is returned for any provenance whose v is not
// ContractVersion — on the way in AND on the way out.
var ErrContractVersion = fmt.Errorf("derived: unsupported provenance contract version (want v=%d)", ContractVersion)

// MarshalJSON refuses to serialise a provenance of an unknown contract
// version. Fail-closed in the write direction too, not only on decode: a
// struct built by a caller that was compiled against another contract must not
// reach the database wearing this build's field names.
func (p Provenance) MarshalJSON() ([]byte, error) {
	if p.V != ContractVersion {
		return nil, ErrContractVersion
	}
	return json.Marshal(provenanceWire(p))
}

// UnmarshalJSON refuses an unknown contract version (§3.2: "Decode lehnt
// Unbekanntes ab"). A missing v decodes to 0 and is refused by the same
// branch, which is the point: metadata that never carried the contract is not
// a v=1 provenance with defaults, it is not a provenance.
func (p *Provenance) UnmarshalJSON(b []byte) error {
	var w provenanceWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if w.V != ContractVersion {
		return ErrContractVersion
	}
	*p = Provenance(w)
	return nil
}

// SourceDigest is the O(1) comparison value for "has the source set changed?":
// sha256 over the sorted, comma-joined id list, prefixed with the algorithm.
//
// The digest is the cheap prefilter, the list stays the truth — a 147-member
// cluster (the live maximum) carries 147 UUIDs, and a regeneration picker that
// compares those pairwise on every pass does not scale.
//
// Sorting here is what makes the digest independent of the order a caller
// happens to hold the ids in; it does NOT excuse an unsorted
// SourceBlockIDs field, which V3 checks separately.
func SourceDigest(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, ",")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
