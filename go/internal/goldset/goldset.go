// Package goldset builds the retrieval gold set: the query slices G-KI, G-Q and
// G-REAL of stage 1 (design 04 §4.5), the multi-gold slices G-SESS, G-MH,
// G-GLOB and the floor check G-GLOB-KONSTR added in wave M-W5 (design 05 §4.5),
// plus the provenance stamp that binds all of them to how they were drawn.
//
// The gold set is FILE data, never a context_blocks row (§3.3) — a block
// holding the gold answer to a gold query would sort itself into the very
// measurement it exists for. Everything written here therefore lives under a
// root-only directory guarded by Guard, and every writer uses mode 0600.
//
// Relevance labels for G-REAL are deliberately NOT produced here; they need
// the pooling dump from the driver and land in wave B-W6.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package goldset

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/GottZ/ctx/internal/jsonl"
)

// Slice identifiers (design 04 §4.5, design 05 §4.5). Never pooled across
// slices — G-KI is structurally trigram-friendly and a mean over slices would
// transfer a figure between instruments that do not share one.
//
// The three multi-gold slices of wave M-W5 answer a structural gap: G-KI, G-Q
// and G-REAL carry exactly ONE gold id per case, and a one-gold slice cannot
// show the use of an aggregating layer — it can only punish it as displacement
// of the single gold block.
const (
	SliceKI   = "G-KI"
	SliceQ    = "G-Q"
	SliceReal = "G-REAL"
	// SliceSess is the session-window slice: "what was worked on at X on this
	// day/period". Gold is constructive — daily reports plus the knowledge
	// blocks created inside the window — and therefore NOT circular against
	// the insight layer it exists to measure.
	SliceSess = "G-SESS"
	// SliceMH is the multi-hop slice: two blocks bridged by a dream link at
	// confidence >= MinDreamConfidence, with a question that needs both.
	SliceMH = "G-MH"
	// SliceGlob is the global/aggregating slice. Its gold is judged, not
	// constructed (E-9), so the generated case carries the query and its pool
	// reference only.
	SliceGlob = "G-GLOB"
	// SliceGlobKonstr is the FLOOR CHECK for SliceGlob: same question family,
	// but gold taken from graph_cluster_member. That gold is circular against
	// the graph layer — a catalog block IS the cluster — so this slice is
	// never a rollout criterion, exactly the role G-KI has today.
	SliceGlobKonstr = "G-GLOB-KONSTR"
)

// Halves of the seeded 50/50 G-Q partition (§4.6): variants are derived on
// DERIV and confirmed on HOLD.
const (
	SplitDeriv = "DERIV"
	SplitHold  = "HOLD"
)

// File names inside the gold directory.
const (
	FileKI         = "g-ki.jsonl"
	FileQ          = "g-q.jsonl"
	FileReal       = "g-real.jsonl"
	FileSess       = "g-sess.jsonl"
	FileMH         = "g-mh.jsonl"
	FileGlob       = "g-glob.jsonl"
	FileGlobKonstr = "g-glob-konstr.jsonl"
	FileStamp      = "STAMP.json"
)

// DirName is the mandated gold directory name (§3.3). It lives under the
// private .project submodule, is untracked in the ctx repo and CI skips it.
const DirName = "goldset-retrieval-2026-08"

// fileMode keeps gold data owner-readable only — the slices carry real query
// texts and block ids of a private corpus.
const fileMode = 0o600

