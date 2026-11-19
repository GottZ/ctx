package config

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestSnapshotForTenantResolvedWaitEndsWithTheCaller (issue #372, part 4):
// the caller's context bounds its wait for a tenant generation that has to be
// built, not the build. A caller whose deadline passes gets the base and
// false at its deadline; the build goes on, on a context the caller's end
// does not cancel, and its generation is cached, so the next call is a hit
// without a second build.
//
// RED before the fix: the call waited for the whole build (here released
// after 1 s), and the build ran on the caller's context, which its deadline
// canceled.
func TestSnapshotForTenantResolvedWaitEndsWithTheCaller(t *testing.T) {
	base := generation(t, "base")
	tcfg := generation(t, "tenant")
	s := NewStore(base)
	release := make(chan struct{})
	var calls atomic.Int32
	buildErr := make(chan error, 1)
	s.SetOverlay(func(ctx context.Context, _ *Config, _ string) (*Config, error) {
		calls.Add(1)
		<-release
		buildErr <- ctx.Err()
		return tcfg, nil
	})
	timer := time.AfterFunc(time.Second, func() { close(release) })
	defer timer.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, resolved := s.SnapshotForTenantResolved(ctx, "acme")
	waited := time.Since(start)
	if got != base || resolved {
		t.Errorf("after the caller's deadline: %s, %v; want the base and false", brandOf(got)[0], resolved)
	}
	if waited > 500*time.Millisecond {
		t.Errorf("the caller waited %v for a build its 50 ms deadline does not cover", waited)
	}

	select {
	case err := <-buildErr:
		if err != nil {
			t.Errorf("the build's context ended with the caller's deadline: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the build never finished")
	}
	// The build stores its generation after the overlay returns.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, resolved = s.SnapshotForTenantResolved(context.Background(), "acme")
		if resolved || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != tcfg || !resolved {
		t.Errorf("after the build: %s, %v; want the tenant generation, resolved", brandOf(got)[0], resolved)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("overlay calls = %d, want 1: the generation the abandoned wait left behind is the cache", n)
	}
}

// TestSnapshotForTenantResolvedSharesTheBuild: callers that ask while one
// build runs share it, whatever their own deadlines; one whose deadline is
// long enough gets the generation.
func TestSnapshotForTenantResolvedSharesTheBuild(t *testing.T) {
	base := generation(t, "base")
	tcfg := generation(t, "tenant")
	s := NewStore(base)
	var calls atomic.Int32
	s.SetOverlay(func(ctx context.Context, _ *Config, _ string) (*Config, error) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond)
		return tcfg, nil
	})
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	done := make(chan bool, 1)
	go func() {
		got, resolved := s.SnapshotForTenantResolved(context.Background(), "acme")
		done <- got == tcfg && resolved
	}()
	time.Sleep(10 * time.Millisecond)
	if got, resolved := s.SnapshotForTenantResolved(short, "acme"); got != base || resolved {
		t.Errorf("the short caller: %s, %v; want the base and false", brandOf(got)[0], resolved)
	}
	if !<-done {
		t.Error("the caller without a deadline did not get the tenant generation: the short caller's end ended the shared build")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("overlay calls = %d, want 1 (one build for both callers)", n)
	}
}

// TestSnapshotForTenantResolvedRaisesAPanicInTheCaller: an overlay that
// panics panics in the caller that waits, as it did when the caller ran the
// build itself, so the request path's recovery answers it.
func TestSnapshotForTenantResolvedRaisesAPanicInTheCaller(t *testing.T) {
	s := NewStore(generation(t, "base"))
	s.SetOverlay(func(context.Context, *Config, string) (*Config, error) { panic("probe: overlay bug") })
	defer func() {
		if p := recover(); p == nil {
			t.Error("no panic reached the caller")
		}
	}()
	s.SnapshotForTenantResolved(context.Background(), "acme")
}
