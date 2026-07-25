package handler

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func egoQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query %q: %v", raw, err)
	}
	return v
}

const egoTestUUID = "019c9629-faac-75a5-98b6-de04dbfc8404"

// Defaults: only block set → hops=1, per_node_cap=25, limit=500,
// edge_limit=4000, min_confidence=0, all filters nil.
func TestParseEgoParams_Defaults(t *testing.T) {
	p, _, err := parseEgoParams(egoQuery(t, "block="+egoTestUUID), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Focus != egoTestUUID {
		t.Errorf("Focus = %q", p.Focus)
	}
	if p.Hops != 1 || p.PerNodeCap != 25 || p.Limit != 500 || p.EdgeLimit != 4000 {
		t.Errorf("defaults wrong: hops=%d cap=%d limit=%d edge_limit=%d", p.Hops, p.PerNodeCap, p.Limit, p.EdgeLimit)
	}
	if p.MinConfidence != 0 {
		t.Errorf("MinConfidence = %v, want 0", p.MinConfidence)
	}
	if p.LinkClasses != nil || p.Categories != nil || p.CreatedAfter != nil || p.CreatedBefore != nil {
		t.Error("optional filters must default to nil")
	}
}

// Ceilings are enforced as 400 (error), never silently clamped.
func TestParseEgoParams_Ceilings(t *testing.T) {
	bad := []string{
		"",                 // block missing
		"block=not-a-uuid", // malformed focus
		"block=" + egoTestUUID + "&hops=0",
		"block=" + egoTestUUID + "&hops=4",
		"block=" + egoTestUUID + "&hops=abc",
		"block=" + egoTestUUID + "&per_node_cap=0",
		"block=" + egoTestUUID + "&per_node_cap=101",
		"block=" + egoTestUUID + "&limit=0",
		"block=" + egoTestUUID + "&limit=9999",
		"block=" + egoTestUUID + "&edge_limit=0",
		"block=" + egoTestUUID + "&edge_limit=20001",
		"block=" + egoTestUUID + "&min_confidence=-0.1",
		"block=" + egoTestUUID + "&min_confidence=1.5",
		"block=" + egoTestUUID + "&min_confidence=abc",
		"block=" + egoTestUUID + "&link_class=topical,unknown",
		"block=" + egoTestUUID + "&created_after=gestern",
		"block=" + egoTestUUID + "&created_before=2026-13-45",
	}
	for _, raw := range bad {
		if _, _, err := parseEgoParams(egoQuery(t, raw), nil); err == nil {
			t.Errorf("query %q: expected error, got nil", raw)
		}
	}

	// Boundary values are valid. limit max = 1500 (G39: lowered from 5000 — the
	// 1M bench proved larger ceilings indefensible under the 500ms p95 bar).
	good := "block=" + egoTestUUID + "&hops=3&per_node_cap=100&limit=1500&edge_limit=20000&min_confidence=1"
	p, _, err := parseEgoParams(egoQuery(t, good), nil)
	if err != nil {
		t.Fatalf("boundary values rejected: %v", err)
	}
	if p.Hops != 3 || p.PerNodeCap != 100 || p.Limit != 1500 || p.EdgeLimit != 20000 || p.MinConfidence != 1 {
		t.Errorf("boundary parse wrong: %+v", p)
	}
	// limit just past the ceiling is a 400, never clamped.
	if _, _, err := parseEgoParams(egoQuery(t, "block="+egoTestUUID+"&limit=1501"), nil); err == nil {
		t.Error("limit=1501 must be rejected (ceiling is 1500)")
	}
}

// link_class CSV: all five classes are valid, whitespace tolerated, unknown
// rejected; category CSV is free-form.
func TestParseEgoParams_CSVFilters(t *testing.T) {
	p, _, err := parseEgoParams(egoQuery(t, "block="+egoTestUUID+"&link_class=topical,%20causal,supersedes&category=learnings,decisions"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.LinkClasses) != 3 || p.LinkClasses[1] != "causal" {
		t.Errorf("LinkClasses = %v", p.LinkClasses)
	}
	if len(p.Categories) != 2 || p.Categories[0] != "learnings" {
		t.Errorf("Categories = %v", p.Categories)
	}
}

// GB5 (design/02 §4.2): the link_class CSV partitions by vocabulary — dream
// names → LinkClasses, registry structural names → StructClasses; a set param
// makes BOTH sides non-nil (an empty side matches nothing on its class).
// RED on the pre-GB5 state: link_class=references was a 400.
func TestParseEgoParams_LinkClassPartition(t *testing.T) {
	vocab := []string{"references", "duplicate-of"}

	// The former 400 pin, inverted: a structural class is now valid.
	p, raw, err := parseEgoParams(egoQuery(t, "block="+egoTestUUID+"&link_class=references"), vocab)
	if err != nil {
		t.Fatalf("link_class=references rejected (pre-GB5 400 behavior): %v", err)
	}
	if p.LinkClasses == nil || len(p.LinkClasses) != 0 {
		t.Errorf("dream side = %#v, want non-nil EMPTY (references is structural-only)", p.LinkClasses)
	}
	if len(p.StructClasses) != 1 || p.StructClasses[0] != "references" {
		t.Errorf("struct side = %#v, want [references]", p.StructClasses)
	}
	if len(raw) != 1 || raw[0] != "references" {
		t.Errorf("raw echo = %#v, want [references]", raw)
	}

	// Mixed CSV splits by vocabulary, echo keeps request order.
	p, raw, err = parseEgoParams(egoQuery(t, "block="+egoTestUUID+"&link_class=references,topical,duplicate-of"), vocab)
	if err != nil {
		t.Fatalf("mixed CSV rejected: %v", err)
	}
	if len(p.LinkClasses) != 1 || p.LinkClasses[0] != "topical" {
		t.Errorf("dream side = %#v, want [topical]", p.LinkClasses)
	}
	if len(p.StructClasses) != 2 {
		t.Errorf("struct side = %#v, want [references duplicate-of]", p.StructClasses)
	}
	if len(raw) != 3 || raw[1] != "topical" {
		t.Errorf("raw echo = %#v, want request order", raw)
	}

	// Absent param: both sides nil (= all classes, default visibility).
	p, raw, err = parseEgoParams(egoQuery(t, "block="+egoTestUUID), vocab)
	if err != nil {
		t.Fatalf("absent link_class: %v", err)
	}
	if p.LinkClasses != nil || p.StructClasses != nil || raw != nil {
		t.Errorf("absent param: dream=%#v struct=%#v raw=%#v, want all nil", p.LinkClasses, p.StructClasses, raw)
	}

	// Unknown class stays a 400 and names the FULL vocabulary.
	_, _, err = parseEgoParams(egoQuery(t, "block="+egoTestUUID+"&link_class=bogus"), vocab)
	if err == nil {
		t.Fatal("unknown class accepted")
	}
	for _, part := range []string{"topical", "references"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("400 message lacks vocabulary part %q: %v", part, err)
		}
	}
}

// created_after / created_before parse RFC3339 into the params.
func TestParseEgoParams_CreatedWindow(t *testing.T) {
	p, _, err := parseEgoParams(egoQuery(t, "block="+egoTestUUID+"&created_after=2026-01-01T00:00:00Z&created_before=2026-06-01T12:30:00%2B02:00"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.CreatedAfter == nil || !p.CreatedAfter.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAfter = %v", p.CreatedAfter)
	}
	if p.CreatedBefore == nil {
		t.Fatal("CreatedBefore nil")
	}
}

// ENVELOPE GOLDEN (design §5.1): the marshaled egoResponse matches the §3.1
// wire example byte-for-byte in field names, nesting and tuple encoding.
// T19 and the client type against exactly this shape — store/wire drift
// breaks here first.
func TestEgoResponse_EnvelopeGolden(t *testing.T) {
	res := &store.EgoResult{
		Focus: egoTestUUID,
		Rels:  store.GraphRels,
		Nodes: []store.GraphNode{
			{
				ID: egoTestUUID, Title: "Hub", Category: "infrastructure",
				Scope: "private", Degree: 18, Hop: 0,
				CreatedAt: time.Date(2026, 2, 23, 11, 2, 41, 0, time.UTC),
			},
			{
				ID: "019c5ea2-0000-7000-9000-000000000001", Title: "Neighbor",
				Category: "learnings", Scope: "private", Degree: 17, Hop: 1,
				CreatedAt: time.Date(2026, 2, 12, 8, 0, 0, 0, time.UTC),
			},
		},
		Edges:     []store.GraphEdge{{Src: 0, Dst: 1, Rel: 0, Conf: 0.83}},
		Truncated: false,
	}
	p := store.EgoParams{Hops: 2, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}

	got, err := json.Marshal(buildEgoResponse(res, p, nil, 4))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	// GA8 (design/02 §4.1): the structural envelope fields ship EMPTY but
	// unconditional — struct_rels/origins after rels, structural_edges after
	// edges, stats.structural_edges between edges and truncated. stats.edges
	// stays dream-only (E4).
	want := `{` +
		`"success":true,` +
		`"focus":"019c9629-faac-75a5-98b6-de04dbfc8404",` +
		`"params":{"hops":2,"per_node_cap":25,"limit":500,"min_confidence":0,"link_class":null,"edge_limit":4000},` +
		`"rels":["topical","factual","causal","recurrent","supersedes"],` +
		`"struct_rels":[],` +
		`"origins":[],` +
		`"nodes":[` +
		`{"id":"019c9629-faac-75a5-98b6-de04dbfc8404","title":"Hub","category":"infrastructure","scope":"private","degree":18,"hop":0,"created_at":"2026-02-23T11:02:41Z"},` +
		`{"id":"019c5ea2-0000-7000-9000-000000000001","title":"Neighbor","category":"learnings","scope":"private","degree":17,"hop":1,"created_at":"2026-02-12T08:00:00Z"}` +
		`],` +
		`"edges":[[0,1,0,0.830]],` +
		`"structural_edges":[],` +
		`"stats":{"nodes":2,"edges":1,"structural_edges":0,"truncated":false,"elapsed_ms":4},` +
		// W05.4 (design/05 §4.5): budget_report is the LAST key, ADDITIVE after
		// stats — stats.truncated keeps its exact old meaning, the report
		// differentiates it by cause/layer. A nil store report (this hand-built
		// EgoResult) renders as the empty SQL-arm report, so the key is
		// unconditional: no client has to distinguish absent from empty.
		`"budget_report":{"limits":[],"budgets":[],"counts":{},"source":"sql","cache_age_ms":0}` +
		`}`

	if string(got) != want {
		t.Errorf("envelope drift:\n got %s\nwant %s", got, want)
	}
}

// Empty result sets marshal as [] (never null) — the client types against
// arrays unconditionally.
func TestEgoResponse_EmptyCollectionsAreArrays(t *testing.T) {
	res := &store.EgoResult{Focus: egoTestUUID, Rels: store.GraphRels}
	p := store.EgoParams{Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}

	got, err := json.Marshal(buildEgoResponse(res, p, nil, 1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(got)
	if strings.Contains(s, `"nodes":null`) || strings.Contains(s, `"edges":null`) {
		t.Errorf("empty collections must marshal as [], got %s", s)
	}
	// GA8: the three structural collections are arrays unconditionally too —
	// old clients destructure prefixes, null would throw in the FE merge loop.
	for _, f := range []string{`"struct_rels":null`, `"origins":null`, `"structural_edges":null`} {
		if strings.Contains(s, f) {
			t.Errorf("structural collection marshaled as null (%s), want []: %s", f, s)
		}
	}
	for _, f := range []string{`"struct_rels":[]`, `"origins":[]`, `"structural_edges":[]`} {
		if !strings.Contains(s, f) {
			t.Errorf("structural collection %s missing from empty envelope: %s", f, s)
		}
	}
}

// parseAllParams (load-all): no block/hops/per_node_cap; limit AND edge_limit
// default to their ceilings (the button's whole point is "everything the
// proven envelope allows"); the shared filter/partition pieces behave exactly
// like ego.
func TestParseAllParams(t *testing.T) {
	t.Run("Defaults", func(t *testing.T) {
		p, raw, err := parseAllParams(egoQuery(t, ""), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Limit != egoMaxLimit || p.EdgeLimit != egoMaxEdgeLimit {
			t.Errorf("defaults wrong: limit=%d edge_limit=%d (want ceilings %d/%d)", p.Limit, p.EdgeLimit, egoMaxLimit, egoMaxEdgeLimit)
		}
		if p.Focus != "" || p.Hops != 0 || p.PerNodeCap != 0 {
			t.Errorf("traversal params must stay zero: %+v", p)
		}
		if raw != nil || p.LinkClasses != nil || p.Categories != nil {
			t.Error("optional filters must default to nil")
		}
	})

	t.Run("CeilingsNotClamped", func(t *testing.T) {
		bad := []string{
			"limit=0", "limit=1501",
			"edge_limit=0", "edge_limit=20001",
			"min_confidence=1.5",
			"link_class=topical,unknown",
			"created_after=gestern",
		}
		for _, raw := range bad {
			if _, _, err := parseAllParams(egoQuery(t, raw), nil); err == nil {
				t.Errorf("query %q: expected error, got nil", raw)
			}
		}
	})

	t.Run("SharedFilterPieces", func(t *testing.T) {
		p, raw, err := parseAllParams(
			egoQuery(t, "limit=10&min_confidence=0.5&link_class=topical,references&category=infra"),
			[]string{"references"},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Limit != 10 || p.MinConfidence != 0.5 {
			t.Errorf("scalars wrong: %+v", p)
		}
		if len(p.LinkClasses) != 1 || p.LinkClasses[0] != "topical" ||
			len(p.StructClasses) != 1 || p.StructClasses[0] != "references" {
			t.Errorf("link_class partition wrong: dream=%v struct=%v", p.LinkClasses, p.StructClasses)
		}
		if len(raw) != 2 || len(p.Categories) != 1 || p.Categories[0] != "infra" {
			t.Errorf("raw echo / category wrong: raw=%v cat=%v", raw, p.Categories)
		}
	})
}
