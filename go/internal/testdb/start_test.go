//go:build integration

package testdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// reaperReuseTimeout builds the error testcontainers-go v0.44.0 returns when
// a container start finds the reaper of an earlier test binary of the same
// session and that reaper is gone before it reports ready: the wrap chain of
// reaper.go fromContainer → reaperSpawner.reaper → DockerContainer.connectReaper
// → GenericContainer → postgres.Run, every layer %w.
func reaperReuseTimeout(id string) error {
	wait := fmt.Errorf("wait for reaper %s: %w", id, errors.Join(nil, context.DeadlineExceeded))
	reuse := fmt.Errorf("from container %q: %w", id, wait)
	reaper := fmt.Errorf("reaper: %w", errors.Join(reuse, nil))
	return fmt.Errorf("run postgres: %w", fmt.Errorf("generic container: %w", fmt.Errorf("create container: %w", reaper)))
}

// fakeStart answers the calls of startContainer from a script of errors; a
// nil entry is a successful start. It counts the calls; a non-nil
// containers[i] is returned with errs[i], as a failed testcontainers start
// returns the container it created before the reaper was consulted.
type fakeStart struct {
	errs       []error
	containers []*tcpostgres.PostgresContainer
	calls      int
	// terminatedBefore records, per call, how many fake containers had been
	// terminated when that call started.
	terminatedBefore []int
	terminated       *int
}

func (f *fakeStart) start(context.Context) (*tcpostgres.PostgresContainer, error) {
	f.calls++
	if f.terminated != nil {
		f.terminatedBefore = append(f.terminatedBefore, *f.terminated)
	}
	if f.calls > len(f.errs) {
		return nil, fmt.Errorf("unexpected start call %d", f.calls)
	}
	var c *tcpostgres.PostgresContainer
	if f.calls <= len(f.containers) {
		c = f.containers[f.calls-1]
	}
	return c, f.errs[f.calls-1]
}

// fakeContainer is a testcontainers.Container whose only implemented method
// is Terminate; any other call panics on the nil embedded interface.
type fakeContainer struct {
	testcontainers.Container
	terminated *int
}

func (c *fakeContainer) Terminate(context.Context, ...testcontainers.TerminateOption) error {
	*c.terminated++
	return nil
}

// The fixture must read exactly like the CI failure (run 43774745584, job
// "Integration Tests (rest)", package internal/recall).
func TestReaperReuseTimeoutFixtureMatchesTheCIText(t *testing.T) {
	const ci = `run postgres: generic container: create container: reaper: from container "9f06f27f": wait for reaper 9f06f27f: context deadline exceeded`
	if got := reaperReuseTimeout("9f06f27f").Error(); got != ci {
		t.Fatalf("fixture text\n got %q\nwant %q", got, ci)
	}
}

func TestStartContainerRetriesOnceAfterReaperReuseTimeout(t *testing.T) {
	terminated := 0
	leftover := &tcpostgres.PostgresContainer{Container: &fakeContainer{terminated: &terminated}}
	f := &fakeStart{
		errs:       []error{reaperReuseTimeout("9f06f27f"), nil},
		containers: []*tcpostgres.PostgresContainer{leftover},
		terminated: &terminated,
	}
	if _, err := startContainer(context.Background(), f.start); err != nil {
		t.Fatalf("startContainer: %v", err)
	}
	if f.calls != 2 {
		t.Fatalf("start calls = %d, want 2 (one retry after the reaper reuse timeout)", f.calls)
	}
	// The container the failed attempt created is removed before the retry
	// creates its own.
	if len(f.terminatedBefore) != 2 || f.terminatedBefore[1] != 1 {
		t.Fatalf("terminated containers before each start = %v, want [0 1]", f.terminatedBefore)
	}
}

func TestStartContainerSecondReaperTimeoutNamesBothAttempts(t *testing.T) {
	f := &fakeStart{errs: []error{reaperReuseTimeout("aaaaaaaa"), reaperReuseTimeout("bbbbbbbb")}}
	_, err := startContainer(context.Background(), f.start)
	if err == nil {
		t.Fatal("startContainer: nil error after two failed attempts")
	}
	if f.calls != 2 {
		t.Fatalf("start calls = %d, want 2", f.calls)
	}
	msg := err.Error()
	for _, want := range []string{"attempt 1 of 2", `from container "aaaaaaaa"`, "attempt 2 of 2", `from container "bbbbbbbb"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %q lost context.DeadlineExceeded from its chain", msg)
	}
}

func TestStartContainerDoesNotRetryOtherErrors(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"docker unreachable", context.Background(), errors.New("run postgres: generic container: create container: ensure default network: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")},
		{"image missing", context.Background(), errors.New("run postgres: generic container: create container: pull image: Error response from daemon: manifest unknown")},
		{"postgres never ready", context.Background(), fmt.Errorf("run postgres: generic container: start container: started hook: wait until ready: %w", context.DeadlineExceeded)},
		{"reaper start of a new reaper", context.Background(), fmt.Errorf("run postgres: generic container: create container: reaper: new reaper: run container: %w", context.DeadlineExceeded)},
		{"reused reaper exited", context.Background(), errors.New(`run postgres: generic container: create container: reaper: from container "9f06f27f": wait for reaper 9f06f27f: mapped port: check target: retries: 1, port: "", last err: container exited with code 0`)},
		{"outer budget spent", expired, reaperReuseTimeout("9f06f27f")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStart{errs: []error{tc.err}}
			_, err := startContainer(tc.ctx, f.start)
			if f.calls != 1 {
				t.Fatalf("start calls = %d, want 1 (no retry)", f.calls)
			}
			if !errors.Is(err, tc.err) || err.Error() != tc.err.Error() {
				t.Fatalf("error = %v, want the start error unchanged: %v", err, tc.err)
			}
		})
	}
}

func TestStartContainerFirstSuccessStartsOnce(t *testing.T) {
	f := &fakeStart{errs: []error{nil}}
	if _, err := startContainer(context.Background(), f.start); err != nil {
		t.Fatalf("startContainer: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("start calls = %d, want 1", f.calls)
	}
}
