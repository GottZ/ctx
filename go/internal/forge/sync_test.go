package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// newGateManager builds a store-free manager for the gate probes that return
// BEFORE any store call (running / suspended / policy). newForge is a spy so the
// tests can assert "0 wire calls" when a gate refuses.
func newGateManager(ts TenantStatusFn, ip IssuePolicyFn, wireCalls *int) *SyncManager {
	return &SyncManager{
		runs:         map[string]*SyncStatus{},
		clock:        time.Now,
		tenantStatus: ts,
		issuePolicy:  ip,
		newForge: func(string) Forge {
			*wireCalls++
			return nil
		},
	}
}

var okTenant = func(context.Context, string) (string, bool, error) { return "active", true, nil }
var okPolicy = func(context.Context, string) (bool, string) { return true, "" }

// TestStartSync_DoubleStart409 is the S7 single-flight gate: a second start of the
// SAME running project ⇒ ErrSyncRunning (409). RED without run-state: both starts
// would launch. 0 wire calls (returns before the client factory).
func TestStartSync_DoubleStart409(t *testing.T) {
	wire := 0
	m := newGateManager(okTenant, okPolicy, &wire)
	m.runs["p1"] = &SyncStatus{ProjectID: "p1", Running: true} // a run is in flight
	_, err := m.StartSync(context.Background(), store.ProjectRow{ID: "p1", Scope: "t:main"}, false)
	if !errors.Is(err, ErrSyncRunning) {
		t.Fatalf("want ErrSyncRunning, got %v", err)
	}
	if wire != 0 {
		t.Fatalf("double-start made %d wire calls, want 0", wire)
	}
}

// TestStartSync_TenantSuspended: a suspended owning tenant ⇒ skip (ErrTenantSuspended),
// never proceed. 0 wire calls.
func TestStartSync_TenantSuspended(t *testing.T) {
	wire := 0
	ts := func(context.Context, string) (string, bool, error) { return "suspended", true, nil }
	m := newGateManager(ts, okPolicy, &wire)
	_, err := m.StartSync(context.Background(), store.ProjectRow{ID: "p1", Scope: "t:main"}, false)
	if !errors.Is(err, ErrTenantSuspended) {
		t.Fatalf("want ErrTenantSuspended, got %v", err)
	}
	if wire != 0 {
		t.Fatalf("suspended gate made %d wire calls, want 0", wire)
	}
}

// TestStartSync_IssuePolicyRefused is the §6.4 digest-flood gate: no resolvable
// issue policy ⇒ ErrIssuePolicy with a clear reason. RED without the gate: the run
// would proceed and (post-I-G) flood the topic map. 0 wire calls.
func TestStartSync_IssuePolicyRefused(t *testing.T) {
	wire := 0
	ip := func(context.Context, string) (bool, string) { return false, "issue type not registered" }
	m := newGateManager(okTenant, ip, &wire)
	_, err := m.StartSync(context.Background(), store.ProjectRow{ID: "p1", Scope: "t:main"}, false)
	if !errors.Is(err, ErrIssuePolicy) {
		t.Fatalf("want ErrIssuePolicy, got %v", err)
	}
	if !strings.Contains(err.Error(), "issue type not registered") {
		t.Fatalf("policy refusal should carry the reason, got %v", err)
	}
	if wire != 0 {
		t.Fatalf("policy gate made %d wire calls, want 0", wire)
	}
}

// TestExpBackoff verifies the exponential offline-first schedule capped at 1h.
func TestExpBackoff(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, time.Minute}, {1, 2 * time.Minute}, {2, 4 * time.Minute},
		{5, 32 * time.Minute}, {6, time.Hour}, {10, time.Hour}, {100, time.Hour},
	}
	for _, c := range cases {
		if got := expBackoff(c.n); got != c.want {
			t.Errorf("expBackoff(%d) = %s, want %s", c.n, got, c.want)
		}
	}
}

// TestSanitizeErr_NoLeak is the leak-scan unit line (§5.4): a wire error carrying
// a URL/token in its message is reduced to a fixed class string — the token/URL
// never reaches last_error / the response.
func TestSanitizeErr_NoLeak(t *testing.T) {
	leaky := errors.New("Get https://api.github.com/repos/o/r/issues: Bearer ghp_supersecret failed")
	if got := sanitizeErr(leaky); strings.Contains(got, "ghp_supersecret") || strings.Contains(got, "api.github.com") {
		t.Fatalf("sanitizeErr leaked material: %q", got)
	}
	if got := sanitizeErr(&RateLimitError{RetryAfter: time.Minute}); got != "rate limited" {
		t.Fatalf("rate-limit sanitize = %q", got)
	}
}

// TestRepoRefFromForge validates the forge-config parse + the github-only guard.
func TestRepoRefFromForge(t *testing.T) {
	if _, err := repoRefFromForge([]byte(`{"kind":"gitlab","owner":"o","repo":"r"}`)); !errors.Is(err, ErrForgeKind) {
		t.Fatalf("gitlab should be ErrForgeKind, got %v", err)
	}
	if _, err := repoRefFromForge([]byte(`{"kind":"github"}`)); err == nil {
		t.Fatal("missing owner/repo should error")
	}
	ref, err := repoRefFromForge([]byte(`{"kind":"github","owner":"o","repo":"r","api_base":"https://ghe.example/api/v3"}`))
	if err != nil || ref.Owner != "o" || ref.APIBase != "https://ghe.example/api/v3" {
		t.Fatalf("parse: %+v err=%v", ref, err)
	}
}
