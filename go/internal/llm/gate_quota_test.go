package llm

import (
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestGateQuota pins the shared foreground quota mapping (GHSA 2026-10-31 §4)
// used by ChainCall and the query handler's embed/rerank chains: nil
// accountant passes through, the call budget is *ErrQuotaExceeded, an
// external_off degrade that empties the chain is *ErrNoEligibleBackend, and
// a mixed chain keeps its local member.
func TestGateQuota(t *testing.T) {
	local := backends.Backend{ID: "l", Name: "local", Locality: "local"}
	ext := backends.Backend{ID: "e", Name: "cloud", Locality: backends.LocalityExternal}
	one := 1
	budget := 0.01

	if got, err := GateQuota(nil, "t", []backends.Backend{ext}, backends.RoleEmbed, backends.SensPublic); err != nil || len(got) != 1 {
		t.Fatalf("nil accountant must pass through: %v %v", got, err)
	}

	calls := backends.NewQuotaAccountant(nil, 0)
	calls.SeedForTest("t", backends.TenantQuota{DailyCalls: &one, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 1, 0)
	_, err := GateQuota(calls, "t", []backends.Backend{local}, backends.RoleEmbed, backends.SensPublic)
	var qe *backends.ErrQuotaExceeded
	if !errors.As(err, &qe) || qe.Reason != "daily_calls" {
		t.Fatalf("call budget: want ErrQuotaExceeded/daily_calls, got %v", err)
	}

	cost := backends.NewQuotaAccountant(nil, 0)
	cost.SeedForTest("t", backends.TenantQuota{DailyCostUSD: &budget, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 1)
	_, err = GateQuota(cost, "t", []backends.Backend{ext}, backends.RoleRerank, backends.SensPublic)
	var ne *backends.ErrNoEligibleBackend
	if !errors.As(err, &ne) || ne.Role != backends.RoleRerank {
		t.Fatalf("external_off emptying the chain: want ErrNoEligibleBackend(rerank), got %v", err)
	}
	got, err := GateQuota(cost, "t", []backends.Backend{local, ext}, backends.RoleEmbed, backends.SensPublic)
	if err != nil || len(got) != 1 || got[0].Name != "local" {
		t.Fatalf("external_off on a mixed chain must keep local only: %v %v", got, err)
	}
	if got, err := GateQuota(cost, "other", []backends.Backend{ext}, backends.RoleEmbed, backends.SensPublic); err != nil || len(got) != 1 {
		t.Fatalf("a scope outside the tenant must pass: %v %v", got, err)
	}
}
