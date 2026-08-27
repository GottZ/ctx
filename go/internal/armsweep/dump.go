package armsweep

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// fileMode keeps every artefact of this instrument owner-readable only. Dumps
// carry the effective query texts of a private corpus — the same class of data
// the gold slices carry, and they live under the same root-only directory.
const fileMode = 0o600

// DumpDirName is the sink beneath the gold directory. Dumps NEVER land next to
// the slice files: a stray `g-*.jsonl` glob would otherwise pick up a dump and
// a loader would read it as gold data.
const DumpDirName = "dumps"

// ReportDirName is the basename the score step's output directory must carry.
const ReportDirName = "reports"

// Selector mirrors the arm_ranks selector block (handler/query.go:1258-1274) —
// the Gen-15 decision in the shape SQL received it. Kept as its own type rather
// than a map so a missing field is a compile error, not a silent zero.
type Selector struct {
	Mode       string `json:"mode"`
	Reason     string `json:"reason"`
	Estimate   int    `json:"estimate"`
	ScanTuples *int   `json:"scan_tuples"`
	ExactCap   *int   `json:"exact_cap"`
}

// Delivered is one row of the LIVE delivered ranking — what the caller actually
// received after gravity, cluster, graph, fold and truncation.
//
// ViaPostStage is DERIVED, not reported by the server: rrf.SearchResult carries
// ViaGraph/ViaCluster (search.go:48,64) but neither llm.Source
// (llm/synthesize.go:221-229) nor sourceResponse (handler/query.go:364-374)
// has the field, so the flag does not exist on the wire. The stand-in is exact
// for what the sweep needs it for: a delivered id that is not among the arm
// candidates cannot have come out of the fusion, so a post-fusion stage put it
// there. It does not distinguish graph from cluster injection — see the wave
// report's deviation note.
type Delivered struct {
	ID           string `json:"id"`
	ViaPostStage bool   `json:"via_post_stage"`
}

// Record is one measured gold query: the fusion inputs, the fusion order the
// live statement produced from them, and the ranking that was delivered.
type Record struct {
	Slice       string `json:"slice"`
	Index       int    `json:"index"`
	QuerySHA256 string `json:"query_sha256"`
	// Split is DERIV or HOLD for G-Q, empty elsewhere. Carried INTO the dump so
	// the score step never has to re-derive a partition from a seed and risk
	// scoring the hold-out half it was supposed to leave alone.
	Split string `json:"split,omitempty"`
	// Regime is local or global for G-REAL, empty elsewhere (wave X-W0b). It is
	// the ONE field the DUMP does not write: the X-W0 labels are a later
	// artefact than the dumps they stratify, so the offline steps stamp it from
	// the label file (regime.go) instead of re-measuring it. Empty means no
	// split was asked for, and the report is then the one it always was.
	Regime  string   `json:"regime,omitempty"`
	GoldIDs []string `json:"gold_ids,omitempty"`

	Rows        []rrf.ArmRow `json:"rows"`
	FusionOrder []string     `json:"fusion_order"`
	Delivered   []Delivered  `json:"delivered"`

	EffectiveQuery       string   `json:"effective_query"`
	EffectiveQuerySpaced string   `json:"effective_query_spaced"`
	EffectiveTemporal    string   `json:"effective_temporal"`
	EmbedModel           string   `json:"embed_model"`
	EmbedCacheHit        bool     `json:"embed_cache_hit"`
	Selector             Selector `json:"selector"`

	// Attempts counts the HTTP attempts this query cost, 1 when it worked the
	// first time. A record only exists for a query that eventually succeeded;
	// the ones that did not are in DumpStamp.Excluded.
	Attempts  int   `json:"attempts"`
	LatencyMS int64 `json:"latency_ms"`
}

// Key identifies a case across dumps and pin files: slice + index + the query
// digest. The digest is in the key on purpose — index alone would silently
// re-point at a different query if a slice were ever regenerated.
func (r Record) Key() string { return CaseKey(r.Slice, r.Index, r.QuerySHA256) }

// CaseKey builds the cross-artefact case key. The key format is shared with the
// judgement tooling of B-W6, so it has exactly one definition
// (goldset.CaseKey) and this is the driver-side name for it.
func CaseKey(slice string, index int, sha string) string {
	return goldset.CaseKey(slice, index, sha)
}

// ShortSHA is the digest prefix a report may carry. Full texts never leave the
// gold directory (§4.5), so a report cites a case as slice + index + prefix.
func ShortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ExcludedCase is a query the dump does NOT contain: it exhausted its retry
// budget. It is listed, never replaced — substituting a different query would
// change the case set a report is computed over without saying so.
type ExcludedCase struct {
	Slice       string `json:"slice"`
	Index       int    `json:"index"`
	QuerySHA256 string `json:"query_sha256"`
	Attempts    int    `json:"attempts"`
	Reason      string `json:"reason"`
}

// Key is the cross-artefact case key of an exclusion.
func (e ExcludedCase) Key() string { return CaseKey(e.Slice, e.Index, e.QuerySHA256) }

// Latency is the wall-clock profile of a run, in milliseconds. p50/p95 rather
// than a mean: the tail is what decides whether a 650-query dump fits an
// off-peak window, and a mean hides it.
type Latency struct {
	N   int   `json:"n"`
	P50 int64 `json:"p50_ms"`
	P95 int64 `json:"p95_ms"`
	Max int64 `json:"max_ms"`
}

// SummariseLatency computes the profile from per-query wall times.
func SummariseLatency(ms []int64) Latency {
	if len(ms) == 0 {
		return Latency{}
	}
	s := append([]int64(nil), ms...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return Latency{N: len(s), P50: s[percentileIdx(0.50, len(s))], P95: s[percentileIdx(0.95, len(s))], Max: s[len(s)-1]}
}

// percentileIdx is the nearest-rank index, clamped — the same convention
// evalscore.percentileIndex uses, so a p95 in a sweep report and a p95 in an
// eval report are read off the same rule.
func percentileIdx(q float64, n int) int {
	idx := int(q*float64(n)+0.5) - 1
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}

// WriteRecords writes dump records as JSONL at mode 0600, sorted by case key so
// two runs over the same case set produce the same file regardless of the
// completion order of concurrent workers.
//
// A path ending in GzipSuffix is written compressed (design/05 §6.1, M-W3d):
// the suffix decides, never a flag, so an artefact's name cannot disagree with
// its content.
func WriteRecords(path string, recs []Record) error {
	sorted := append([]Record(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key() < sorted[j].Key() })

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w, err := NewRecordWriter(f, path)
	if err != nil {
		return err
	}
	for i := range sorted {
		if err := w.Write(sorted[i]); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return f.Close()
}

// ReadRecords loads a dump file whole. A compressed artefact is read through
// the record stream instead — the same bytes, without a second gzip path.
//
// Whole-file loading is right for `score`, which re-fuses one dump under 16
// configurations and therefore needs every record repeatedly; `compare` pairs
// four dumps once and streams (compare.go).
func ReadRecords(path string) ([]Record, error) {
	if strings.HasSuffix(path, GzipSuffix) {
		return readRecordsStreamed(path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Record
	for n, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// WriteJSONFile persists any artefact as indented JSON at mode 0600.
func WriteJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), fileMode)
}

// ReadJSONFile loads an artefact written by WriteJSONFile.
func ReadJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
