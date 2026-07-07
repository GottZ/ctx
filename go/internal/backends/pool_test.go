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

// seedPoolWithDisabledBy publishes a static snapshot carrying a precomputed
// disabledBy map (backend_id → comma-joined SORTED active-profile names, the
// shape buildDisabledBy produces at Reload). W2 chain arm + Status() read it.
func seedPoolWithDisabledBy(bs []Backend, disabledBy map[string]string) *Pool {
	p := NewPool(nil, nil)
	p.snap.Store(&snapshot{backends: bs, disabledBy: disabledBy, version: 1, loadedAt: time.Now()})
	return p
}

func chainNames(t *testing.T, p *Pool, role string, required Sensitivity) []string {
	t.Helper()
	chain, err := p.Chain(role, required, "")
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
	got := chainNames(t, p, RoleSynthesis, SensPublic)
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
	got = chainNames(t, p, RoleSynthesis, SensCredentials)
	if len(got) != 2 || got[0] != "gpu" || got[1] != "cpu" {
		t.Fatalf("credentials chain = %v, want [gpu cpu]", got)
	}

	// role filter: embed chain never contains synthesis backends.
	got = chainNames(t, p, RoleEmbed, SensCredentials)
	if len(got) != 1 || got[0] != "embedder" {
		t.Fatalf("embed chain = %v, want [embedder]", got)
	}

	// disabled backends never appear despite top priority.
	for _, n := range chainNames(t, p, RoleSynthesis, SensPublic) {
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

	got := chainNames(t, p, RoleSynthesis, SensPublic)
	if got[len(got)-1] != "gpu" {
		t.Fatalf("cooled backend not sorted last: %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("cooldown removed a backend: %v", got)
	}

	// Single-backend role under cooldown: still returned.
	p.ReportFailure("5", ClassGateway, 0)
	got = chainNames(t, p, RoleEmbed, SensCredentials)
	if len(got) != 1 || got[0] != "embedder" {
		t.Fatalf("single cooled backend dropped: %v", got)
	}

	// Success clears the cooldown.
	p.ReportSuccess("1")
	got = chainNames(t, p, RoleSynthesis, SensPublic)
	if got[0] != "gpu" {
		t.Fatalf("ReportSuccess did not clear cooldown: %v", got)
	}
}

// TestChainProfileExclusion is the W2 gate (§7-W2): a backend disabled by an
// ACTIVE profile (present in the snapshot disabledBy map) must fall out of the
// chain with the reason "disabled by profile <names>". Against the W1 stand this
// FAILS — Chain() ignored disabledBy, so the member stayed in the chain.
func TestChainProfileExclusion(t *testing.T) {
	// Active profile "eject" disables "gpu" (id "1").
	p := seedPoolWithDisabledBy(poolFixture(), map[string]string{"1": "eject"})

	got := chainNames(t, p, RoleSynthesis, SensPublic)
	for _, n := range got {
		if n == "gpu" {
			t.Fatalf("profile-disabled backend made the chain: %v", got)
		}
	}

	// The exclusion reason names the profile(s), comma-joined and sorted. Drive
	// the empty-chain path so Excluded is populated: embedder (id "5") is the
	// only embed backend — disabling it empties the embed chain.
	p2 := seedPoolWithDisabledBy(poolFixture(), map[string]string{"5": "eject,gpu-wartung"})
	_, err := p2.Chain(RoleEmbed, SensPublic, "")
	var noElig *ErrNoEligibleBackend
	if !asNoEligible(err, &noElig) {
		t.Fatalf("expected ErrNoEligibleBackend, got %T (%v)", err, err)
	}
	if len(noElig.Excluded) == 0 {
		t.Fatalf("reasons missing: %+v", noElig)
	}
	if noElig.Excluded[0].Reason != "disabled by profile eject,gpu-wartung" {
		t.Fatalf("reason = %q, want %q", noElig.Excluded[0].Reason, "disabled by profile eject,gpu-wartung")
	}

	// A backend NOT in an active profile stays in the chain (disabledBy absent).
	got = chainNames(t, p, RoleEmbed, SensCredentials)
	if len(got) != 1 || got[0] != "embedder" {
		t.Fatalf("non-disabled backend dropped: %v", got)
	}
}

// TestChainEmpty: trust beats availability — the error carries per-backend
// reasons for slog/admin, and NEVER silently escalates. The single embed
// backend is emptied via an ACTIVE disable-profile (the sole exclusion
// mechanism since U01-W5; the reason carries the profile name).
func TestChainEmpty(t *testing.T) {
	p := seedPoolWithDisabledBy(poolFixture(), map[string]string{"5": "eject"})

	_, err := p.Chain(RoleEmbed, SensPublic, "")
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
	if noElig.Excluded[0].Reason != "disabled by profile eject" {
		t.Fatalf("reason = %q", noElig.Excluded[0].Reason)
	}

	// Unknown role: empty chain, no reasons (no row was ever a candidate).
	if _, err := p.Chain("proxy:nonexistent", SensPublic, ""); err == nil {
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
				chain, err := p.Chain(RoleSynthesis, SensPublic, "")
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

// TestStatusProfileDisabled is the W2 effective_state gate (§7-W2): a backend
// held by an ACTIVE profile reports effective_state "profile-disabled" and its
// DisabledByProfiles names; `disabled` (enabled=false) wins the precedence, and
// cooldown loses to profile-disabled.
func TestStatusProfileDisabled(t *testing.T) {
	p := seedPoolWithDisabledBy(poolFixture(), map[string]string{
		"1": "eject",          // gpu (enabled)      → profile-disabled
		"4": "eject",          // off (enabled=false) → disabled wins
		"5": "eject,gpu-wart", // embedder           → profile-disabled, 2 names
	})

	byName := func() map[string]BackendStatus {
		m := map[string]BackendStatus{}
		for _, s := range p.Status() {
			m[s.Name] = s
		}
		return m
	}

	st := byName()
	if st["gpu"].EffectiveState != "profile-disabled" {
		t.Fatalf("gpu state = %q, want profile-disabled", st["gpu"].EffectiveState)
	}
	if got := st["gpu"].DisabledByProfiles; len(got) != 1 || got[0] != "eject" {
		t.Fatalf("gpu DisabledByProfiles = %v, want [eject]", got)
	}
	if got := st["embedder"].DisabledByProfiles; len(got) != 2 || got[0] != "eject" || got[1] != "gpu-wart" {
		t.Fatalf("embedder DisabledByProfiles = %v, want [eject gpu-wart]", got)
	}
	// disabled beats profile-disabled — but the membership is still surfaced.
	if st["off"].EffectiveState != "disabled" {
		t.Fatalf("off state = %q, want disabled (disabled beats profile-disabled)", st["off"].EffectiveState)
	}
	if got := st["off"].DisabledByProfiles; len(got) != 1 || got[0] != "eject" {
		t.Fatalf("off DisabledByProfiles = %v, want [eject] even when disabled", got)
	}
	// A backend outside any active profile stays active with no profile names.
	if st["cpu"].EffectiveState != "active" || len(st["cpu"].DisabledByProfiles) != 0 {
		t.Fatalf("cpu = %+v, want active/no profiles", st["cpu"])
	}

	// cooldown loses to profile-disabled (precedence).
	p.ReportFailure("1", ClassTransport, 0) // gpu cooldown
	if got := byName()["gpu"].EffectiveState; got != "profile-disabled" {
		t.Fatalf("gpu state under cooldown+profile = %q, want profile-disabled", got)
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

// TestBuildDisabledBy_SortedAndActiveOnly ist der konstruktive Beweis gegen die
// diffKey-Instabilität (§4.1): buildDisabledBy sortiert die gejointen
// Profil-Namen explizit und berücksichtigt NUR aktive Profile. Zwei Membership-
// Reihenfolgen, die dasselbe Set beschreiben, ergeben byte-identische Map-Werte
// — es gibt keinen Pfad, der eine Go-Map-Iterationsordnung nach außen trägt.
func TestBuildDisabledBy_SortedAndActiveOnly(t *testing.T) {
	profiles := []Profile{
		{ID: "pa", Name: "alpha", Active: true},
		{ID: "pz", Name: "zeta", Active: true},
		{ID: "pm", Name: "mid", Active: false}, // inaktiv → darf nie erscheinen
	}
	// Memberships absichtlich in "falscher" Reihenfolge: zeta vor alpha, plus
	// das inaktive mid — das aktive Set für backend "b1" ist {alpha,zeta}.
	memberships := []profileMembership{
		{profileID: "pz", backendID: "b1"},
		{profileID: "pm", backendID: "b1"},
		{profileID: "pa", backendID: "b1"},
		{profileID: "pa", backendID: "b2"},
	}
	got := buildDisabledBy(profiles, memberships)

	if got["b1"] != "alpha,zeta" {
		t.Fatalf("b1 disabledBy = %q, want %q (sortiert, aktiv-only)", got["b1"], "alpha,zeta")
	}
	if got["b2"] != "alpha" {
		t.Fatalf("b2 disabledBy = %q, want %q", got["b2"], "alpha")
	}
	// Ein Backend ohne aktive Profil-Zugehörigkeit ist ABWESEND (Go-Zero "").
	if _, ok := got["b3"]; ok {
		t.Fatalf("b3 darf nicht in disabledBy stehen (kein aktives Profil): %v", got)
	}

	// Stabilitätsprobe: eine permutierte Membership-Liste liefert byte-identisch.
	perm := []profileMembership{
		{profileID: "pa", backendID: "b2"},
		{profileID: "pa", backendID: "b1"},
		{profileID: "pz", backendID: "b1"},
		{profileID: "pm", backendID: "b1"},
	}
	got2 := buildDisabledBy(profiles, perm)
	if got["b1"] != got2["b1"] || got["b2"] != got2["b2"] {
		t.Fatalf("buildDisabledBy nicht ordnungsstabil: %v vs %v", got, got2)
	}
}
