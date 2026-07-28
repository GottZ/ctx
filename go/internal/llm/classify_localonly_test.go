// H13b — negative probe for the claimed capability boundary "the classify
// path NEVER reaches an external backend" (design/04 §4.8 table row 2).
//
// The claim rests on two fields at classify.go:103-104: Required
// SensCredentials (the trust gate) and LocalOnly true (the locality gate).
// The trust gate ALONE does not carry the claim: `full-trust` is documented
// as "local providers + ZDR cloud" (backends/trust.go:9), so a full-trust row
// with locality `external` passes the trust gate by construction. Only
// LocalOnly removes it — and when it is the only eligible row, the call must
// end as ErrNoEligibleBackend WITHOUT wire contact, never as a silent
// escalation across the locality border.
//
// TestDropExternal (classify_test.go) probes the filter function in isolation.
// This probe drives the REAL entry point ClassifyBlockBool over a pool that
// contains exactly one such backend, and counts the hits on it.
//
// Mutation that turns this RED: LocalOnly:false at classify.go:105.
package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// externalFullTrustBackend is the constructible worst case: trusted enough for
// credentials, located outside the trusted zone.
func externalFullTrustBackend(host string) backends.Backend {
	return backends.Backend{
		ID: "zdr-cloud", Name: "zdr-cloud", Host: host,
		Trust:    backends.TrustFull,
		Locality: backends.LocalityExternal,
		Roles:    []string{backends.RoleClassify},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Priority: 100, Enabled: true,
	}
}

func TestClassifyBlockBoolNeverLeavesTheLocalityBorder(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":true}"}}]}`))
	}))
	defer srv.Close()

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{externalFullTrustBackend(srv.URL)})

	// Vacuum-green guard: the row IS eligible after the trust gate — the raw
	// chain contains it. Anything the probe below shows is therefore the work
	// of the locality filter, not of an empty pool.
	chain, cerr := p.Chain(backends.RoleClassify, backends.SensCredentials, "")
	if cerr != nil || len(chain) != 1 || chain[0].Name != "zdr-cloud" {
		t.Fatalf("trust-gated chain = %v (err %v), want exactly [zdr-cloud] — the probe would be vacuum-green", chain, cerr)
	}

	// A LIVE pass-through admission (the classify caller is the background
	// scheduler job): nothing but the locality filter may stand between the
	// call and the wire — otherwise the hit counter below would prove the
	// dispatcher's refusal instead of the locality border's.
	// db is nil on purpose: an empty chain must return BEFORE any llmlog write.
	got, err := ClassifyBlockBool(context.Background(), nil, p,
		"Does this block contain credentials?", "Titel", "geheimer Inhalt", "11111111-1111-1111-1111-111111111111",
		testAdmission(t, dispatch.ClassBackground))

	// The load-bearing half FIRST: no wire contact at all.
	if n := hits.Load(); n != 0 {
		t.Fatalf("the external backend was called %d times — unclassified block content crossed the locality border", n)
	}
	var noElig *backends.ErrNoEligibleBackend
	if !errors.As(err, &noElig) {
		t.Fatalf("ClassifyBlockBool = (%v, %v), want *ErrNoEligibleBackend — an external backend must never serve the classify role", got, err)
	}
	if got {
		t.Fatalf("verdict = %v on a refused call, want the false zero value", got)
	}
	if noElig.Role != backends.RoleClassify || noElig.Required != backends.SensCredentials {
		t.Fatalf("error = %+v, want role %q / required %q", noElig, backends.RoleClassify, backends.SensCredentials)
	}
	if len(noElig.Excluded) == 0 || !strings.Contains(noElig.Excluded[0].Reason, "local-only") {
		t.Fatalf("exclusion reasons = %+v, want the local-only reason (admin-visible diagnosis)", noElig.Excluded)
	}
}
