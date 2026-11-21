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
	"testing"

	"github.com/GottZ/ctx/internal/clientkey"
	"github.com/GottZ/ctx/internal/llm"
)

func transportErrorJobs(n int) ([]job, map[string][]caseRun) {
	runs := make([]caseRun, n)
	jobs := make([]job, n)
	for i := range runs {
		reqs := []ChatRequest{{System: "s", User: fmt.Sprintf("case-%d", i)}}
		runs[i] = caseRun{c: &Case{ID: fmt.Sprintf("c%d", i)}, reqs: reqs,
			outputs: make([]string, 1), usages: make([]CallUsage, 1), tops: make([][]llm.TokenLogprob, 1)}
		jobs[i] = job{axis: "p", idx: i, reqs: reqs}
	}
	return jobs, map[string][]caseRun{"p": runs}
}

// TestTransportErrorsDoNotAbort: only a refused redirect ends the run (#446).
// An HTTP 500, a connection closed without an answer and a redirect loop
// inside the origin stay errors of their case — the case scores 0, the run
// goes on, and none of them is a clientkey.Refusal. With and without a key.
func TestTransportErrorsDoNotAbort(t *testing.T) {
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasPrefix(r.URL.Path, "/loop") {
			http.Redirect(w, r, "/loop"+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		if len(body.Messages) < 2 {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		switch body.Messages[1].Content {
		case "case-1":
			http.Error(w, "broken", http.StatusInternalServerError)
		case "case-2":
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = c.Close()
			}
		case "case-3":
			http.Redirect(w, r, "/loop/x", http.StatusTemporaryRedirect)
		default:
			_, _ = io.WriteString(w, refusalAnswer)
		}
	}))
	t.Cleanup(home.Close)
	for _, key := range []string{"", "0f1e2d3c4b5a69788796a5b4c3d2e1f0"} {
		jobs, runs := transportErrorJobs(6)
		cfg := Config{Endpoint: home.URL, Model: "m", APIKey: key, Concurrency: 2, Seed: 1, TimeoutSec: 5, TempOverride: -1}
		if err := executeJobs(context.Background(), cfg, jobs, runs, nil); err != nil {
			t.Errorf("key %t: the run ended on an ordinary transport error: %v", key != "", err)
		}
		for i, r := range runs["p"] {
			if errors.As(r.callErr, new(*clientkey.Refusal)) {
				t.Errorf("key %t, c%d: an ordinary error is reported as a refusal: %v", key != "", i, r.callErr)
			}
			if failed := i >= 1 && i <= 3; failed != (r.callErr != nil) {
				t.Errorf("key %t, c%d: callErr = %v, want an error: %t", key != "", i, r.callErr, failed)
			}
		}
	}
}
