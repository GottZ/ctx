package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
)

// sampleSnapshot is a two-target dispatch snapshot: herbert-chat carries an OWN
// bucket ("private") and a FOREIGN bucket ("acme-secret") plus preempt counters;
// a second, tenant-INVISIBLE origin exists to prove the visibility filter.
func sampleSnapshot() dispatch.Snapshot {
	return dispatch.Snapshot{
		Enabled:         true,
		Demand:          2,
		ReapsTotal:      3,
		ClassDowngrades: 1,
		UnchargedCalls:  4,
		OpsTotal:        99,
		MaxOpDur:        7 * time.Millisecond,
		Targets: []dispatch.TargetSnapshot{
			{
				Origin:            "http://herbert:8089",
				Slots:             1,
				PreemptBackground: true,
				HeraldScope:       dispatch.HeraldGlobal,
				Held:              1,
				Inflight:          1,
				Interactive:       dispatch.WaitStats{Waiting: 2, OldestWait: 500 * time.Millisecond, P95Wait: 400 * time.Millisecond, MaxWait: 600 * time.Millisecond, Samples: 10},
				Background:        dispatch.WaitStats{Waiting: 3},
				Preempt:           dispatch.PreemptStats{PreemptsTotal: 5, WastedMsTotal: 20, AgedAdmitsTotal: 2, AgedPreemptsTotal: 1},
				Buckets: []dispatch.BucketSnapshot{
					{FairKey: "private", Waiting: 1, OldestWait: 300 * time.Millisecond, Inflight: 1, Tokens: 111, Charges: 2},
					{FairKey: "acme-secret", Waiting: 4, OldestWait: 900 * time.Millisecond, Inflight: 2, Tokens: 999, Charges: 9},
				},
			},
			{
				Origin: "http://sidecar:9099",
				Slots:  2,
				Held:   0,
			},
		},
	}
}

// TestCoarsenDepth pins the E-A5-6(b) mapping: only a monotone leer/niedrig/hoch
// bucketing, never a live count.
func TestCoarsenDepth(t *testing.T) {
	cases := []struct {
		waiting int
		want    string
	}{
		{-1, "leer"}, {0, "leer"}, {1, "niedrig"}, {3, "niedrig"}, {4, "hoch"}, {100, "hoch"},
	}
	for _, c := range cases {
		if got := coarsenDepth(c.waiting); got != c.want {
			t.Errorf("coarsenDepth(%d) = %q, want %q", c.waiting, got, c.want)
		}
	}
}

// TestBuildDispatchTenantNoForeignPrincipal is the F-B3 negative probe: a
// tenant-bound view must expose the caller's OWN bucket detail but NEVER a
// foreign fairKey / principal detail. The whole marshalled payload is scanned
// for the foreign key string.
func TestBuildDispatchTenantNoForeignPrincipal(t *testing.T) {
	snap := sampleSnapshot()
	visible := map[string]bool{"http://herbert:8089": true} // sidecar NOT visible
	ts := buildDispatchTenant(snap, visible, "private")

	if len(ts.Targets) != 1 {
		t.Fatalf("want 1 visible target, got %d (sidecar must be filtered out)", len(ts.Targets))
	}
	tt := ts.Targets[0]
	if tt.Origin != "http://herbert:8089" {
		t.Fatalf("unexpected origin %q", tt.Origin)
	}
	// Own bucket ("private"): detail present.
	if tt.OwnWaiting != 1 || tt.OwnInflight != 1 || tt.OwnOldestWaitMs != 300 {
		t.Errorf("own detail wrong: waiting=%d inflight=%d oldest=%d", tt.OwnWaiting, tt.OwnInflight, tt.OwnOldestWaitMs)
	}
	// Occupancy aggregate: busy true, total waitQ 2+3=5 ⇒ hoch (anonymous).
	if !tt.Busy || tt.Depth != "hoch" {
		t.Errorf("occupancy wrong: busy=%v depth=%q", tt.Busy, tt.Depth)
	}

	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(b)
	// F-B3: no foreign fairKey, no foreign token/charge leak, no "fair_key" field.
	for _, forbidden := range []string{"acme-secret", "fair_key", "999", "sidecar"} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("F-B3 leak: tenant payload contains %q\n%s", forbidden, payload)
		}
	}
}

