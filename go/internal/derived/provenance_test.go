package derived

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestProvenanceDecodeIsFailClosed — an unknown contract version is refused,
// not best-effort parsed (§3.2). A missing v decodes to 0 and hits the same
// branch, which is the point: metadata that never carried the contract is not
// a v=1 provenance with defaults, it is not a provenance.
func TestProvenanceDecodeIsFailClosed(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"future version", `{"v":2,"stratum":1}`},
		{"missing version", `{"stratum":1}`},
		{"zero version", `{"v":0,"stratum":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p Provenance
			err := json.Unmarshal([]byte(c.raw), &p)
			if !errors.Is(err, ErrContractVersion) {
				t.Fatalf("decode of %s: err = %v, want ErrContractVersion", c.name, err)
			}
		})
	}
}

// TestProvenanceMarshalIsFailClosed — the refusal holds in the write direction
// too: a struct built against another contract must not reach the database
// wearing this build's field names.
func TestProvenanceMarshalIsFailClosed(t *testing.T) {
	p := validProvenance(3)
	p.V = 2
	if _, err := json.Marshal(p); err == nil {
		t.Fatal("Marshal accepted an unknown contract version")
	}
}

// TestProvenanceRoundTrip — every field of §3.2 survives the round trip, so a
// regeneration picker reading the stored metadata sees what the writer meant.
func TestProvenanceRoundTrip(t *testing.T) {
	in := validProvenance(4)
	in.Coverage.Rejects["g3"] = 4
	in.Coverage.Truncated = true
	in.UntrustedSources = 2

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Provenance
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !out.GeneratedAt.Equal(in.GeneratedAt) {
		t.Errorf("generated_at drifted: %v vs %v", out.GeneratedAt, in.GeneratedAt)
	}
	out.GeneratedAt = in.GeneratedAt
	b2, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(b) != string(b2) {
		t.Errorf("round trip is not stable:\n%s\n%s", b, b2)
	}
}

// TestProvenanceWireKeys pins the JSON key names against §3.2. They are the
// contract a SQL query filters on (metadata->'provenance'->>'stratum'), so a
// rename here is a silent break of every consumer.
func TestProvenanceWireKeys(t *testing.T) {
	b, err := json.Marshal(validProvenance(3))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []string{
		"v", "stratum", "arm", "source_block_ids", "source_count", "source_digest",
		"anchor", "generated_at", "generator", "coverage", "untrusted_sources",
		"sensitivity_max",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("provenance is missing key %q", k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("provenance carries %d keys, §3.2 names %d", len(m), len(want))
	}

	var anchor map[string]json.RawMessage
	if err := json.Unmarshal(m["anchor"], &anchor); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	for _, k := range []string{
		"kind", "topic_id", "core_hash", "attempts", "next_attempt_at",
		"block_id", "root_session_id", "manifest_id", "watermark_from",
	} {
		if _, ok := anchor[k]; !ok {
			t.Errorf("anchor is missing key %q", k)
		}
	}
}

// TestSourceDigestIsOrderIndependent — the digest is the O(1) prefilter for
// "has the source set changed?", so it must depend on the SET and not on the
// order a caller happens to hold it in.
func TestSourceDigestIsOrderIndependent(t *testing.T) {
	a := []string{srcID(2), srcID(0), srcID(1)}
	b := []string{srcID(0), srcID(1), srcID(2)}
	if SourceDigest(a) != SourceDigest(b) {
		t.Error("SourceDigest depends on input order")
	}
	if SourceDigest(a) == SourceDigest(append(b, srcID(3))) {
		t.Error("SourceDigest ignored an added source")
	}
	if !strings.HasPrefix(SourceDigest(b), "sha256:") {
		t.Errorf("SourceDigest = %q, want a sha256: prefix", SourceDigest(b))
	}
	before := []string{srcID(2), srcID(0), srcID(1)}
	SourceDigest(before)
	if before[0] != srcID(2) {
		t.Error("SourceDigest sorted the caller's slice in place")
	}
}