// Case is one gold query. GoldIDs is empty for G-REAL until B-W6 supplies the
// pooled relevance judgements; that is a documented stage boundary, not a gap.
type Case struct {
	Slice string `json:"slice"`
	Index int    `json:"index"`
	Query string `json:"query"`
	// QuerySHA256 lets reports cite a query as slice+index+hash prefix without
	// ever carrying the text out of the gold directory (§4.5).
	QuerySHA256 string `json:"query_sha256"`
	// GoldIDs are the constructive labels (G-KI, G-Q). Empty for G-REAL.
	GoldIDs []string `json:"gold_ids,omitempty"`
	// GoldSource names where GoldIDs came from when a slice carries more than
	// one gold variant side by side (wave C3-4a: "fable-kern" on the 20 core
	// queries, "judge-uebertragen" on all 150). Empty everywhere else, so a
	// slice file written before C3-4a round-trips byte-identically.
	GoldSource string `json:"gold_source,omitempty"`
	// Split is DERIV or HOLD, G-Q only.
	Split string `json:"split,omitempty"`
	// Origin names how the query was constructed: title-paraphrase,
	// llm-question, access-log, session-window, dream-bridge, tag-aggregate
	// or cluster-aggregate.
	Origin string `json:"origin"`
	// PoolRef names the construction source in a form a later judgement run can
	// resolve back to a candidate pool: "window:2026-08-18..2026-08-20",
	// "link:<src>|<dst>", "tag:<name>" or "cluster:<uuid>". G-GLOB carries no
	// gold, so without this reference its cases could not be pooled at all.
	PoolRef string `json:"pool_ref,omitempty"`
	// SourceTitle is the unparaphrased block title (G-KI only) — kept so the
	// paraphrase stays auditable against its input.
	SourceTitle string `json:"source_title,omitempty"`
}

// Generator records where G-Q questions came from. Block content reaches a
// model at exactly one point in this axis, so the endpoint is pinned and
// stamped (§4.5).
type Generator struct {
	Backend      string  `json:"backend"`
	Model        string  `json:"model"`
	Endpoint     string  `json:"endpoint"`
	Locality     string  `json:"locality"`
	Trust        string  `json:"trust"`
	PromptSHA256 string  `json:"prompt_sha256"`
	GeneratedAt  string  `json:"generated_at"`
	Calls        int     `json:"calls"`
	DurationSec  float64 `json:"duration_seconds"`
	Concurrency  int     `json:"concurrency"`
}

