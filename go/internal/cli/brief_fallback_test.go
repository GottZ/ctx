package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBriefBlock is one agent-briefing row the fake /api/search serves.
type fakeBriefBlock struct {
	id    string
	title string
	tags  []string
}

// newFakeBriefSearch models the real handler's empty-query browse contract:
// category and server-side overlap-tag filters, limit default 10/cap 50,
// compact defaulting to true, and success:false error envelopes. Every result
// carries tags, as store.BlockPreview does. Blocks are given newest first.
func newFakeBriefSearch(t *testing.T, blocks []fakeBriefBlock, hits *atomic.Int32, failingTags ...string) *httptest.Server {
	t.Helper()
	fail := map[string]bool{}
	for _, tag := range failingTags {
		fail[tag] = true
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			hits.Add(1)
		}
		var req struct {
			Category string   `json:"category"`
			Tags     []string `json:"tags"`
			Limit    int      `json:"limit"`
			Compact  *bool    `json:"compact"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad search body: %v", err)
			return
		}
		compact := true
		if req.Compact != nil {
			compact = *req.Compact
		}
		if compact {
			t.Errorf("brief must request compact:false so content is returned")
		}
		w.Header().Set("Content-Type", "application/json")
		if len(req.Tags) == 1 {
			tag := req.Tags[0]
			if fail["transport:"+tag] {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("response writer cannot hijack transport")
					return
				}
				conn, _, err := hijacker.Hijack()
				if err != nil {
					t.Errorf("hijack transport: %v", err)
					return
				}
				_ = conn.Close()
				return
			}
			if fail["malformed:"+tag] {
				_, _ = w.Write([]byte("{not-json"))
				return
			}
			if fail[tag] {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": false, "error": "injected search failure",
				})
				return
			}
		}
		limit := req.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		results := []map[string]any{}
		for i, b := range blocks {
			if req.Category != briefCategory || !hasAnyTag(b.tags, req.Tags) {
				continue
			}
			tags := b.tags
			if tags == nil {
				tags = []string{}
			}
			results = append(results, map[string]any{
				"id": b.id, "category": briefCategory, "title": b.title,
				"content": "content of " + b.title, "tags": tags,
				"updated_at": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Minute),
			})
			if len(results) == limit {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "count": len(results), "compact": compact,
			"results": results, "next_after": nil,
		})
	}))
}

func hasAnyTag(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		for _, h := range have {
			if h == w {
				return true
			}
		}
	}
	return false
}

func runBriefAgainst(t *testing.T, srv *httptest.Server, hook bool, limit int, project string) string {
	t.Helper()
	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	return captureStdout(t, func() {
		if err := runBrief(getClient, hook, limit, project); err != nil {
			t.Errorf("runBrief: %v", err)
		}
	})
}

// TestBriefFallbackRequiresGlobalTag pins #58: untagged content is not global
// evidence. Only the producer-set marker makes a block eligible for fallback.
func TestBriefFallbackRequiresGlobalTag(t *testing.T) {
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Other project briefing", tags: []string{"/srv/other"}},
		{id: "b2", title: "Untagged foreign briefing"},
		{id: "b3", title: "Topic-only briefing", tags: []string{"topic:operations"}},
		{id: "b4", title: "Global briefing", tags: []string{briefGlobalTag, "/srv/other"}},
	}, nil)
	defer srv.Close()

	for _, hook := range []bool{false, true} {
		t.Run(fmt.Sprintf("hook=%v", hook), func(t *testing.T) {
			out := runBriefAgainst(t, srv, hook, 20, "/srv/mine")
			if !strings.Contains(out, "Global briefing") {
				t.Errorf("fallback lost the marked global block:\n%s", out)
			}
			for _, unwanted := range []string{"Other project briefing", "Untagged foreign briefing", "Topic-only briefing"} {
				if strings.Contains(out, unwanted) {
					t.Errorf("fallback injected %s:\n%s", unwanted, out)
				}
			}
		})
	}
}

func TestBriefProjectBlocksWin(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Mine", tags: []string{"/srv/mine", briefGlobalTag}},
		{id: "b2", title: "Global briefing", tags: []string{briefGlobalTag}},
	}, &hits)
	defer srv.Close()

	out := runBriefAgainst(t, srv, false, 20, "/srv/mine")
	if !strings.Contains(out, "--- Mine ---") || strings.Contains(out, "Global briefing") {
		t.Errorf("want only the project's block:\n%s", out)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("project hit must not run fallback: requests = %d, want 1", got)
	}
}

func TestBriefFallbackUsesOneRequest(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Global briefing", tags: []string{briefGlobalTag}},
	}, &hits)
	defer srv.Close()

	out := runBriefAgainst(t, srv, true, 20, "/srv/mine")
	if !strings.Contains(out, "Global briefing") {
		t.Fatalf("fallback output missing global block:\n%s", out)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("project miss + one global request = 2, got %d", got)
	}
}

type limitRecordingTransport struct {
	base   http.RoundTripper
	limits *[]int
}

func (t *limitRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		var payload struct {
			Limit int `json:"limit"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		*t.limits = append(*t.limits, payload.Limit)
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	return t.base.RoundTrip(r)
}

// TestBriefLimitClampedToFiftyOnBothPaths checks the resulting block count.
func TestBriefLimitClampedToFiftyOnBothPaths(t *testing.T) {
	blocks := make([]fakeBriefBlock, 0, 120)
	for i := 0; i < 60; i++ {
		blocks = append(blocks, fakeBriefBlock{
			id: fmt.Sprintf("p%02d", i), title: fmt.Sprintf("Project %02d", i), tags: []string{"/srv/mine"},
		})
	}
	for i := 0; i < 60; i++ {
		blocks = append(blocks, fakeBriefBlock{
			id: fmt.Sprintf("g%02d", i), title: fmt.Sprintf("Global %02d", i), tags: []string{briefGlobalTag},
		})
	}
	for _, tc := range []struct {
		name    string
		project string
	}{
		{name: "project", project: "/srv/mine"},
		{name: "global fallback", project: "/srv/missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeBriefSearch(t, blocks, nil)
			defer srv.Close()
			out := runBriefAgainst(t, srv, false, 100, tc.project)
			if !strings.Contains(out, "(50 blocks)") {
				t.Errorf("--limit 100 must be clamped to the REST ceiling 50:\n%s", out)
			}
		})
	}
}

