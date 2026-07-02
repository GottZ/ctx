//go:build integration

// E2E-Q-INT — the RELEASE GATE (design/02 §Q-INT, Masterplan/BUILD-LOG).
//
// The sharp proof that the api-key quota is enforced AT THE HANDLER SEAM — the
// real HandleManage path (auth-injected AuthResult → enforceActionTier →
// dispatchAPIKeyAction → handleApiKeyCreate → store.MintKeyWithQuota), NOT the
// store mechanic in isolation (that is BE6-2's api_keys_mint_quota test) and NOT
// a Playwright fixture (that is BE5-Q4's status:429 mock). The release-invariante
// is: self-service onboarding must NOT be unlocked/advertised before these three
// are green, because a fixture-only proof would let the FE show a 429 surface while
// the server enforced nothing.
//
// Three proofs, all through HandleManage:
//  1. LIVE429  max_keys=2, a tenant-admin mints 3× into its OWN tenant/home_scope:
//     first two → 200, the third → 429 ("tenant key quota exceeded"), and the
//     server-side active-key count is exactly 2 (no leaked over-cap key).
//  2. ParallelHandlerRace  max_keys=3, 12 CONCURRENT api-key-create calls over
//     HandleManage → EXACTLY 3 commit (200), 9 → 429, final active-key count == 3.
//     The context_tenants-row FOR UPDATE inside MintKeyWithQuota serialises the
//     whole handler seam — no TOCTOU over the real path (a per-key/no-lock impl
//     would over-admit and the count would exceed 3).
//  3. Tier403BeforeQuota429  the gate ORDER: enforceActionTier fires BEFORE
//     handleApiKeyCreate. At a frozen cap (max_keys=0) the CONTROL tenant-admin
//     reaches the quota gate and gets 429 — proving the path genuinely 429s here —
//     while a MEMBER of the same tenant at the same cap is stopped at the tier
//     gate with 403, NEVER reaching the 429. Tier protects before quota runs.
//
// Helpers (be5*/be6*) are shared with scope_create_handler_test.go and
// api_key_create_tenant_test.go (same package, same build tag) — reused, not
// re-declared. The structural cap lives on the typed 069 column max_keys
// (CHECK >= 0, so 0 = frozen is a valid stamp), set via be6SetMaxKeys.
//
//	go test -tags=integration ./internal/handler/ -run TestApiKeyQuota_QINT_ReleaseGate -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