// TestBuildDispatchTenantNoOwnBucketWhenScopeEmpty proves the empty-scope caller
// (no HomeScope) never matches any bucket — no own detail, still anonymous
// occupancy.
func TestBuildDispatchTenantNoOwnBucketWhenScopeEmpty(t *testing.T) {
	snap := sampleSnapshot()
	visible := map[string]bool{"http://herbert:8089": true}
	ts := buildDispatchTenant(snap, visible, "")
	tt := ts.Targets[0]
	if tt.OwnWaiting != 0 || tt.OwnInflight != 0 || tt.OwnOldestWaitMs != 0 {
		t.Errorf("empty scope must expose no own bucket, got %+v", tt)
	}
	b, _ := json.Marshal(ts)
	if strings.Contains(string(b), "acme-secret") {
		t.Errorf("F-B3 leak with empty scope: %s", b)
	}
}

// TestBuildDispatchAdminExposesFullDetail is the positive exposure probe: the
// server-admin view DOES carry the foreign fairKey, preempt/aged counters,
// class downgrades and enforcing — everything the tenant view withholds.
func TestBuildDispatchAdminExposesFullDetail(t *testing.T) {
	snap := sampleSnapshot()
	guard := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ds := buildDispatchAdmin(snap, true, &guard, nil, nil, nil)

	if !ds.Enforcing || ds.ClassDowngrades != 1 || ds.UnchargedCalls != 4 || ds.MaxOpMs != 7 {
		t.Errorf("admin scalar detail wrong: %+v", ds)
	}
	if ds.LastGuardAt == nil || !ds.LastGuardAt.Equal(guard) || ds.LastDigestAt != nil {
		t.Errorf("last-run stamps wrong: guard=%v digest=%v", ds.LastGuardAt, ds.LastDigestAt)
	}
	if len(ds.Targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(ds.Targets))
	}
	tgt := ds.Targets[0]
	if tgt.Preempt.PreemptsTotal != 5 || tgt.Preempt.AgedAdmitsTotal != 2 || tgt.Preempt.AgedPreemptsTotal != 1 {
		t.Errorf("preempt/aged counters missing: %+v", tgt.Preempt)
	}
	if tgt.Interactive.P95WaitMs != 400 || tgt.Interactive.MaxWaitMs != 600 || tgt.Interactive.Samples != 10 {
		t.Errorf("wait aggregates missing: %+v", tgt.Interactive)
	}
	// The foreign fairKey IS present on the admin side.
	b, _ := json.Marshal(ds)
	if !strings.Contains(string(b), "acme-secret") {
		t.Errorf("admin view must expose foreign fairKey; payload: %s", b)
	}
}

// TestEmbedTokensFromRollup is the D1(a) probe: the metric sums prompt_tokens of
// the embed pipelines per target, and excludes non-embed pipelines.
func TestEmbedTokensFromRollup(t *testing.T) {
	rollup := []llm24hRow{
		{Backend: "llama-embed", Pipeline: "query-embed", PromptTokens: 100},
		{Backend: "llama-embed", Pipeline: "embed-backfill", PromptTokens: 50},
		{Backend: "llama-embed", Pipeline: "query-rerank", PromptTokens: 9999}, // NOT embed
		{Backend: "herbert-chat", Pipeline: "query-synthesize", PromptTokens: 7}, // NOT embed
		{Backend: "openrouter", Pipeline: "embed-backfill", PromptTokens: 5},
	}
	got := embedTokensFromRollup(rollup)
	want := map[string]int64{"llama-embed": 150, "openrouter": 5}
	if len(got) != len(want) {
		t.Fatalf("want %d embed targets, got %d: %+v", len(want), len(got), got)
	}
	for _, et := range got {
		if want[et.Target] != et.PromptTokens {
			t.Errorf("embed tokens for %q = %d, want %d", et.Target, et.PromptTokens, want[et.Target])
		}
	}
}
