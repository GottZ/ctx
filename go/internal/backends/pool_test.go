package backends

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// seedPool publishes a static snapshot (white-box: the swap is exactly what
// Reload does after scanning).
func seedPool(bs []Backend) *Pool {
	p := NewPool(nil, nil)
	p.snap.Store(&snapshot{backends: bs, version: 1, loadedAt: time.Now()})
	return p
}

func chainNames(t *testing.T, p *Pool, role string, required Sensitivity, g GamingState) []string {
	t.Helper()
	chain, err := p.Chain(role, required, g)
	if err != nil {
		t.Fatalf("Chain(%s, %s): %v", role, required, err)
	}
	names := make([]string, len(chain))
	for i, b := range chain {
		names[i] = b.Name
	}
	return names
}

func poolFixture() []Backend {
	return []Backend{
		{ID: "1", Name: "gpu", Trust: TrustFull, Roles: []string{RoleSynthesis, RoleDream}, Priority: 100, Enabled: true},
		{ID: "2", Name: "cpu", Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 10, Enabled: true},
		{ID: "3", Name: "cloud", Trust: TrustNoCredentials, Roles: []string{RoleSynthesis}, Priority: 50, Enabled: true},
		{ID: "4", Name: "off", Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 200, Enabled: false},
		{ID: "5", Name: "embedder", Trust: TrustFull, Roles: []string{RoleEmbed}, Priority: 100, Enabled: true},
	}
}

func TestChainFilterAndOrder(t *testing.T) {
	p := seedPool(poolFixture())

	// public content: every enabled synthesis backend, priority DESC.
	got := chainNames(t, p, RoleSynthesis, SensPublic, GamingState{})
	want := []string{"gpu", "cloud", "cpu"}
	if len(got) != len(want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain = %v, want %v", got, want)
		}
	}

	// credentials content: trust gate removes the no-credentials cloud.
	got = chainNames(t, p, RoleSynthesis, SensCredentials, GamingState{})
	if len(got) != 2 || got[0] != "gpu" || got[1] != "cpu" {
		t.Fatalf("credentials chain = %v, want [gpu cpu]", got)
	}

	// role filter: embed chain never contains synthesis backends.
	got = chainNames(t, p, RoleEmbed, SensCredentials, GamingState{})
	if len(got) != 1 || got[0] != "embedder" {
		t.Fatalf("embed chain = %v, want [embedder]", got)
	}

	// disabled backends never appear despite top priority.
	for _, n := range chainNames(t, p, RoleSynthesis, SensPublic, GamingState{}) {
		if n == "off" {
			t.Fatal("disabled backend made the chain")
		}
	}
}

