package cli

import (
	"encoding/json"
	"fmt"
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

// newFakeBriefSearch serves POST /api/search the way the real handler answers
// the empty-query browse `ctx brief` sends (go/internal/handler/context_search.go):
// category filter, tag filter (the block carries one of the requested tags),
// limit capped at 50, newest first, a keyset cursor `next_after`
// {after_updated, after_id} on a full page that the next request sends back as
// `after`, and every result carrying its `tags` (store.BlockPreview).
// blocks are given newest first.
func newFakeBriefSearch(t *testing.T, blocks []fakeBriefBlock, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updated := func(i int) time.Time { return base.Add(-time.Duration(i) * time.Minute) }
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
			After    *struct {
				ID string `json:"after_id"`
			} `json:"after"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad search body: %v", err)
			return
		}
		limit := req.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		start := 0
		if req.After != nil {
			for i, b := range blocks {
				if b.id == req.After.ID {
					start = i + 1
				}
			}
		}
		results := []map[string]any{}
		var next any
		for i := start; i < len(blocks); i++ {
			b := blocks[i]
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
				"updated_at": updated(i),
			})
			if len(results) == limit {
				next = map[string]any{"after_updated": updated(i), "after_id": b.id}
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "count": len(results), "results": results, "next_after": next,
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

// TestBriefFallbackIsUntaggedOnly pins issue #58: a project without its own
// agent-briefing blocks falls back to the UNTAGGED ones, as the command's Long
// text says, and never to a block tagged for another project. Both output
// modes go through the same block selection.
func TestBriefFallbackIsUntaggedOnly(t *testing.T) {
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Other project briefing", tags: []string{"/srv/other"}},
		{id: "b2", title: "General briefing"},
	}, nil)
	defer srv.Close()

	for _, hook := range []bool{false, true} {
		t.Run(fmt.Sprintf("hook=%v", hook), func(t *testing.T) {
			out := runBriefAgainst(t, srv, hook, 20, "/srv/mine")
			if !strings.Contains(out, "General briefing") {
				t.Errorf("fallback lost the untagged block:\n%s", out)
			}
			if strings.Contains(out, "Other project briefing") {
				t.Errorf("fallback injected another project's briefing:\n%s", out)
			}
		})
	}
}

// TestBriefProjectBlocksWin: when the project has tagged blocks, only those are
// used and the fallback does not run.
func TestBriefProjectBlocksWin(t *testing.T) {
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Mine", tags: []string{"/srv/mine"}},
		{id: "b2", title: "General briefing"},
	}, nil)
	defer srv.Close()

	out := runBriefAgainst(t, srv, false, 20, "/srv/mine")
	if !strings.Contains(out, "--- Mine ---") || strings.Contains(out, "General briefing") {
		t.Errorf("want only the project's block:\n%s", out)
	}
}

// TestBriefFallbackPagesPastTaggedBlocks: the untagged filter runs on the
// client, so pages full of other projects' blocks must not starve the
// fallback. It pages on until it has `limit` untagged blocks or the store
// runs out.
func TestBriefFallbackPagesPastTaggedBlocks(t *testing.T) {
	var blocks []fakeBriefBlock
	for i := 0; i < 120; i++ {
		blocks = append(blocks, fakeBriefBlock{
			id: fmt.Sprintf("t%03d", i), title: fmt.Sprintf("Tagged %03d", i), tags: []string{"/srv/other"},
		})
	}
	blocks = append(blocks,
		fakeBriefBlock{id: "u1", title: "Untagged one"},
		fakeBriefBlock{id: "u2", title: "Untagged two"},
		fakeBriefBlock{id: "u3", title: "Untagged three"},
	)
	srv := newFakeBriefSearch(t, blocks, nil)
	defer srv.Close()

	out := runBriefAgainst(t, srv, false, 2, "/srv/mine")
	if !strings.Contains(out, "(2 blocks)") || !strings.Contains(out, "Untagged one") || !strings.Contains(out, "Untagged two") {
		t.Errorf("want the two newest untagged blocks:\n%s", out)
	}
	if strings.Contains(out, "Tagged 0") || strings.Contains(out, "Untagged three") {
		t.Errorf("want nothing beyond the two newest untagged blocks:\n%s", out)
	}
}

// TestBriefNoProjectUsesUntagged: with no project left after resolution there
// is no tag to scope by, so the result is the untagged set, not every briefing
// block the key can read.
func TestBriefNoProjectUsesUntagged(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Other project briefing", tags: []string{"/srv/other"}},
		{id: "b2", title: "General briefing"},
	}, &hits)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}
	blocks := searchUntaggedBriefBlocks(c, 20)
	if len(blocks) != 1 || blocks[0].Title != "General briefing" {
		t.Errorf("want only the untagged block, got %+v", blocks)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("want 1 search request for a single short page, got %d", got)
	}
}

// TestBriefFallbackPageCap: a category with no untagged block among its newest
// briefMaxPages*briefPageSize rows ends the fallback after briefMaxPages
// requests with nothing, instead of walking the whole store from a hook.
func TestBriefFallbackPageCap(t *testing.T) {
	var blocks []fakeBriefBlock
	for i := 0; i < briefMaxPages*briefPageSize+5; i++ {
		blocks = append(blocks, fakeBriefBlock{
			id: fmt.Sprintf("t%04d", i), title: fmt.Sprintf("Tagged %04d", i), tags: []string{"/srv/other"},
		})
	}
	blocks = append(blocks, fakeBriefBlock{id: "u1", title: "Untagged beyond the cap"})
	var hits atomic.Int32
	srv := newFakeBriefSearch(t, blocks, &hits)
	defer srv.Close()

	out := runBriefAgainst(t, srv, true, 20, "/srv/mine")
	if out != "" {
		t.Errorf("want no hook output past the page cap, got:\n%s", out)
	}
	// 1 project-scoped request + briefMaxPages fallback pages.
	if got, want := hits.Load(), int32(1+briefMaxPages); got != want {
		t.Errorf("search requests = %d, want %d", got, want)
	}
}