func TestBriefOutgoingLimitUsesClampAndDefault(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inputLimit   int
		wantLimit    int
		project      string
		wantRequests int
	}{
		{name: "project clamp", inputLimit: 100, wantLimit: 50, project: "/srv/mine", wantRequests: 1},
		{name: "global clamp", inputLimit: 100, wantLimit: 50, project: "/srv/missing", wantRequests: 2},
		{name: "project default", inputLimit: 0, wantLimit: 10, project: "/srv/mine", wantRequests: 1},
		{name: "global default", inputLimit: 0, wantLimit: 10, project: "/srv/missing", wantRequests: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeBriefSearch(t, []fakeBriefBlock{
				{id: "p1", title: "Mine", tags: []string{"/srv/mine"}},
				{id: "g1", title: "Global", tags: []string{briefGlobalTag}},
			}, nil)
			defer srv.Close()
			limits := []int{}
			client := srv.Client()
			client.Transport = &limitRecordingTransport{base: client.Transport, limits: &limits}
			getClient := func() (*Client, error) {
				return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: client}, nil
			}
			_ = captureStdout(t, func() {
				if err := runBrief(getClient, false, tc.inputLimit, tc.project); err != nil {
					t.Errorf("runBrief: %v", err)
				}
			})
			if len(limits) != tc.wantRequests {
				t.Fatalf("requests = %d, want %d (limits %v)", len(limits), tc.wantRequests, limits)
			}
			for _, got := range limits {
				if got != tc.wantLimit {
					t.Errorf("outgoing limit = %d, want %d (all %v)", got, tc.wantLimit, limits)
				}
			}
		})
	}
}

func TestBriefSearchSeparatesEmptyAndError(t *testing.T) {
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Global briefing", tags: []string{briefGlobalTag}},
	}, nil, "/srv/error")
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}

	blocks, err := searchBriefBlocks(c, 20, "/srv/missing")
	if err != nil || len(blocks) != 0 {
		t.Errorf("legitimate empty result: blocks=%v err=%v", blocks, err)
	}
	blocks, err = searchBriefBlocks(c, 20, "/srv/error")
	if err == nil {
		t.Errorf("failed request must return an error, got blocks=%v", blocks)
	}
}

func TestBriefFailedProjectRequestFallsBack(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Global briefing", tags: []string{briefGlobalTag}},
	}, &hits, "/srv/error")
	defer srv.Close()

	out := runBriefAgainst(t, srv, false, 20, "/srv/error")
	if !strings.Contains(out, "Global briefing") {
		t.Errorf("project transport/API failure must still reach global fallback:\n%s", out)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("failed project request + global request = 2, got %d", got)
	}
}

func TestBriefResponseFailuresFallBack(t *testing.T) {
	for _, failure := range []string{"transport", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			var hits atomic.Int32
			srv := newFakeBriefSearch(t, []fakeBriefBlock{
				{id: "b1", title: "Global briefing", tags: []string{briefGlobalTag}},
			}, &hits, failure+":/srv/error")
			defer srv.Close()

			out := runBriefAgainst(t, srv, false, 20, "/srv/error")
			if !strings.Contains(out, "Global briefing") {
				t.Errorf("%s failure must reach global fallback:\n%s", failure, out)
			}
			if got := hits.Load(); got != 2 {
				t.Errorf("failed project request + global request = 2, got %d", got)
			}
		})
	}
}

func TestBriefFailedGlobalRequestProducesNoOutput(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeBriefSearch(t, nil, &hits, briefGlobalTag)
	defer srv.Close()

	out := runBriefAgainst(t, srv, true, 20, "/srv/mine")
	if out != "" {
		t.Errorf("failed project and global requests must not invent output:\n%s", out)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("failed project + failed global = 2, got %d", got)
	}
}