// TestChainCooldownSortsNotRemoves: cooldown is an order hint, not a circuit
// breaker — a cooled single-backend role stays reachable.
func TestChainCooldownSortsNotRemoves(t *testing.T) {
	p := seedPool(poolFixture())
	p.ReportFailure("1", ClassTransport, 0) // gpu: 30s cooldown

	got := chainNames(t, p, RoleSynthesis, SensPublic, GamingState{})
	if got[len(got)-1] != "gpu" {
		t.Fatalf("cooled backend not sorted last: %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("cooldown removed a backend: %v", got)
	}

	// Single-backend role under cooldown: still returned.
	p.ReportFailure("5", ClassGateway, 0)
	got = chainNames(t, p, RoleEmbed, SensCredentials, GamingState{})
	if len(got) != 1 || got[0] != "embedder" {
		t.Fatalf("single cooled backend dropped: %v", got)
	}

	// Success clears the cooldown.
	p.ReportSuccess("1")
	got = chainNames(t, p, RoleSynthesis, SensPublic, GamingState{})
	if got[0] != "gpu" {
		t.Fatalf("ReportSuccess did not clear cooldown: %v", got)
	}
}

func TestChainGamingExclusion(t *testing.T) {
	p := seedPool(poolFixture())
	g := GamingState{Active: true, DisabledBackends: []string{"gpu"}}

	got := chainNames(t, p, RoleSynthesis, SensPublic, g)
	for _, n := range got {
		if n == "gpu" {
			t.Fatal("gaming-disabled backend made the chain")
		}
	}

	// Inactive gaming: list is ignored.
	g.Active = false
	got = chainNames(t, p, RoleSynthesis, SensPublic, g)
	if got[0] != "gpu" {
		t.Fatalf("inactive gaming still excluded: %v", got)
	}
}

// TestChainEmpty: trust beats availability — the error carries per-backend
// reasons for slog/admin, and NEVER silently escalates.
func TestChainEmpty(t *testing.T) {
	p := seedPool(poolFixture())

	_, err := p.Chain(RoleEmbed, SensPublic, GamingState{Active: true, DisabledBackends: []string{"embedder"}})
	var noElig *ErrNoEligibleBackend
	if err == nil {
		t.Fatal("expected ErrNoEligibleBackend")
	}
	if !asNoEligible(err, &noElig) {
		t.Fatalf("error type = %T", err)
	}
	if noElig.Role != RoleEmbed || len(noElig.Excluded) == 0 {
		t.Fatalf("reasons missing: %+v", noElig)
	}
	if noElig.Excluded[0].Reason != "disabled by gaming" {
		t.Fatalf("reason = %q", noElig.Excluded[0].Reason)
	}

	// Unknown role: empty chain, no reasons (no row was ever a candidate).
	if _, err := p.Chain("proxy:nonexistent", SensPublic, GamingState{}); err == nil {
		t.Fatal("unknown role must yield ErrNoEligibleBackend")
	}
}

func asNoEligible(err error, target **ErrNoEligibleBackend) bool {
	return errors.As(err, target)
}

// TestSnapshotAtomicity hammers Chain while snapshots swap underneath (run
// under -race): every read dereferences the pointer exactly once — no mixed
// read across two generations (the chatWithFallback torn-read path died for
// this).
func TestSnapshotAtomicity(t *testing.T) {
	p := seedPool(poolFixture())
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		gen := poolFixture()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				p.snap.Store(&snapshot{backends: gen, version: int64(i), loadedAt: time.Now()})
			}
		}
	}()

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				chain, err := p.Chain(RoleSynthesis, SensPublic, GamingState{})
				if err != nil || len(chain) != 3 {
					t.Errorf("chain corrupted during swap: %v %v", chain, err)
					return
				}
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestParseModelMap: string short form and object form load equivalently.
func TestParseModelMap(t *testing.T) {
	short, err := ParseModelMap([]byte(`{"default":"qwen3.5:9b"}`))
	if err != nil {
		t.Fatal(err)
	}
	long, err := ParseModelMap([]byte(`{"default":{"model":"qwen3.5:9b"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if short["default"].Model != long["default"].Model {
		t.Fatalf("short %v != long %v", short, long)
	}

	withParams, err := ParseModelMap([]byte(`{"synthesis":{"model":"m","params":{"top_p":0.8,"think":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if withParams["synthesis"].Params["top_p"] != 0.8 {
		t.Fatalf("params lost: %v", withParams)
	}

	if _, err := ParseModelMap([]byte(`{"x":42}`)); err == nil {
		t.Fatal("numeric model_map value must fail")
	}
}

func TestModelForAndTimeoutFor(t *testing.T) {
	b := Backend{
		ModelMap: map[string]ModelSpec{
			"default":     {Model: "base"},
			RoleSynthesis: {Model: "big"},
		},
		Timeouts: map[string]int{RoleSynthesis: 420},
	}
	if b.ModelFor(RoleSynthesis).Model != "big" {
		t.Error("role-specific model not resolved")
	}
	if b.ModelFor(RoleTranslate).Model != "base" {
		t.Error("default fallback not resolved")
	}
	if b.TimeoutFor(RoleSynthesis, time.Minute) != 420*time.Second {
		t.Error("timeout override not resolved")
	}
	if b.TimeoutFor(RoleTranslate, time.Minute) != time.Minute {
		t.Error("timeout default not resolved")
	}
}

// TestStatusSanitized: the status surface carries error CLASSES, never raw
// error strings (URLs/provider bodies live in slog only).
func TestStatusSanitized(t *testing.T) {
	p := seedPool(poolFixture())
	p.ReportFailure("1", ClassAuth, 0)

	var gpu *BackendStatus
	for _, s := range p.Status() {
		if s.Name == "gpu" {
			tmp := s
			gpu = &tmp
		}
	}
	if gpu == nil {
		t.Fatal("gpu missing from status")
	}
	if gpu.EffectiveState != "cooldown" || gpu.CooldownRemaining <= 0 {
		t.Fatalf("cooldown state missing: %+v", gpu)
	}
	if gpu.LastErrorClass != "auth" {
		t.Fatalf("last error class = %q, want auth", gpu.LastErrorClass)
	}
}

// TestConsecutiveFailBackoff: ≥3 fails double the class cooldown (cap 10m).
func TestConsecutiveFailBackoff(t *testing.T) {
	p := seedPool(poolFixture())
	for i := 0; i < 3; i++ {
		p.ReportFailure("2", ClassTransport, 0)
	}
	p.healthM.Lock()
	until := p.health["2"].cooldownUntil
	p.healthM.Unlock()
	if d := time.Until(until); d < 45*time.Second || d > 70*time.Second {
		t.Fatalf("doubled cooldown out of range: %s", d)
	}
}
