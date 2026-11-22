//go:build integration

// Issue #492, end to end: what a new installation's Dream does before and
// after the operator seeds the backend pool — in ONE running ctxd, no
// restart in between. The child is the production main() booted against an
// empty database through the harness of dream_install_default_integration_test.go
// (startDreamBootChild), with CTX_DREAM_ENABLED unset: Dream runs on the
// installation's default.
//
//   - Phase A (no backend of role dream/chat): one real minute at the default
//     dream.idle_wait (20 s). The loop must skip before the pick at idle
//     cadence — no busy loop, no ERROR line naming Dream — and /health says
//     what an operator sees.
//
//   - Phase B (same child): an OpenAI-wire stub serves embeddings and the
//     dream chat stages; the pool is seeded with exactly the rows
//     `ctx backends seed` writes (POST /api/manage backend-create), two
//     related blocks are stored over POST /api/store, and the dream loop must
//     write a context_dream_links row without the child being restarted.
//
// Run it alone (about 90 s: one boot, one minute of phase A, phase B):
//
//	go test -tags=integration -p 1 -count=1 -v -run 'TestDreamFreshInstallE2E' ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

const (
	// dreamSkipNoBackendLog is the line RunDreamCycle's step 0 logs when no
	// backend serves the dream role (internal/dream/dream.go).
	dreamSkipNoBackendLog = `"msg":"dream: no eligible dream backend`
	// phaseAWindow is the observation window of phase A: one real minute.
	phaseAWindow = 60 * time.Second
	// phaseAMaxSkipsPerWindow is the busy-loop gate: at the default idle wait
	// of 20 s one worker skips about 3 times a minute; more than twice that is
	// a loop that does not take its idle wait.
	phaseAMaxSkipsPerWindow = 6
	// phaseBLinkDeadline bounds the wait from the seed to the first link.
	phaseBLinkDeadline = 4 * time.Minute
	// stubEmbedDims is the dimension of context_blocks.embedding.
	stubEmbedDims = 1024
	// stubEmbedComponent is the magnitude of every component of the stub's
	// one vector, sign alternating: all inputs embed to the same point, so
	// every block is a vector-arm candidate of every keyword. 1/32 over 1024
	// dimensions gives norm 1 and variance 1/1024 — the embed quality gate
	// (internal/embed qualityGate: norm in [0.99, 1.01], variance ≥ 0.0001)
	// refuses a constant vector such as all components 0.02.
	stubEmbedComponent = 1.0 / 32
)