// qintActiveKeyCount reads the server-side truth: the tenant's ACTIVE key count
// (the exact quantity MintKeyWithQuota caps against, S3b active-only).
func qintActiveKeyCount(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid AND active = true`,
		tenantID).Scan(&n); err != nil {
		t.Fatalf("count active keys for %s: %v", tenantID, err)
	}
	return n
}

// qintManageStatus drives HandleManage and returns ONLY the HTTP status. It is
// deliberately GOROUTINE-SAFE — it makes no *testing.T calls (a t.Fatalf from a
// non-test goroutine calls runtime.Goexit on the wrong goroutine), so it is the
// driver used by the parallel race. The json.Marshal of a fixed map cannot fail;
// any error degrades to status 0, which the caller's tally flags as "other".
func qintManageStatus(h *ManageHandler, ar *auth.AuthResult, body map[string]any) int {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec.Code
}

// TestApiKeyQuota_QINT_ReleaseGate_Integration is the Q-INT release gate: the
// api-key cap proven SHARP at the HandleManage seam (live handler, not store, not
// fixture). Each subtest seeds its own isolated tenant so the active-key count is
// deterministic.
func TestApiKeyQuota_QINT_ReleaseGate_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	// 1. LIVE-429 (live handler): max_keys=2, a tenant-admin self-mints 3× → the
	// first two 200, the third 429 with the quota error, active count exactly 2.
	t.Run("LIVE429_ThirdMintRejected", func(t *testing.T) {
		tid := be5SeedTenant(t, pool, "qint-live")
		scope := "qint-live:keys"
		be5SeedScope(t, pool, scope, tid)
		be6SetMaxKeys(t, pool, tid, 2)
		admin := be5TenantAdmin(tid)

		for _, label := range []string{"k1", "k2"} {
			rec := be5ScopeManageAs(t, h, admin, map[string]any{
				"action": "api-key-create",
				"data":   map[string]any{"label": label, "home_scope": scope},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("mint %q under cap=2: status %d, want 200 (body=%s)", label, rec.Code, rec.Body.String())
			}
		}

		// The third mint exceeds max_keys=2 → 429, mapped from ErrKeyQuotaExceeded
		// by the REAL handler (not a fixture).
		rec := be5ScopeManageAs(t, h, admin, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "k3", "home_scope": scope},
		})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("third mint over cap=2: status %d, want 429 (body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "quota") {
			t.Errorf("429 body is not the quota gate (body=%s)", rec.Body.String())
		}
		// Server-side truth: the rejected mint left NO committed key behind.
		if n := qintActiveKeyCount(t, pool, tid); n != 2 {
			t.Fatalf("active keys after 3 mints (cap=2) = %d, want 2 (no over-cap leak)", n)
		}
	})

	// 2. 4N-parallel handler-seam race: max_keys=3, 12 concurrent api-key-create
	// calls over HandleManage → exactly 3 commit, 9 → 429, final active count == 3.
	// The FOR UPDATE on the context_tenants row inside MintKeyWithQuota serialises
	// the whole live path; a per-key/no-lock impl would over-admit (count > 3).
	t.Run("ParallelHandlerRace_ExactlyCapCommit", func(t *testing.T) {
		const capN = 3
		const callers = 4 * capN // 12 — the 4N-parallel probe
		tid := be5SeedTenant(t, pool, "qint-race")
		scope := "qint-race:keys"
		be5SeedScope(t, pool, scope, tid)
		be6SetMaxKeys(t, pool, tid, capN)
		admin := be5TenantAdmin(tid)

		codes := make([]int, callers)
		var wg sync.WaitGroup
		wg.Add(callers)
		for i := 0; i < callers; i++ {
			go func(i int) {
				defer wg.Done()
				// Each caller carries its OWN label so success is not deduped by
				// any label collision — the only thing capping them is max_keys.
				codes[i] = qintManageStatus(h, admin, map[string]any{
					"action": "api-key-create",
					"data":   map[string]any{"label": fmt.Sprintf("race-%d", i), "home_scope": scope},
				})
			}(i)
		}
		wg.Wait()

		ok, tooMany, other := 0, 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusTooManyRequests:
				tooMany++
			default:
				other++
			}
		}
		if other != 0 {
			t.Fatalf("race: %d caller(s) returned an unexpected status (codes=%v)", other, codes)
		}
		if ok != capN || tooMany != callers-capN {
			t.Fatalf("race outcome: 200=%d 429=%d, want exactly %d and %d (codes=%v)",
				ok, tooMany, capN, callers-capN, codes)
		}
		// The load-bearing assertion: the cap held under the live concurrent seam.
		if n := qintActiveKeyCount(t, pool, tid); n != capN {
			t.Fatalf("active keys after %d-way race (cap=%d) = %d, want %d (TOCTOU over the handler path)",
				callers, capN, n, capN)
		}
	})

	// 3. 403-before-429: the tier gate fires before the quota logic. At a frozen
	// cap (max_keys=0) the CONTROL tenant-admin reaches the quota gate → 429 (the
	// path genuinely 429s at this cap); the MEMBER at the SAME cap is stopped at
	// enforceActionTier → 403, never reaching the 429 the admin hit.
	t.Run("Tier403BeforeQuota429", func(t *testing.T) {
		tid := be5SeedTenant(t, pool, "qint-tier")
		scope := "qint-tier:keys"
		be5SeedScope(t, pool, scope, tid)
		be6SetMaxKeys(t, pool, tid, 0) // frozen: anything reaching the quota gate 429s
		body := map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "x", "home_scope": scope},
		}

		// CONTROL: a tenant-admin clears the tier gate, reaches the quota logic, and
		// gets 429 at cap=0 — establishing that this very path DOES 429 here.
		if rec := be5ScopeManageAs(t, h, be5TenantAdmin(tid), body); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("control tenant-admin at cap=0: status %d, want 429 (the path must reach the quota gate; body=%s)",
				rec.Code, rec.Body.String())
		}

		// PROOF: a member of the same tenant at the same cap is stopped at the tier
		// gate → 403, NEVER 429. enforceActionTier runs before handleApiKeyCreate.
		rec := be5ScopeManageAs(t, h, be5Member(tid), body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member at cap=0: status %d, want 403 (tier gate before quota; body=%s)", rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("member leaked a 429 — quota ran before the tier gate (S4 violation)")
		}
		// Neither the control nor the member committed a key (control 429'd, member 403'd).
		if n := qintActiveKeyCount(t, pool, tid); n != 0 {
			t.Fatalf("active keys after frozen-cap probes = %d, want 0", n)
		}
	})
}
