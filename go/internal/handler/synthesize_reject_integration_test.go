//go:build integration

// MW8 (D3-W4, DECISIONS amendment B1) end-to-end through the REAL daily
// handler: a dispatcher capacity rejection on the /api/synthesize/daily path
// maps to a generic 429 WITH a B1 Retry-After header — the design's
// "Integrations-Test mit Fake-Admitter" gate for the daily row of §4.5.2. The
// body stays B6-generic (no target/depth); the header carries the snapshot
// estimate the fake admitter exposes for the rejected origin.
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/testdb"
)

// rejectHintAdmitter rejects every acquire and exposes a fixed B1 estimate for
// the rejected origin (implements dispatch.RetryHinter).
type rejectHintAdmitter struct {
	origin string
	hint   time.Duration
}

func (a rejectHintAdmitter) Acquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, dispatch.ErrTargetSaturated
}

func (a rejectHintAdmitter) RetryAfterHint(origin string, _ dispatch.Class) time.Duration {
	if origin == a.origin {
		return a.hint
	}
	return 0
}

func TestHandleDailyRejection429WithRetryAfter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	const scope = "scope-1"
	// Seed today's activity so GenerateDailyReport reaches the LLM chain
	// (empty activity short-circuits before any acquire).
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('decisions', 'seed', 'seed content', $1)`, scope); err != nil {
		t.Fatalf("seed block: %v", err)
	}

	const origin = "http://gpu:8089"
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "gpu", Name: "gpu", Host: origin, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleDigest},
	}})

	adm := rejectHintAdmitter{origin: origin, hint: 7 * time.Second}
	h := NewSynthesizeHandler(pool, bpool, blocktype.NewRegistry(), adm)

	req := httptest.NewRequest(http.MethodPost, "/api/synthesize/daily", nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		IsValid: true, ApiKeyID: "key-1", TenantID: "tenant-1", HomeScope: scope,
	}))
	rr := httptest.NewRecorder()
	h.HandleDaily(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (dispatcher rejection)", rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want \"7\" (B1 estimate for the rejected origin)", got)
	}
	body := strings.ToLower(rr.Body.String())
	if !strings.Contains(body, "server busy") {
		t.Fatalf("body not the §3.3 generic text: %s", body)
	}
	for _, leak := range []string{"gpu", "8089", "http", "saturat", "target"} {
		if strings.Contains(body, leak) {
			t.Fatalf("429 body leaks topology signal %q: %s", leak, body)
		}
	}
}
