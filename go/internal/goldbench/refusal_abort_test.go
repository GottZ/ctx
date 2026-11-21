package goldbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/clientkey"
	"github.com/GottZ/ctx/internal/llm"
)

// A refused redirect is the configuration of -endpoint, not a property of one
// case: every chat call goes to the same URL, so every one of them is refused
// the same way. Scoring those cases 0 and going on would report a
// misconfigured endpoint as a model that scores 0 (#446). ctx-goldbench stops
// the run on the first refusal instead, as ctx-armsweep does.

const refusalAnswer = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":1}}`

// refusalJobs builds n one-slot cases of one axis, shaped as buildRuns shapes
// them for a run that is not a dry run.
func refusalJobs(n int, samples bool, temperature float64) ([]job, map[string][]caseRun) {
	runs := make([]caseRun, n)
	jobs := make([]job, n)
	for i := range runs {
		reqs := []ChatRequest{{System: "s", User: "the gold case's prompt", Opts: SamplingOpts{Temperature: temperature}}}
		runs[i] = caseRun{c: &Case{ID: fmt.Sprintf("c%d", i)}, reqs: reqs,
			outputs: make([]string, 1), usages: make([]CallUsage, 1), tops: make([][]llm.TokenLogprob, 1)}
		if samples {
			runs[i].samples = make([][]string, 1)
			runs[i].samplesUsage = make([][]CallUsage, 1)
		}
		jobs[i] = job{axis: "refusal", idx: i, reqs: reqs}
	}
	return jobs, map[string][]caseRun{"refusal": runs}
}

// TestGoldbenchRunStopsOnARefusedRedirect: the configured server answers the
// first chat call with a redirect to another origin. The run ends with the
// refusal — naming the axis, the case and where the redirect pointed — after
// that one call, and the other server sees nothing; with a key and without.
//
// RED before #446: the refusal (with a key) or the followed redirect (without
// one) failed only its case, which scored 0, and the run went on through every
// case.
func TestGoldbenchRunStopsOnARefusedRedirect(t *testing.T) {
	for _, key := range []string{"", "0f1e2d3c4b5a69788796a5b4c3d2e1f0"} {
		t.Run(map[bool]string{true: "with key", false: "without key"}[key != ""], func(t *testing.T) {
			var otherSeen, homeSeen atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				otherSeen.Add(1)
				_, _ = io.WriteString(w, refusalAnswer)
			}))
			t.Cleanup(other.Close)
			home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				homeSeen.Add(1)
				http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			}))
			t.Cleanup(home.Close)

			cfg := Config{Endpoint: home.URL, Model: "m", APIKey: key, Concurrency: 1, Seed: 1, TimeoutSec: 5, TempOverride: -1}
			jobs, runs := refusalJobs(5, false, 0)
			err := executeJobs(context.Background(), cfg, jobs, runs, nil)

			if !errors.As(err, new(*clientkey.Refusal)) {
				t.Fatalf("err = %v, want the run to end with the refusal", err)
			}
			for _, want := range []string{"run abgebrochen", "refusal/c0", "refused a redirect (HTTP 307) to " + other.URL, "-endpoint"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err %q does not say %q", err, want)
				}
			}
			if n := homeSeen.Load(); n != 1 {
				t.Errorf("the configured server saw %d chat calls, want the run to stop after the first", n)
			}
			if n := otherSeen.Load(); n != 0 {
				t.Errorf("the other server saw %d requests", n)
			}
		})
	}
}

// TestGoldbenchRunStopsOnARefusedRedirectWithWorkers: with four workers the
// first refusal still ends the run — before the cases run out — and no worker
// sends anything to the other server.
func TestGoldbenchRunStopsOnARefusedRedirectWithWorkers(t *testing.T) {
	var otherSeen, homeSeen atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherSeen.Add(1)
		_, _ = io.WriteString(w, refusalAnswer)
	}))
	t.Cleanup(other.Close)
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		homeSeen.Add(1)
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}))
	t.Cleanup(home.Close)

	const cases = 200
	cfg := Config{Endpoint: home.URL, Model: "m", Concurrency: 4, Seed: 1, TimeoutSec: 5, TempOverride: -1}
	jobs, runs := refusalJobs(cases, false, 0)
	err := executeJobs(context.Background(), cfg, jobs, runs, nil)

	if !errors.As(err, new(*clientkey.Refusal)) {
		t.Fatalf("err = %v, want the run to end with the refusal", err)
	}
	if n := homeSeen.Load(); n >= cases {
		t.Errorf("the configured server saw %d chat calls for %d cases — the run did not stop", n, cases)
	}
	if n := otherSeen.Load(); n != 0 {
		t.Errorf("the other server saw %d requests", n)
	}
}

// TestGoldbenchRunStopsOnARefusedRedirectOfALaterSample: a refusal of sample
// s>0 (-samples, temperature above 0) stops the run as well, not only a
// refusal of the first call of a slot.
//
// RED before #446: a failed sample s>0 only marked its case incomplete.
func TestGoldbenchRunStopsOnARefusedRedirectOfALaterSample(t *testing.T) {
	var otherSeen, homeSeen atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherSeen.Add(1)
		_, _ = io.WriteString(w, refusalAnswer)
	}))
	t.Cleanup(other.Close)
	const seed = 7
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		homeSeen.Add(1)
		var body struct {
			Seed int64 `json:"seed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Seed == seed+1 { // sample s=1
			http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, refusalAnswer)
	}))
	t.Cleanup(home.Close)

	cfg := Config{Endpoint: home.URL, Model: "m", APIKey: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Concurrency: 1,
		Seed: seed, Samples: 2, TimeoutSec: 5, TempOverride: -1}
	jobs, runs := refusalJobs(3, true, 0.7)
	err := executeJobs(context.Background(), cfg, jobs, runs, nil)

	if !errors.As(err, new(*clientkey.Refusal)) {
		t.Fatalf("err = %v, want the run to end with the refusal of sample 1", err)
	}
	if n := homeSeen.Load(); n != 2 {
		t.Errorf("the configured server saw %d chat calls, want 2 (sample 0, then the refused sample 1)", n)
	}
	if n := otherSeen.Load(); n != 0 {
		t.Errorf("the other server saw %d requests", n)
	}
}
