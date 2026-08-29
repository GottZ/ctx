package armsweep

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/GottZ/ctx/internal/goldset"
)

// Pin freezes the two non-deterministic stages of the query path for one gold
// case (design 04 §4.4, the B-W2 seam): the translation result and the temporal
// FTS expansion. Both come out of an LLM on the live path, so an unpinned rerun
// of the same query is not the same measurement — and the whole point of a
// dump PAIR is that the two runs differ only by noise in the corpus, not by
// noise in the prompt.
//
// Temporal is a VALUE, not an optional: the empty string means "explicitly no
// temporal expansion" and suppresses the LLM fallback (query.go:748). Encoding
// it as a plain string rather than a pointer is what makes that unambiguous on
// the wire, and TemporalSet records that the pin was measured rather than
// defaulted.
type Pin struct {
	Slice       string `json:"slice"`
	Index       int    `json:"index"`
	QuerySHA256 string `json:"query_sha256"`
	Translation string `json:"pinned_translation"`
	Temporal    string `json:"pinned_temporal"`
	// QuerySpaced is the trigram-spaced form the priming run observed. It is
	// NOT sent back (the handler derives it from the translation); it travels
	// so a dump can prove the derivation did not move between the two runs.
	QuerySpaced string `json:"effective_query_spaced"`
	EmbedModel  string `json:"embed_model"`
}

// Key is the cross-artefact case key.
func (p Pin) Key() string { return CaseKey(p.Slice, p.Index, p.QuerySHA256) }

// PrimeStamp is the provenance of one priming run: what the pins were measured
// against. A dump that used pins from a different model or a different endpoint
// is a different measurement, and the score step must be able to see that
// without being told.
type PrimeStamp struct {
	RunID      string   `json:"run_id"`
	CreatedAt  string   `json:"created_at"`
	BaseURL    string   `json:"base_url"`
	EmbedModel string   `json:"embed_model"`
	Slices     []string `json:"slices"`
	Pins       int      `json:"pins"`
	PinFile    string   `json:"pin_file"`
	PinSHA256  string   `json:"pin_sha256"`
	// PoolFile holds the per-arm top-20 candidates of the unlabelled slices
	// G-REAL and — since wave C4-3a — G-GLOB: the pooling input waves B-W6 and
	// C3-4b turn into relevance judgements. One file for both, keyed by case,
	// so a consumer selects a slice by reading the entries rather than by
	// being handed a different file.
	PoolFile     string         `json:"pool_file,omitempty"`
	Excluded     []ExcludedCase `json:"excluded"`
	Latency      Latency        `json:"latency"`
	EmbedWarmed  int            `json:"embed_cache_misses"`
	TemporalHits int            `json:"temporal_queries"`
}

// PoolEntry is one arm's candidate list for a pooling judgement (§4.5; G-REAL,
// and G-GLOB since wave C4-3a). Top-20 per arm BY RANK — the union of four
// solo-arm heads is the standard pooling construction, and taking it per arm
// rather than from the fused order is what keeps the pool from inheriting the
// very weighting under test.
//
// The definition moved to internal/goldset in wave B-W6, where the judgement
// template that consumes this file is built. The dependency runs armsweep ->
// goldset and cannot run back, so an alias is what keeps ONE struct for the
// format both waves write and read.
type PoolEntry = goldset.PoolEntry

// PoolDepth is the per-arm pooling depth.
const PoolDepth = goldset.PoolDepth

// WritePins persists a pin file as JSONL, sorted by case key.
func WritePins(path string, pins []Pin) error {
	sorted := append([]Pin(nil), pins...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key() < sorted[j].Key() })
	return writeJSONL(path, len(sorted), func(enc *json.Encoder, i int) error { return enc.Encode(sorted[i]) })
}

// ReadPins loads a pin file into a key-indexed map. A duplicate key is refused
// rather than last-write-wins: two pins for one case mean two priming runs got
// concatenated, and silently picking one of them would pin a measurement to an
// arbitrary half of a mixed file.
func ReadPins(path string) (map[string]Pin, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]Pin{}
	for n, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p Pin
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		if _, dup := out[p.Key()]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate pin for case %s", path, n+1, p.Key())
		}
		out[p.Key()] = p
	}
	return out, nil
}

// WritePool persists the pooling file as JSONL, sorted by case key.
func WritePool(path string, entries []PoolEntry) error {
	sorted := append([]PoolEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return CaseKey(sorted[i].Slice, sorted[i].Index, sorted[i].QuerySHA256) <
			CaseKey(sorted[j].Slice, sorted[j].Index, sorted[j].QuerySHA256)
	})
	return writeJSONL(path, len(sorted), func(enc *json.Encoder, i int) error { return enc.Encode(sorted[i]) })
}

// ReadPool loads a pooling file. One reader, in the package that owns the
// format — the judgement tooling of B-W6 must not be able to disagree with the
// driver about what a pool file says.
func ReadPool(path string) ([]PoolEntry, error) { return goldset.ReadPool(path) }

// writeJSONL is the shared 0600 JSONL writer.
func writeJSONL(path string, n int, encode func(*json.Encoder, int) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := 0; i < n; i++ {
		if err := encode(enc, i); err != nil {
			return fmt.Errorf("encode line %d: %w", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}
