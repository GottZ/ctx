//go:build integration

// W05.4 envelope gate (design/05 §7 W05.4): the ego wire envelope must carry a
// `budget_report` that differentiates the ONE existing `truncated` bool by
// CAUSE and by LAYER. Red against the Ist: the envelope has no budget_report
// field at all, only stats.truncated.
//
// The layer assertion is the design leistung of §4.5: a node cut caused by
// p.Limit is an API-CONTRACT trip (`node_limit_reached`, layer LIMITS), NOT a
// server budget trip (`visited_capped`, layer BUDGETS) — p.Limit is what the
// client asked for, MaxVisited is what the server is willing to pay.

package handler

import (
	"encoding/json"
	"testing"
)

// hgBudgetReport decodes the ego envelope generically and returns the
// budget_report object. Generic decoding (map, not the typed envelope) is
// deliberate: it is the RED-capable shape — against the Ist the key is simply
// absent, which a typed struct would silently zero-value away.
func hgBudgetReport(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	raw, ok := env["budget_report"]
	if !ok {
		t.Fatalf("envelope has NO budget_report field (red against Ist) — keys: %v", envKeys(env))
	}
	rep, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("budget_report is %T, want object", raw)
	}
	return rep
}

func envKeys(env map[string]any) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	return out
}

// hgHasClass reports whether the given budget_report layer array carries the
// class token.
func hgHasClass(t *testing.T, rep map[string]any, layer, class string) bool {
	t.Helper()
	arr, ok := rep[layer].([]any)
	if !ok {
		t.Fatalf("budget_report.%s missing or not an array: %#v", layer, rep[layer])
	}
	for _, v := range arr {
		if s, _ := v.(string); s == class {
			return true
		}
	}
	return false
}

// TestHandleEgo_BudgetReportNodeLimit is the W05.4 envelope gate: an ego
// request with an artificially small limit trips the node budget; the envelope
// must declare `node_limit_reached` in the LIMITS layer — and must NOT declare
// `visited_capped` (that would be the server-budget layer, a different claim).
func TestHandleEgo_BudgetReportNodeLimit(t *testing.T) {
	pool := hgSetup(t)

	// limit=1: the focus alone fills the budget, the neighbor frontier stays
	// unexpanded ⇒ node-limit truncation.
	rec := hgDo(t, pool, hgAuth(hgKeyA, "private"), "block="+hgShared+"&hops=1&limit=1")
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Stats struct {
			Truncated bool `json:"truncated"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if !env.Stats.Truncated {
		t.Fatalf("setup: stats.truncated = false, expected a truncated response at limit=1")
	}

	rep := hgBudgetReport(t, rec.Body.Bytes())
	if !hgHasClass(t, rep, "limits", "node_limit_reached") {
		t.Errorf("budget_report.limits = %v, want node_limit_reached", rep["limits"])
	}
	if hgHasClass(t, rep, "budgets", "visited_capped") {
		t.Errorf("budget_report.budgets carries visited_capped — p.Limit is an API contract (LIMITS), not a server budget: %v", rep["budgets"])
	}
	if src, _ := rep["source"].(string); src != "sql" {
		t.Errorf("budget_report.source = %q, want \"sql\" (no cache arm before W05.5)", src)
	}
}

// TestHandleEgo_BudgetReportEdgeLimit is the edge-side half of the envelope
// gate: an edge_limit=1 request on a fixture with an induced edge trips the
// edge budget ⇒ `edge_limit_reached` in the LIMITS layer.
func TestHandleEgo_BudgetReportEdgeLimit(t *testing.T) {
	pool := hgSetup(t)

	// Two dream edges between the two shared blocks, edge_limit=1 ⇒ the induced
	// edge resolution reads one row beyond the budget and truncates.
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid, $2::uuid, 'causal', 0.9, 0.9, 'shared', 5)`,
		hgShared2, hgShared,
	); err != nil {
		t.Fatalf("insert second link: %v", err)
	}

	rec := hgDo(t, pool, hgAuth(hgKeyA, "private"), "block="+hgShared+"&hops=1&edge_limit=1")
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rep := hgBudgetReport(t, rec.Body.Bytes())
	if !hgHasClass(t, rep, "limits", "edge_limit_reached") {
		t.Errorf("budget_report.limits = %v, want edge_limit_reached", rep["limits"])
	}
	if hgHasClass(t, rep, "limits", "node_limit_reached") {
		t.Errorf("budget_report.limits carries node_limit_reached on a pure edge cut: %v", rep["limits"])
	}
}

// TestHandleEgo_BudgetReportUntruncated pins the quiet case: a request that
// trips nothing still carries the (empty) report — the field is unconditional,
// so a client never has to distinguish "absent" from "no trips".
func TestHandleEgo_BudgetReportUntruncated(t *testing.T) {
	pool := hgSetup(t)

	rec := hgDo(t, pool, hgAuth(hgKeyA, "private"), "block="+hgShared+"&hops=1")
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rep := hgBudgetReport(t, rec.Body.Bytes())
	if arr, ok := rep["limits"].([]any); !ok || len(arr) != 0 {
		t.Errorf("budget_report.limits = %#v, want empty array", rep["limits"])
	}
	if arr, ok := rep["budgets"].([]any); !ok || len(arr) != 0 {
		t.Errorf("budget_report.budgets = %#v, want empty array", rep["budgets"])
	}
}
