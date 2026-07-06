package embedcache

// MW11 telemetry tests for the embed walk (design/05 A5-W5 gates): the
// per-attempt wait/wire split (§4.4a — a forced admitter wait W must land in
// WaitMs while Ms stays the physical call span, NOT ≥ W), the §4.4c abort
// rule in the failover loop (a dispatcher-preempted attempt is terminal, no
// health report, no spill onto the next chain link — B-R9), and the K9
// payload on the mirror AdmissionError (target + futile wait for the
// rejection line).

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestEmbedChainWaitFreeAttemptSplit is the §4.4a negative probe on the
// embed path: a held slot forces a real queue wait W — the attempt's WaitMs
// carries W while Ms measures ONLY the physical call (an httptest roundtrip,
// far below W). The old caller-side sequence clock would have folded W into
// duration_ms.
func TestEmbedChainWaitFreeAttemptSplit(t *testing.T) {
	srv := newEmbedServer(t, 0, http.StatusOK)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	origin, err := dispatch.NormalizeOrigin(srv.srv.URL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})
	adm := interactiveAdmission(t, d, "k1", "scope-wait")

	// Hold the single slot so the embed acquire has to queue.
	holder, _, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: srv.srv.URL}, Class: dispatch.ClassInteractive, Role: "embed",
	})
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	type outcome struct {
		attempts []WireAttempt
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		_, _, attempts, _, err := EmbedChain(context.Background(), nil,
			[]backends.Backend{embedBackend("b", srv.srv.URL)},
			"embed", "wait probe", embed.PrefixQuery, nil, adm)
		done <- outcome{attempts, err}
	}()

	waitFor(t, "embed acquire queued", func() bool {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == origin && ts.Interactive.Waiting == 1 {
				return true
			}
		}
		return false
	})
	const holdMs = 60
	time.Sleep(holdMs * time.Millisecond)
	holder.Release()

	o := <-done
	if o.err != nil {
		t.Fatalf("EmbedChain: %v", o.err)
	}
	if len(o.attempts) != 1 || o.attempts[0].Class != "ok" {
		t.Fatalf("attempts = %+v, want one ok attempt", o.attempts)
	}
	a := o.attempts[0]
	if a.WaitMs < holdMs-10 {
		t.Errorf("WaitMs = %d, want >= ~%d (the forced queue wait)", a.WaitMs, holdMs-10)
	}
	if a.Ms >= a.WaitMs {
		t.Errorf("Ms = %d >= WaitMs = %d — the wire span must be wait-free (§4.4a), not the acquire+call span", a.Ms, a.WaitMs)
	}
}

// TestEmbedChainPreemptIsTerminalAndClassified drives the §4.4c abort rule
// through the REAL preempt path (B-R9 on the embed loop): slots=1 +
// preempt_background=true, a background embed on the wire, an interactive
// acquire preempts it. The aborted attempt must be TERMINAL — no failover
// onto the second chain link, no health report (shared pool health) — and
// carry the class "preempted" instead of a transport class.
func TestEmbedChainPreemptIsTerminalAndClassified(t *testing.T) {
	blocked := newEmbedServer(t, 0, http.StatusOK)
	spill := newEmbedServer(t, 0, http.StatusOK)
	running := make(chan struct{})
	var gateOnce sync.Once
	gate := make(chan struct{})
	t.Cleanup(func() { gateOnce.Do(func() { close(gate) }) }) // unblock the handler before srv.Close
	var runOnce sync.Once
	blocked.gate = func(string) {
		runOnce.Do(func() { close(running) })
		<-gate
	}

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	origin, err := dispatch.NormalizeOrigin(blocked.srv.URL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		origin: {Slots: 1, PreemptBackground: true},
	}})
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{ApiKeyID: "k-pre", TenantID: "t1", HomeScope: "scope-pre"}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })

	var reports []backends.ErrClass
	report := func(_ string, class backends.ErrClass, _ time.Duration) { reports = append(reports, class) }
	bg := Admission{Admitter: d, Class: dispatch.ClassBackground}

	type outcome struct {
		attempts []WireAttempt
		wired    bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		_, _, attempts, wired, err := EmbedChain(context.Background(), nil,
			[]backends.Backend{embedBackend("gpu", blocked.srv.URL), embedBackend("fallback", spill.srv.URL)},
			"embed", "preempt probe", embed.PrefixQuery, report, bg)
		done <- outcome{attempts, wired, err}
	}()

	<-running // background embed holds the slot on the wire
	// Interactive demand on the same origin preempts the background lease.
	lease, _, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: blocked.srv.URL}, Class: dispatch.ClassInteractive, Role: "embed",
	})
	if err != nil {
		t.Fatalf("interactive contender: %v", err)
	}
	lease.Release()

	o := <-done
	if o.err == nil {
		t.Fatal("preempted walk must return its error")
	}
	if !o.wired {
		t.Fatal("wired = false, want true (the attempt reached the wire before the preempt)")
	}
	if len(o.attempts) != 1 || o.attempts[0].Class != llmlog.AbortPreempted {
		t.Fatalf("attempts = %+v, want exactly one attempt classed %q (terminal, §4.4c)", o.attempts, llmlog.AbortPreempted)
	}
	if spill.hits() != 0 {
		t.Fatalf("fallback wire hits = %d, want 0 — a preempted background embed must never spill (B-R9)", spill.hits())
	}
	if len(reports) != 0 {
		t.Fatalf("health reports = %v, want none — a preempt must not cooldown the shared target", reports)
	}
}

// TestEmbedChainRejectionCarriesK9Payload pins the mirror AdmissionError:
// the rejected target and the futile wait travel with the error so
// llm.RecordRejection can attribute the K9 line (MW11, design/05 §3.2).
func TestEmbedChainRejectionCarriesK9Payload(t *testing.T) {
	srv := newEmbedServer(t, 0, http.StatusOK)
	rec := newRecordingAdmitter(t)
	rec.reject = dispatch.ErrQueueFull
	adm := interactiveAdmission(t, rec, "k1", "scope-k9")

	_, _, _, _, err := EmbedChain(context.Background(), nil,
		[]backends.Backend{embedBackend("cpu-embed", srv.srv.URL)},
		"embed", "k9 payload probe", embed.PrefixQuery, nil, adm)
	var ae *AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want the mirror *AdmissionError in the chain", err)
	}
	if ae.Backend != "cpu-embed" || ae.Host != srv.srv.URL || ae.WaitMs < 0 {
		t.Fatalf("K9 payload = %q/%q/%d, want the rejected target + non-negative wait", ae.Backend, ae.Host, ae.WaitMs)
	}
	if !errors.Is(err, dispatch.ErrQueueFull) || !dispatch.IsRejection(err) {
		t.Fatalf("err = %v, sentinel must stay errors.Is-reachable through the mirror", err)
	}
}
