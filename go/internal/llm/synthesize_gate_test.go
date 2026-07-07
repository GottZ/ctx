// F3-P3 trust gate tests (design 03 §5, negative-probed classes 3/6/8):
// the gate measures the FINAL prompt set, a zero-value sensitivity acts as
// credentials, and a backend the matrix excludes is never contacted — proven
// by hit counters on real HTTP servers, not by inspecting the chain.
package llm

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

func asNoEligible(err error, target **backends.ErrNoEligibleBackend) bool {
	return errors.As(err, target)
}

// countingOllama returns an ollama-wire chat stub counting its hits.
func countingOllama(t *testing.T, answer string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprintf(w, `{"message":{"role":"assistant","content":"%s"},"eval_count":1,"prompt_eval_count":1}`, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func gateBackend(id string, host string, trust backends.Trust, priority int) backends.Backend {
	return backends.Backend{
		ID: id, Name: id, Host: host, Protocol: backends.ProtocolOllama,
		Model: "m", Trust: trust, Enabled: true, Priority: priority,
		Roles: []string{backends.RoleSynthesis},
	}
}

func gateSettings() SynthesisSettings {
	return SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: PromptVersionV52}
}

// TestSynthesizeGate_PublicBackendNeverSeesPersonal: a public-trust backend
// with the HIGHEST priority must receive zero hits for a personal-class
// operation — the full-trust backend serves instead. Red without the chain's
// trust filter (priority would route every request to the public stub).
func TestSynthesizeGate_PublicBackendNeverSeesPersonal(t *testing.T) {
	public, publicHits := countingOllama(t, "leaked")
	private, privateHits := countingOllama(t, "served")

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		gateBackend("pub", public.URL, backends.TrustPublic, 1000), // tempting priority
		gateBackend("ft", private.URL, backends.TrustFull, 1),
	})

	sources := []Source{{ID: "1", Title: "t", Category: "c", Content: "body",
		Score: 0.5, Sensitivity: backends.SensPersonal}}
	res, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, gateSettings(),
		backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if res.Answer != "served" {
		t.Errorf("answer = %q, want the full-trust backend's answer", res.Answer)
	}
	if publicHits.Load() != 0 {
		t.Errorf("public backend hits = %d, MUST be 0 — personal content crossed the trust border", publicHits.Load())
	}
	if privateHits.Load() != 1 {
		t.Errorf("full-trust backend hits = %d, want 1", privateHits.Load())
	}
}

// TestSynthesizeGate_MeasuresFinalSetNotAllCandidates: a credentials block
// BELOW the score threshold never enters the prompt and must not raise the
// requirement — the no-credentials backend still serves. Red against a "gate
// over all candidates" implementation (over-blocking, risk 6.3).
func TestSynthesizeGate_MeasuresFinalSetNotAllCandidates(t *testing.T) {
	nc, ncHits := countingOllama(t, "served")

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		gateBackend("nc", nc.URL, backends.TrustNoCredentials, 1),
	})

	sources := []Source{
		{ID: "1", Title: "t", Category: "c", Content: "body",
			Score: 0.5, Sensitivity: backends.SensPersonal},
		// Filtered out by FilterByScore (below threshold) — never in the prompt.
		{ID: "2", Title: "secret", Category: "c", Content: "key material",
			Score: 0.0001, Sensitivity: backends.SensCredentials},
	}
	res, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, gateSettings(),
		backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
	if err != nil {
		t.Fatalf("Synthesize: %v — a rank-filtered credentials block locked the chain", err)
	}
	if res.Answer != "served" || ncHits.Load() != 1 {
		t.Errorf("answer=%q hits=%d, want served/1 (required must stay personal)", res.Answer, ncHits.Load())
	}
}

// TestSynthesizeGate_ZeroValueActsAsCredentials: a source whose Sensitivity
// was never assigned (lookup miss / forgotten future call site) counts as
// credentials — the no-credentials backend gets zero hits and the chain comes
// up empty. Red against "unknown ⇒ rank 0" (the silent public downgrade).
func TestSynthesizeGate_ZeroValueActsAsCredentials(t *testing.T) {
	nc, ncHits := countingOllama(t, "leaked")

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		gateBackend("nc", nc.URL, backends.TrustNoCredentials, 1),
	})

	sources := []Source{{ID: "1", Title: "t", Category: "c", Content: "body",
		Score: 0.5 /* Sensitivity deliberately unset */}}
	_, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, gateSettings(),
		backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
	if err == nil {
		t.Fatal("want ErrNoEligibleBackend — zero-value sensitivity must act as credentials")
	}
	var noElig *backends.ErrNoEligibleBackend
	if !asNoEligible(err, &noElig) {
		t.Fatalf("error = %v, want *ErrNoEligibleBackend", err)
	}
	if noElig.Required != backends.SensCredentials {
		t.Errorf("required = %q, want credentials (fail-closed)", noElig.Required)
	}
	if ncHits.Load() != 0 {
		t.Errorf("no-credentials backend hits = %d, MUST be 0", ncHits.Load())
	}
}