// freshInstallDSN is the harness's fresh database: no migration history and
// the four extensions init-data.sh creates as superuser.
func freshInstallDSN(t *testing.T) string {
	t.Helper()
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 0)
	if _, err := pool.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS vector;
		CREATE EXTENSION IF NOT EXISTS timescaledb;
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE EXTENSION IF NOT EXISTS pg_trgm`); err != nil {
		t.Fatalf("init-data extensions: %v", err)
	}
	return dsn
}

// phaseAResult is what one observation window of a booted child showed.
type phaseAResult struct {
	skips        int      // dreamSkipNoBackendLog lines in the window
	errorLines   []string // every "level":"ERROR" line in the window
	healthStatus string
	services     map[string]string
}

// observeWindow lets the child run for d and reads what it logged meanwhile
// and what /health says at the end.
func observeWindow(t *testing.T, c *dreamBootChild, d time.Duration) phaseAResult {
	t.Helper()
	from := len(c.out.String())
	time.Sleep(d)
	status, services := c.health(t)
	window := c.out.String()[from:]
	var r phaseAResult
	for _, line := range strings.Split(window, "\n") {
		if strings.Contains(line, dreamSkipNoBackendLog) {
			r.skips++
		}
		if strings.Contains(line, `"level":"ERROR"`) {
			r.errorLines = append(r.errorLines, line)
		}
	}
	r.healthStatus, r.services = status, services
	return r
}

// dreamStub is the OpenAI-wire stand-in for the operator's backend. It
// answers each dream stage with the form its parser accepts and counts the
// calls per stage.
type dreamStub struct {
	embedCalls, embedInputs                         atomic.Int64
	keywords, eval, evalLinks, temporal, recurrence atomic.Int64
	otherChat, otherPath                            atomic.Int64
	mu                                              sync.Mutex
	otherPrompts                                    map[string]int // first line of an unclassified system prompt
	otherPaths                                      map[string]int // method + path of an unserved request
}

// stubCandidateID reads a candidate id out of the link-evaluation prompt
// (dream.buildEvalPrompt: <block id="…" title=…>).
var stubCandidateID = regexp.MustCompile(`<block id="([0-9a-fA-F-]{36})"`)

func newDreamStub() *dreamStub {
	return &dreamStub{otherPrompts: map[string]int{}, otherPaths: map[string]int{}}
}

func (s *dreamStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		s.serveEmbeddings(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.serveChat(w, r)
	default:
		s.otherPath.Add(1)
		s.mu.Lock()
		s.otherPaths[r.Method+" "+r.URL.Path]++
		s.mu.Unlock()
		http.NotFound(w, r)
	}
}

func (s *dreamStub) serveEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n := 1
	if list, ok := req.Input.([]any); ok {
		n = len(list)
	}
	s.embedCalls.Add(1)
	s.embedInputs.Add(int64(n))
	vec := make([]float64, stubEmbedDims)
	for i := range vec {
		vec[i] = stubEmbedComponent
		if i%2 == 1 {
			vec[i] = -stubEmbedComponent
		}
	}
	type entry struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	data := make([]entry, n)
	for i := range data {
		data[i] = entry{Object: "embedding", Index: i, Embedding: vec}
	}
	writeStubJSON(w, map[string]any{
		"object": "list",
		"model":  req.Model,
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": 4 * n, "total_tokens": 4 * n},
	})
}

func (s *dreamStub) serveChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var system, user string
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = m.Content
		case "user":
			user = m.Content
		}
	}
	var content string
	switch {
	case strings.HasPrefix(system, "You extract conceptual keywords"):
		s.keywords.Add(1)
		content = `["dream link probe","fresh installation","backend seed","knowledge graph","semantic neighbour"]`
	case strings.Contains(system, "source block and candidate blocks"):
		s.eval.Add(1)
		type link struct {
			TargetID   string  `json:"target_id"`
			Type       string  `json:"type"`
			Confidence float64 `json:"confidence"`
		}
		links := []link{}
		for _, m := range stubCandidateID.FindAllStringSubmatch(user, 5) {
			links = append(links, link{TargetID: m[1], Type: "topical", Confidence: 0.9})
		}
		s.evalLinks.Add(int64(len(links)))
		raw, _ := json.Marshal(links)
		content = string(raw)
	case strings.HasPrefix(system, "You are a temporal reference extractor"):
		s.temporal.Add(1)
		content = `{"dates":[],"directions":[],"false_positives":[]}`
	case strings.HasPrefix(system, "Classify whether two knowledge blocks form a recurring pattern"):
		s.recurrence.Add(1)
		content = `{"verdict":"none","pattern":"none","confidence":0.9}`
	default:
		s.otherChat.Add(1)
		first, _, _ := strings.Cut(system, "\n")
		if len(first) > 100 {
			first = first[:100]
		}
		s.mu.Lock()
		s.otherPrompts[fmt.Sprintf("stream=%v %q", req.Stream, first)]++
		s.mu.Unlock()
		content = `{}`
	}
	writeStubJSON(w, map[string]any{
		"id":      "stub-1",
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
}

func writeStubJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// report is the per-stage call count, for the log.
func (s *dreamStub) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := func(m map[string]int) []string {
		out := make([]string, 0, len(m))
		for k, v := range m {
			out = append(out, fmt.Sprintf("%s ×%d", k, v))
		}
		sort.Strings(out)
		return out
	}
	return fmt.Sprintf("embeddings %d calls / %d inputs, keywords %d, eval %d (links answered %d), temporal %d, recurrence %d, other chat %d %v, other paths %d %v",
		s.embedCalls.Load(), s.embedInputs.Load(), s.keywords.Load(), s.eval.Load(), s.evalLinks.Load(),
		s.temporal.Load(), s.recurrence.Load(), s.otherChat.Load(), keys(s.otherPrompts), s.otherPath.Load(), keys(s.otherPaths))
}

// adminCall sends one JSON request with the bootstrap admin key and decodes
// the answer into a map.
func (c *dreamBootChild) adminCall(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s body: %v", path, err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://"+c.addr+path, rd)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	req.Header.Set("X-Context-Key", dreamBootAdminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: HTTP %d, undecodable answer %q", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

// seedPayloads are the two backend-create payloads `ctx backends seed` sends
// for a SeedSpec with protocol openai on both legs and no api_key
// (internal/cli/backends_seed.go, seedRowFor): names chat-primary and
// embed-primary, trust full-trust with confirm_trust_elevation, the leg's
// roles in the seed's order, model_map.default, priority 100, scope _global.
func seedPayloads(host string) []map[string]any {
	leg := func(name, model string, roles []string) map[string]any {
		return map[string]any{
			"name":                    name,
			"base_url":                host,
			"protocol":                "openai",
			"trust":                   "full-trust",
			"roles":                   roles,
			"model_map":               map[string]any{"default": map[string]any{"model": model}},
			"priority":                100,
			"scope":                   "_global",
			"confirm_trust_elevation": true,
		}
	}
	return []map[string]any{
		leg("chat-primary", "stub-chat", []string{"synthesis", "translate", "chat", "digest", "dream"}),
		leg("embed-primary", "stub-embed", []string{"embed", "dream-embed"}),
	}
}

func TestDreamFreshInstallE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	dsn := freshInstallDSN(t)

	// CTX_DREAM_ENABLED stays unset: Dream runs on the installation's default.
	child := startDreamBootChild(t, dsn, nil)
	pid := child.cmd.Process.Pid

	if value, source := child.settingSource(t, "dream.enabled"); value != true || source != "install" {
		t.Fatalf("dream.enabled = %v (source %q), want true from the installation's default (source \"install\")", value, source)
	}

	// ---- Phase A: Dream on, no backend of role dream/chat.
	a := observeWindow(t, child, phaseAWindow)
	t.Logf("phase A: %d × %q in %s; /health %q services %v; %d ERROR line(s)",
		a.skips, dreamSkipNoBackendLog, phaseAWindow, a.healthStatus, a.services, len(a.errorLines))
	for _, l := range a.errorLines {
		t.Logf("phase A ERROR: %s", l)
	}
	if a.skips == 0 {
		t.Errorf("phase A: no %q line in %s — the dream loop did not run its backend probe", dreamSkipNoBackendLog, phaseAWindow)
	}
	if a.skips > phaseAMaxSkipsPerWindow {
		t.Errorf("phase A: %d skips in %s, want at most %d — the loop does not take its idle wait", a.skips, phaseAWindow, phaseAMaxSkipsPerWindow)
	}
	for _, l := range a.errorLines {
		if strings.Contains(strings.ToLower(l), "dream") {
			t.Errorf("phase A: ERROR line naming dream: %s", l)
		}
	}

	// ---- Phase B: seed without a restart, then links.
	stub := newDreamStub()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	links, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool on the child's database: %v", err)
	}
	t.Cleanup(links.Close)
	countLinks := func() int {
		var n int
		if err := links.QueryRow(ctx, `SELECT count(*) FROM context_dream_links`).Scan(&n); err != nil {
			t.Fatalf("count context_dream_links: %v", err)
		}
		return n
	}
	if n := countLinks(); n != 0 {
		t.Fatalf("context_dream_links holds %d rows before the seed", n)
	}

	phaseBFrom := len(child.out.String())
	seedAt := time.Now()

	// The seed's two pre-checks (whoami: server-admin; backend-list: the pool
	// holds no foreign row), then one backend-create per leg.
	if st, who := child.adminCall(t, http.MethodGet, "/api/whoami", nil); st != http.StatusOK || who["admin"] != true {
		t.Fatalf("whoami: HTTP %d %v, want a server-admin key", st, who)
	}
	if st, list := child.adminCall(t, http.MethodPost, "/api/manage", map[string]any{"action": "backend-list"}); st != http.StatusOK {
		t.Fatalf("backend-list: HTTP %d %v", st, list)
	} else if rows, _ := list["backends"].([]any); len(rows) != 0 {
		t.Fatalf("backend-list before the seed: %d rows, want none (fresh install): %v", len(rows), rows)
	}
	for _, p := range seedPayloads(srv.URL) {
		st, resp := child.adminCall(t, http.MethodPost, "/api/manage", map[string]any{"action": "backend-create", "data": p})
		if st != http.StatusOK || resp["success"] != true {
			t.Fatalf("backend-create %s: HTTP %d %v", p["name"], st, resp)
		}
		b, _ := resp["backend"].(map[string]any)
		t.Logf("backend-create %s: HTTP %d, locality %v, trust %v, warnings %v", p["name"], st, b["locality"], b["trust"], resp["warnings"])
	}

	// Two related blocks in scope private — inside the default read window
	// (scheduler.read_scopes = private,shared,work), so no CTX_READ_SCOPES.
	var blockIDs []string
	for _, blk := range []struct{ title, content string }{
		{"Zeppelin hangar heating decision",
			"The Zeppelin hangar is heated by a heat pump from October on; the gas boiler stays as a reserve for frost below minus ten degrees."},
		{"Zeppelin hangar heat pump installation",
			"The heat pump for the Zeppelin hangar was installed in September and replaces the gas boiler as the primary heating of the hangar."},
	} {
		st, resp := child.adminCall(t, http.MethodPost, "/api/store", map[string]any{
			"category": "decisions", "title": blk.title, "content": blk.content, "scope": "private",
		})
		b, _ := resp["block"].(map[string]any)
		id, _ := b["id"].(string)
		if st != http.StatusOK || resp["success"] != true || id == "" {
			t.Fatalf("store %q: HTTP %d %v", blk.title, st, resp)
		}
		t.Logf("stored %s %q (type %v, lifecycle %v, sensitivity %v)", id, blk.title, b["type"], b["lifecycle_state"], b["sensitivity"])
		blockIDs = append(blockIDs, id)
	}

	var firstLink time.Duration
	for deadline := time.Now().Add(phaseBLinkDeadline); ; {
		select {
		case err := <-child.done:
			child.done <- err
			t.Fatalf("phase B: the child exited (%v); output tail:\n%s", err, tailLines(child.out.String(), 80))
		default:
		}
		if countLinks() > 0 {
			firstLink = time.Since(seedAt)
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("phase B: no context_dream_links row within %s of the seed; stub: %s", phaseBLinkDeadline, stub.report())
			t.Logf("phase B output:\n%s", tailLines(child.out.String()[phaseBFrom:], 120))
			return
		}
		time.Sleep(2 * time.Second)
	}

	status, services := child.health(t)
	rows, err := links.Query(ctx, `SELECT row_to_json(l)::text FROM context_dream_links l ORDER BY created_at`)
	if err != nil {
		t.Fatalf("read context_dream_links: %v", err)
	}
	var linkRows []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan link: %v", err)
		}
		linkRows = append(linkRows, s)
	}
	rows.Close()

	t.Logf("phase B: first link %s after the seed; %d link(s): %v", firstLink.Round(100*time.Millisecond), len(linkRows), linkRows)
	t.Logf("phase B: stub %s", stub.report())
	t.Logf("phase B: /health %q services %v", status, services)

	if child.cmd.Process.Pid != pid {
		t.Errorf("phase B: child pid %d, want %d — the child was restarted", child.cmd.Process.Pid, pid)
	}
	if n := strings.Count(child.out.String(), dreamWorkersStarted); n != 1 {
		t.Errorf("phase B: %q logged %d times, want once — one boot", dreamWorkersStarted, n)
	}
	if stub.eval.Load() == 0 {
		t.Errorf("phase B: a link without a link-evaluation call at the stub: %s", stub.report())
	}
	var between int
	if err := links.QueryRow(ctx, `SELECT count(*) FROM context_dream_links
		WHERE source_block_id = ANY($1::uuid[]) AND target_block_id = ANY($1::uuid[])`, blockIDs).Scan(&between); err != nil {
		t.Fatalf("count links between the stored blocks: %v", err)
	}
	if between != len(linkRows) {
		t.Errorf("phase B: %d of %d link(s) join the two stored blocks %v", between, len(linkRows), blockIDs)
	}

	// ERROR lines from the seed up to the SIGTERM, and those of the shutdown.
	preStop := len(child.out.String())
	out := child.stop(t)
	for _, w := range []struct {
		name string
		text string
	}{{"before the stop", out[phaseBFrom:preStop]}, {"during the shutdown", out[preStop:]}} {
		var errs []string
		for _, line := range strings.Split(w.text, "\n") {
			if strings.Contains(line, `"level":"ERROR"`) {
				errs = append(errs, line)
			}
		}
		t.Logf("phase B: %d ERROR line(s) %s", len(errs), w.name)
		for _, l := range errs {
			t.Logf("phase B ERROR (%s): %s", w.name, l)
			if w.name == "before the stop" && strings.Contains(strings.ToLower(l), "dream") {
				t.Errorf("phase B: ERROR line naming dream: %s", l)
			}
		}
	}
}