// SliceStamp is the per-slice profile: reached n, discard counts and the file
// digest that binds the stamp to the data it describes.
type SliceStamp struct {
	N          int    `json:"n"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	Candidates int    `json:"candidates,omitempty"`
	// DiscardedRedaction counts G-REAL texts dropped by the redaction sweep.
	// They are DROPPED, not redacted — a part-redacted query is no longer a
	// real query (§4.5).
	// A measured zero is a finding, not an absent field — no omitempty.
	DiscardedRedaction int `json:"discarded_redaction"`
	DiscardedGenerator int `json:"discarded_generator"`
	DiscardedHandcheck int `json:"discarded_handcheck"`
	// DiscardedConstruction counts candidates the construction rule itself
	// rejected before any model saw them — a window whose gold set exceeds the
	// cap, a dream link below the confidence floor, a cluster too small to
	// carry an aggregating question.
	DiscardedConstruction int `json:"discarded_construction,omitempty"`
	// GoldIDs is the total number of gold labels the slice carries. For the
	// multi-gold slices this is NOT n, and the drift census is sized on it
	// (design/05 F-25).
	GoldIDs       int `json:"gold_ids,omitempty"`
	GoldIDsMedian int `json:"gold_ids_median,omitempty"`
	HandcheckN    int `json:"handcheck_n,omitempty"`
	SplitDeriv    int `json:"split_deriv,omitempty"`
	SplitHold     int `json:"split_hold,omitempty"`
	// SplitFingerprint is the digest of the DERIV/HOLD partition — the seed
	// plus this value make the split reproducible and checkable.
	SplitFingerprint string `json:"split_fingerprint,omitempty"`
	// Profile is the declared construction of the slice: how it was built, what
	// it is biased towards, and which model wrote its questions. A slice
	// without one is a number without a method.
	Profile *SliceProfile `json:"profile,omitempty"`
}

// Stamp is the gold provenance file (§4.5, §5.3c).
type Stamp struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	// BuildRev is the Go VCS build stamp (+"-dirty"), identifying the binary —
	// not an assertion about the repository the gold data was drawn under.
	BuildRev string `json:"build_vcs_revision,omitempty"`
	// CorpusMaxCreatedAt is max(created_at) over context_blocks at draw time.
	// The score step flags any query whose top-k holds a block created after
	// this instant as contamination-suspect (§5.3c).
	CorpusMaxCreatedAt string `json:"corpus_max_created_at"`
	RetrievableBlocks  int    `json:"retrievable_blocks"`
	// Population is the K9 answer: there is no single canonical corpus count,
	// so every measurement names the ground set it drew from instead of
	// implying one.
	Population *Population           `json:"population,omitempty"`
	SampleSeed int64                 `json:"sample_seed"`
	SplitSeed  int64                 `json:"split_seed"`
	Generator  *Generator            `json:"generator,omitempty"`
	Slices     map[string]SliceStamp `json:"slices"`
	// AllowOutsideGoldset records a set --allow-outside-goldset override so the
	// report can declare it (§3.3, §4.8).
	AllowOutsideGoldset bool `json:"allow_outside_goldset"`
}

// SHA256Hex is the digest helper used for query hashes and file digests.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// WriteJSONL writes cases as one JSON object per line at mode 0600. Index and
// QuerySHA256 are (re)assigned here so a slice file is always self-consistent.
func WriteJSONL(path string, cases []Case) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := range cases {
		cases[i].Index = i
		cases[i].QuerySHA256 = SHA256Hex(cases[i].Query)
		if err := enc.Encode(cases[i]); err != nil {
			return fmt.Errorf("encode case %d: %w", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// O_CREATE applies fileMode only when the file did not exist, so a rewrite
	// of an already world-readable slice would keep it that way.
	return os.Chmod(path, fileMode)
}

// WriteJSONLKeepIndex writes cases WITHOUT reassigning their index (wave
// C3-4a, design/05a §C3-2-D05-7 step 4).
//
// WriteJSONL renumbers, which is right for a freshly drawn slice and wrong for
// a labelled SUBSET of one: the Fable core is 20 of the 150 G-REAL cases, and
// renumbering them 0..19 would give every one of them a case key that matches
// no dump record — the flip test would then silently compare an empty
// intersection instead of the core. The digest is still verified against the
// query text, so a file this writes is as self-consistent as one WriteJSONL
// produces; only the identity is taken from the case instead of from its
// position.
func WriteJSONLKeepIndex(path string, cases []Case) error {
	for i := range cases {
		if want := SHA256Hex(cases[i].Query); cases[i].QuerySHA256 != want {
			return fmt.Errorf("case %d (%s #%d): query_sha256 %q passt nicht zum Fragetext (%q)",
				i, cases[i].Slice, cases[i].Index, cases[i].QuerySHA256, want)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := range cases {
		if err := enc.Encode(cases[i]); err != nil {
			return fmt.Errorf("encode case %d: %w", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, fileMode)
}

// ReadJSONL reads a slice file back.
func ReadJSONL(path string) ([]Case, error) {
	return jsonl.All[Case](path)
}

// FileDigest is the sha256 of a file's bytes, for the stamp.
func FileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return SHA256Hex(string(b)), nil
}

// Population answers K9 (masterplan): the retrievable ground set has no single
// canonical size — 1 384 retrieval-visible, 1 407 including system-meta, and
// the overview node cut counts differently again. A measurement therefore
// states its own ground set rather than pointing at "the" corpus count.
type Population struct {
	// Definition is the filter in words, so a reader can reproduce the number.
	Definition string `json:"definition"`
	// Retrievable is the size of that ground set.
	Retrievable int `json:"retrievable"`
	// Active is every non-archived block, retrievable or not — the second
	// figure that makes the first one readable.
	Active int `json:"active"`
}

// WriteStamp persists the provenance file at mode 0600, keys sorted for a
// stable byte image.
//
// The on-prem assertion runs HERE rather than only at call time (§5 B6): the
// stamp is what a later reader trusts, so a stamp that would record an external
// endpoint aborts the build instead of reaching disk.
func WriteStamp(path string, s Stamp) error {
	if err := RequireOnPremStamp(s); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), fileMode)
}

// ReadStamp loads an existing stamp, or returns a zero stamp if absent.
func ReadStamp(path string) (Stamp, error) {
	var s Stamp
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Stamp{Version: 1, Slices: map[string]SliceStamp{}}, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.Slices == nil {
		s.Slices = map[string]SliceStamp{}
	}
	return s, nil
}

// SliceNames returns the stamped slice names in stable order.
func SliceNames(s Stamp) []string {
	names := make([]string, 0, len(s.Slices))
	for k := range s.Slices {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
