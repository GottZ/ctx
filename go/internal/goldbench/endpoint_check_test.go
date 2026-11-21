package goldbench

import (
	"context"
	"strings"
	"testing"
)

// #459 point 3: an -endpoint without an http or https host fails every chat
// call the same way — the configuration, not a case. The run ends before the
// first call instead of scoring every case 0.
func TestGoldbenchRunStopsOnAnEndpointWithoutHTTPHost(t *testing.T) {
	for _, endpoint := range []string{"localhost:8080", "ftp://bench.example", "//bench.example:8080", "http://"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := Config{Endpoint: endpoint, Model: "m", Concurrency: 1, Seed: 1, TimeoutSec: 5, TempOverride: -1}
			jobs, runs := refusalJobs(3, false, 0)
			err := executeJobs(context.Background(), cfg, jobs, runs, nil)
			if err == nil || !strings.Contains(err.Error(), "-endpoint") {
				t.Fatalf("err = %v, want the run to end naming -endpoint", err)
			}
			for i, r := range runs["refusal"] {
				if r.callErr != nil || r.outputs[0] != "" {
					t.Errorf("case %d was attempted (callErr %v): the check must come before the first call", i, r.callErr)
				}
			}
		})
	}
}
