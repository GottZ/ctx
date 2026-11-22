//go:build integration

// Issue #492, end to end: what a new installation's Dream does before and
// after the operator seeds the backend pool — in ONE running ctxd, no
// restart in between. The child is the production main() booted against an
// empty database through the harness of dream_install_default_integration_test.go
// (startDreamBootChild), with CTX_DREAM_ENABLED unset: Dream runs on the
// installation's default. CTX_GRAPH_EXPAND_ENABLED=true is what the shipped
// docker-compose.yml sets for every installation that does not override it.
//
//   - Phase A (no backend of role dream/chat): one real minute at the default
//     dream.idle_wait (20 s). The loop skips before the pick, no ERROR line
//     names Dream, /health says what an operator sees — and the operator gets
//     one WARN that says what is missing and how to fix it, not three WARN
//     and six INFO lines a minute and worker (issue #495; the idle cadence of
//     the skip, now logged at DEBUG, is pinned by the events test
//     TestDreamLoopReportsAMissingDreamBackendOncePerChange).
//
//   - Phase B (same child): an OpenAI-wire stub serves embeddings and the
//     dream chat stages; the pool is seeded with exactly the rows
//     `ctx backends seed` writes (POST /api/manage backend-create), two pairs
//     of related blocks are stored over POST /api/store/batch, and the dream
//     loop must link each pair without the child being restarted; one INFO
//     line says that a backend of role dream serves again.
//
//   - Phase C (issue #495, same child): does a query bring in a linked block
//     through the graph expansion? The stub gives every text a vector of its
//     own (rows of a Hadamard matrix), 80 unrelated blocks closer to the
//     queries than the linked blocks push those out of the semantic arm (its
//     cap is 75, migration 147), and their texts share no lexeme with the
//     queries — the measurement's arm_ranks block shows that no arm of the
//     fusion found them. A query on the first block of each pair must bring
//     the second one exactly while graph.enabled is on.
//
// Run it alone (about 2½ minutes: one boot, one minute of phase A, B and C):
//
//	go test -tags=integration -p 1 -count=1 -v -run 'TestDreamFreshInstallE2E' ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/bits"
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

// The lines of a dream loop without a backend of role dream (issue #495).
const (
	// dreamSkipNoBackendLog is the per-cycle skip line (DEBUG since #495, an
	// INFO line before).
	dreamSkipNoBackendLog = `"msg":"dream: no eligible dream backend`
	// dreamEmptyChainLog is backends.Pool.Chain's WARN for an empty chain; the
	// skip path asked Chain until #495 and logged it once per cycle.
	dreamEmptyChainLog = `"msg":"backends: empty chain","role":"dream"`
	// dreamIdleLog is the scheduler's idle line; a skipped cycle logged it
	// until #495.
	dreamIdleLog = `"msg":"scheduler: dream idle, waiting"`
	// dreamMissingNotice is dream.BackendNotice's WARN when the loop first
	// finds no backend of role dream; dreamServesNotice its INFO when one
	// serves again.
	dreamMissingNotice = `"level":"WARN","msg":"dream: no backend of role dream`
	dreamServesNotice  = `"level":"INFO","msg":"dream: a backend of role dream serves again`
)

const (
	// phaseAWindow is the observation window of phase A: one real minute.
	phaseAWindow = 60 * time.Second
	// phaseBLinkDeadline bounds the wait from the seed to the links.
	phaseBLinkDeadline = 4 * time.Minute
	// stubEmbedDims is the dimension of context_blocks.embedding.
	stubEmbedDims = 1024
	// phaseCDistractors is the number of unrelated blocks phase C stores: more
	// than the semantic arm's cap of 75 (ctx_rrf_arms p_cap_semantic,
	// migration 147), all of them closer to the queries than the linked
	// blocks, so no linked block is a semantic candidate of a query.
	phaseCDistractors = 80
)

// Hadamard rows the stub builds its vectors from. Row r of the 1024×1024
// Sylvester matrix has the component sign (-1)^popcount(r&i); two different
// rows are orthogonal, every row but 0 has mean 0, so a unit combination of
// rows has norm 1 and variance 1/1024 — inside the embed quality gate
// (internal/embed qualityGate: norm in [0.99, 1.01], variance ≥ 0.0001).
const (
	rowQueryHangar     = 1
	rowQueryLighthouse = 2
	rowDefault         = 3 // every text no rule names: dream keywords, probes
	rowHangarOwn       = 10
	rowLighthouseOwn   = 11
	rowDistractorBase  = 100 // distractor j uses row rowDistractorBase+j
	rowLinkedHangar    = 900
	rowLinkedLight     = 901
)

// stubVector is the unit vector Σ w·row / ‖w‖.
func stubVector(weights map[int]float64) []float64 {
	var norm float64
	for _, w := range weights {
		norm += w * w
	}
	norm = math.Sqrt(norm)
	vec := make([]float64, stubEmbedDims)
	for i := range vec {
		var x float64
		for r, w := range weights {
			if bits.OnesCount(uint(r&i))%2 == 0 {
				x += w
			} else {
				x -= w
			}
		}
		vec[i] = x / norm / math.Sqrt(stubEmbedDims)
	}
	return vec
}

// stubVecRule gives every embedding input that contains marker the vector vec.
type stubVecRule struct {
	marker string
	vec    []float64
}

// stubPair is one pair of related blocks by title: the link-evaluation call of
// either one answers a link to the other, of the given type.
type stubPair struct {
	partner string
	typ     string
}

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
	skips, emptyChains, idles int            // the per-cycle lines of the skip path in the window
	warns, infos              int            // every WARN and INFO line in the window
	byMsg                     map[string]int // WARN and INFO lines in the window by level and message
	errorLines                []string       // every "level":"ERROR" line in the window
	healthStatus              string
	services                  map[string]string
}

// observeWindow lets the child run for d and reads what it logged meanwhile
// and what /health says at the end.
func observeWindow(t *testing.T, c *dreamBootChild, d time.Duration) phaseAResult {
	t.Helper()
	from := len(c.out.String())
	time.Sleep(d)
	status, services := c.health(t)
	window := c.out.String()[from:]
	r := phaseAResult{byMsg: map[string]int{}}
	for _, line := range strings.Split(window, "\n") {
		switch {
		case strings.Contains(line, dreamSkipNoBackendLog):
			r.skips++
		case strings.Contains(line, dreamEmptyChainLog):
			r.emptyChains++
		case strings.Contains(line, dreamIdleLog):
			r.idles++
		}
		var rec struct{ Level, Msg string }
		_ = json.Unmarshal([]byte(line), &rec)
		switch {
		case strings.Contains(line, `"level":"WARN"`):
			r.warns++
			r.byMsg[rec.Level+" "+rec.Msg]++
		case strings.Contains(line, `"level":"INFO"`):
			r.infos++
			r.byMsg[rec.Level+" "+rec.Msg]++
		case strings.Contains(line, `"level":"ERROR"`):
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
	vecRules                                        []stubVecRule       // first match wins
	pairs                                           map[string]stubPair // by title
	otherPrompts                                    map[string]int      // first line of an unclassified system prompt
	otherPaths                                      map[string]int      // method + path of an unserved request
}

// stubCandidate reads a candidate's id and title out of the link-evaluation
// prompt (dream.buildEvalPrompt: <block id="…" title="…" …>); stubSourceTitle
// reads the source block's title line.
var (
	stubCandidate   = regexp.MustCompile(`<block id="([0-9a-fA-F-]{36})" title="([^"]*)"`)
	stubSourceTitle = regexp.MustCompile(`(?m)^Title: (.*)$`)
)

func newDreamStub() *dreamStub {
	return &dreamStub{pairs: map[string]stubPair{}, otherPrompts: map[string]int{}, otherPaths: map[string]int{}}
}

// setVectors installs the embedding rules; setPair links two titles.
func (s *dreamStub) setVectors(rules []stubVecRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vecRules = rules
}

func (s *dreamStub) setPair(a, b, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairs[a] = stubPair{partner: b, typ: typ}
	s.pairs[b] = stubPair{partner: a, typ: typ}
}

// vectorFor is the vector of one embedding input.
func (s *dreamStub) vectorFor(text string) []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.vecRules {
		if strings.Contains(text, r.marker) {
			return r.vec
		}
	}
	return stubVector(map[int]float64{rowDefault: 1})
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
	var inputs []string
	switch in := req.Input.(type) {
	case string:
		inputs = []string{in}
	case []any:
		for _, x := range in {
			str, _ := x.(string)
			inputs = append(inputs, str)
		}
	}
	s.embedCalls.Add(1)
	s.embedInputs.Add(int64(len(inputs)))
	type entry struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	data := make([]entry, len(inputs))
	for i, in := range inputs {
		data[i] = entry{Object: "embedding", Index: i, Embedding: s.vectorFor(in)}
	}
	n := len(inputs)
	writeStubJSON(w, map[string]any{
		"object": "list",
		"model":  req.Model,
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": 4 * n, "total_tokens": 4 * n},
	})
}

// evalAnswer links the source block to its pair partner when the partner is
// among the candidates, and to nothing else.
func (s *dreamStub) evalAnswer(user string) string {
	type link struct {
		TargetID   string  `json:"target_id"`
		Type       string  `json:"type"`
		Confidence float64 `json:"confidence"`
	}
	links := []link{}
	if m := stubSourceTitle.FindStringSubmatch(user); m != nil {
		s.mu.Lock()
		pair, ok := s.pairs[strings.TrimSpace(m[1])]
		s.mu.Unlock()
		if ok {
			for _, c := range stubCandidate.FindAllStringSubmatch(user, -1) {
				if c[2] == pair.partner {
					links = append(links, link{TargetID: c[1], Type: pair.typ, Confidence: 0.9})
				}
			}
		}
	}
	s.evalLinks.Add(int64(len(links)))
	raw, _ := json.Marshal(links)
	return string(raw)
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
		content = s.evalAnswer(user)
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

// graphCase is one pair of phase C: the block a query finds (seed) and the one
// Dream linked to it (linked), with the link type the stub answers.
type graphCase struct {
	name                         string
	query                        string
	linkType                     string
	seedTitle, seedContent       string
	linkedTitle, linkedContent   string
	linkedCategory               string
	queryRow, seedRow, linkedRow int
}

// graphCases share no lexeme between a query and its linked block (nor with
// the distractors), and the linked block's title shares at most two trigrams
// with the query (pg_trgm similarity < 0.05, the trigram arm's threshold) —
// the linked block can come only through the graph. The two link types are
// the lowest (topical, weight 0.5) and a high one (factual, weight 0.9) of
// the graph.weight_* defaults.
var graphCases = []graphCase{
	{
		name:          "topical",
		query:         "Zeppelin hangar gas boiler frost reserve?",
		linkType:      "topical",
		seedTitle:     "Zeppelin hangar heating decision",
		seedContent:   "The Zeppelin hangar is heated by a heat pump from October on; the gas boiler stays as a frost reserve below minus ten degrees.",
		linkedTitle:   "Electrician bookings",
		linkedContent: "Three electricians and one welder are booked for the heat pump retrofit; work starts on Monday.",
		// Same category as the seed: a topical tie (dream keeps factual only
		// across categories, coerceCategoryFactual).
		linkedCategory: "decisions",
		queryRow:       rowQueryHangar, seedRow: rowHangarOwn, linkedRow: rowLinkedHangar,
	},
	{
		name:          "factual",
		query:         "Lighthouse halogen lamp spare storeroom?",
		linkType:      "factual",
		seedTitle:     "Lighthouse lamp replacement decision",
		seedContent:   "The lighthouse lamp was replaced by an LED array in March; the old halogen lamp stays in the storeroom as a spare.",
		linkedTitle:   "Nordlicht invoice terms",
		linkedContent: "Nordlicht GmbH delivers the LED array with a five year warranty; invoices go to the harbour office.",
		// Another category than the seed's: dream coerces a factual link
		// between two blocks of one category to topical (V10,
		// coerceCategoryFactual), so the factual pair spans two.
		linkedCategory: "projects",
		queryRow:       rowQueryLighthouse, seedRow: rowLighthouseOwn, linkedRow: rowLinkedLight,
	},
}

// distractor j of phase C.
func distractorTitle(j int) string { return fmt.Sprintf("Depot inventory sheet %02d", j) }
func distractorContent(j int) string {
	return fmt.Sprintf("Pallet count %02d for the north depot lists crates, shelving units and forklift batteries.", j)
}

// stubVectorRules: each query embeds to its own row; each seed block close to
// its query (cosine 0.95); each linked block to a row of its own (cosine 0
// with every query); every distractor to cosine 0.5 with both queries, so the
// 80 of them rank before every linked block in the semantic arm.
func stubVectorRules() []stubVecRule {
	var rules []stubVecRule
	for _, c := range graphCases {
		rules = append(rules,
			// Without the question mark: the marker must survive whatever the
			// query path does to punctuation before it embeds.
			stubVecRule{strings.TrimSuffix(c.query, "?"), stubVector(map[int]float64{c.queryRow: 1})},
			stubVecRule{c.seedContent, stubVector(map[int]float64{c.queryRow: 0.95, c.seedRow: 0.3122})},
			stubVecRule{c.linkedContent, stubVector(map[int]float64{c.linkedRow: 1})},
		)
	}
	for j := 1; j <= phaseCDistractors; j++ {
		rules = append(rules, stubVecRule{distractorContent(j), stubVector(map[int]float64{
			rowQueryHangar: 0.5, rowQueryLighthouse: 0.5, rowDistractorBase + j: 0.7071,
		})})
	}
	return rules
}

// storeBatch writes items over POST /api/store/batch (all or none) and
// returns the block id of each item, by title.
func (c *dreamBootChild) storeBatch(t *testing.T, items []map[string]any) map[string]string {
	t.Helper()
	st, resp := c.adminCall(t, http.MethodPost, "/api/store/batch", map[string]any{"items": items})
	answers, _ := resp["items"].([]any)
	if st != http.StatusOK || resp["success"] != true || len(answers) != len(items) {
		t.Fatalf("store batch of %d: HTTP %d %v", len(items), st, resp)
	}
	ids := make(map[string]string, len(items))
	for i, a := range answers {
		m, _ := a.(map[string]any)
		b, _ := m["block"].(map[string]any)
		id, _ := b["id"].(string)
		if id == "" {
			t.Fatalf("store batch item %d (%v): no block in the answer %v", i, items[i]["title"], m)
		}
		ids[items[i]["title"].(string)] = id
	}
	return ids
}

// graphQuery is one retrieval-only query with the arm_ranks measurement: the
// source ids in answer order and the ids any arm of the fusion found.
func (c *dreamBootChild) graphQuery(t *testing.T, query string, limit int) (sources []string, scores []float64, armIDs map[string]bool) {
	t.Helper()
	st, resp := c.adminCall(t, http.MethodPost, "/api/query", map[string]any{
		"query": query, "limit": limit, "synthesize": false, "arm_ranks": true,
	})
	if st != http.StatusOK || resp["success"] != true {
		t.Fatalf("query %q: HTTP %d %v", query, st, resp)
	}
	list, _ := resp["sources"].([]any)
	for _, s := range list {
		m, _ := s.(map[string]any)
		id, _ := m["id"].(string)
		score, _ := m["score"].(float64)
		sources = append(sources, id)
		scores = append(scores, score)
	}
	armIDs = map[string]bool{}
	ranks, _ := resp["arm_ranks"].(map[string]any)
	rows, _ := ranks["rows"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		armIDs[id] = true
	}
	return sources, scores, armIDs
}

// setGraphEnabled writes graph.enabled over the settings API and waits until
// the served snapshot carries it (the write reaches the store through the
// settings NOTIFY).
func (c *dreamBootChild) setGraphEnabled(t *testing.T, on bool) {
	t.Helper()
	if st, resp := c.adminCall(t, http.MethodPut, "/api/settings/graph.enabled", map[string]any{"value": on}); st != http.StatusOK || resp["success"] != true {
		t.Fatalf("PUT graph.enabled=%v: HTTP %d %v", on, st, resp)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if v, _ := c.settingSource(t, "graph.enabled"); v == on {
			return
		}
	}
	t.Fatalf("graph.enabled did not become %v within 30 s", on)
}

func indexOf(list []string, id string) int {
	for i, x := range list {
		if x == id {
			return i
		}
	}
	return -1
}

func TestDreamFreshInstallE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	dsn := freshInstallDSN(t)

	// CTX_DREAM_ENABLED stays unset: Dream runs on the installation's default.
	// The graph expansion is on as the shipped docker-compose.yml sets it.
	child := startDreamBootChild(t, dsn, map[string]string{"CTX_GRAPH_EXPAND_ENABLED": "true"})
	pid := child.cmd.Process.Pid

	if value, source := child.settingSource(t, "dream.enabled"); value != true || source != "install" {
		t.Fatalf("dream.enabled = %v (source %q), want true from the installation's default (source \"install\")", value, source)
	}

	// ---- Phase A: Dream on, no backend of role dream/chat.
	a := observeWindow(t, child, phaseAWindow)
	notices := strings.Count(child.out.String(), dreamMissingNotice) // since the boot
	t.Logf("phase A, lines in %s: %d WARN (%d × empty chain role=dream), %d INFO (%d × skip, %d × idle); since the boot %d × the missing-backend notice; /health %q services %v; %d ERROR line(s)",
		phaseAWindow, a.warns, a.emptyChains, a.infos, a.skips, a.idles, notices, a.healthStatus, a.services, len(a.errorLines))
	for msg, n := range a.byMsg {
		t.Logf("phase A line ×%d: %s", n, msg)
	}
	for _, l := range a.errorLines {
		t.Logf("phase A ERROR: %s", l)
	}
	if notices != 1 {
		t.Errorf("phase A: the missing-backend notice logged %d times since the boot, want once — it says what is missing and how to fix it", notices)
	}
	if a.emptyChains+a.skips+a.idles != 0 {
		t.Errorf("phase A: %d empty-chain WARN, %d skip INFO, %d idle INFO in %s — a skipped cycle logs nothing above DEBUG",
			a.emptyChains, a.skips, a.idles, phaseAWindow)
	}
	for _, l := range a.errorLines {
		if strings.Contains(strings.ToLower(l), "dream") {
			t.Errorf("phase A: ERROR line naming dream: %s", l)
		}
	}

	// ---- Phase B: seed without a restart, then links.
	stub := newDreamStub()
	stub.setVectors(stubVectorRules())
	for _, c := range graphCases {
		stub.setPair(c.seedTitle, c.linkedTitle, c.linkType)
	}
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

	// Two pairs of related blocks in scope private — inside the default read
	// window (scheduler.read_scopes = private,shared,work), so no
	// CTX_READ_SCOPES — in one batch: the dream loop sees all four or none,
	// so no block is picked before its partner exists.
	var items []map[string]any
	for _, c := range graphCases {
		items = append(items,
			map[string]any{"category": "decisions", "title": c.seedTitle, "content": c.seedContent, "scope": "private"},
			map[string]any{"category": c.linkedCategory, "title": c.linkedTitle, "content": c.linkedContent, "scope": "private"},
		)
	}
	ids := child.storeBatch(t, items)
	label := map[string]string{}
	for _, c := range graphCases {
		label[ids[c.seedTitle]] = c.name + "/seed"
		label[ids[c.linkedTitle]] = c.name + "/linked"
		t.Logf("stored %s %q and %s %q", ids[c.seedTitle], c.seedTitle, ids[c.linkedTitle], c.linkedTitle)
	}

	// The forward link of each pair (seed → linked) is the edge the directed
	// expansion (graph.directed default true) follows from the seed.
	forwardLinks := func() int {
		var n int
		for _, c := range graphCases {
			var ok bool
			if err := links.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM context_dream_links
				WHERE source_block_id = $1 AND target_block_id = $2)`, ids[c.seedTitle], ids[c.linkedTitle]).Scan(&ok); err != nil {
				t.Fatalf("read forward link: %v", err)
			}
			if ok {
				n++
			}
		}
		return n
	}
	var firstLink, allForward time.Duration
	for deadline := time.Now().Add(phaseBLinkDeadline); ; {
		select {
		case err := <-child.done:
			child.done <- err
			t.Fatalf("phase B: the child exited (%v); output tail:\n%s", err, tailLines(child.out.String(), 80))
		default:
		}
		if firstLink == 0 && countLinks() > 0 {
			firstLink = time.Since(seedAt)
		}
		if forwardLinks() == len(graphCases) {
			allForward = time.Since(seedAt)
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("phase B: %d of %d forward links within %s of the seed; stub: %s", forwardLinks(), len(graphCases), phaseBLinkDeadline, stub.report())
			t.Logf("phase B output:\n%s", tailLines(child.out.String()[phaseBFrom:], 120))
			return
		}
		time.Sleep(2 * time.Second)
	}

	status, services := child.health(t)
	// One statement reads the rows and whether each joins the blocks of one
	// pair: Dream keeps running, and a count read separately could see a link
	// the row read did not.
	pairOf := map[string]string{}
	for _, c := range graphCases {
		pairOf[ids[c.seedTitle]], pairOf[ids[c.linkedTitle]] = c.name, c.name
	}
	rows, err := links.Query(ctx, `SELECT l.source_block_id::text, l.target_block_id::text, l.relationship, l.raw_confidence
		  FROM context_dream_links l ORDER BY created_at`)
	if err != nil {
		t.Fatalf("read context_dream_links: %v", err)
	}
	var linkRows []string
	var between int
	for rows.Next() {
		var src, dst, rel string
		var conf float64
		if err := rows.Scan(&src, &dst, &rel, &conf); err != nil {
			t.Fatalf("scan link: %v", err)
		}
		linkRows = append(linkRows, fmt.Sprintf("%s → %s %s %.2f", label[src], label[dst], rel, conf))
		if p := pairOf[src]; p != "" && p == pairOf[dst] {
			between++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read context_dream_links: %v", err)
	}

	t.Logf("phase B: first link %s, both forward links %s after the seed; %d link(s): %v",
		firstLink.Round(100*time.Millisecond), allForward.Round(100*time.Millisecond), len(linkRows), linkRows)
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
	if between != len(linkRows) {
		t.Errorf("phase B: %d of %d link(s) join the blocks of one pair", between, len(linkRows))
	}
	if n := strings.Count(child.out.String()[phaseBFrom:], dreamServesNotice); n != 1 {
		t.Errorf("phase B: %q logged %d times after the seed, want once", dreamServesNotice, n)
	}

	// ---- Phase C: a query brings the linked block in through the graph.
	var distractors []map[string]any
	for j := 1; j <= phaseCDistractors; j++ {
		distractors = append(distractors, map[string]any{
			"category": "reference", "title": distractorTitle(j), "content": distractorContent(j), "scope": "private",
		})
	}
	for title, id := range child.storeBatch(t, distractors) {
		label[id] = "distractor/" + strings.TrimPrefix(title, "Depot inventory sheet ")
	}
	// Every block carries its vector before the queries run: a block without
	// one is no semantic candidate, and the measurement needs the 80 in the arm.
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(500 * time.Millisecond) {
		var missing int
		if err := links.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE embedding IS NULL AND NOT is_archived`).Scan(&missing); err != nil {
			t.Fatalf("count unembedded blocks: %v", err)
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase C: %d block(s) still without a vector after a minute", missing)
		}
	}

	type measured struct {
		on5, on20, off5, off20 []string
		onScores5              []float64
		arms                   map[string]bool
	}
	render := func(list []string, scores []float64) string {
		parts := make([]string, len(list))
		for i, id := range list {
			parts[i] = label[id]
			if scores != nil {
				parts[i] += fmt.Sprintf(" %.5f", scores[i])
			}
		}
		return strings.Join(parts, ", ")
	}
	got := map[string]*measured{}
	for _, c := range graphCases {
		m := &measured{}
		m.on5, m.onScores5, m.arms = child.graphQuery(t, c.query, 5)
		var scores20 []float64
		m.on20, scores20, _ = child.graphQuery(t, c.query, 20)
		got[c.name] = m
		t.Logf("phase C %s graph on, limit 5: [%s]", c.name, render(m.on5, m.onScores5))
		t.Logf("phase C %s graph on, limit 20: [%s]", c.name, render(m.on20, scores20))
	}
	child.setGraphEnabled(t, false)
	for _, c := range graphCases {
		m := got[c.name]
		m.off5, _, _ = child.graphQuery(t, c.query, 5)
		m.off20, _, _ = child.graphQuery(t, c.query, 20)
		t.Logf("phase C %s graph off, limit 5: [%s]", c.name, render(m.off5, nil))
	}

	phaseCLog := child.out.String()
	t.Logf("phase C: child logged %d × \"graph expansion active without reranker noise-filter\", %d × \"graph expand failed\"",
		strings.Count(phaseCLog, "graph expansion active without reranker noise-filter"), strings.Count(phaseCLog, "graph expand failed"))
	for _, c := range graphCases {
		m, seed, linked := got[c.name], ids[c.seedTitle], ids[c.linkedTitle]
		// The precondition of the measurement: the query finds the seed first,
		// and no arm of the fusion finds the linked block.
		if indexOf(m.on5, seed) != 0 || indexOf(m.off5, seed) != 0 {
			t.Errorf("phase C %s: the seed is not the first source (graph on: %d, off: %d)", c.name, indexOf(m.on5, seed), indexOf(m.off5, seed))
		}
		if m.arms[linked] {
			t.Errorf("phase C %s: an arm of the fusion found the linked block — it is a direct hit, the measurement proves nothing", c.name)
		}
		if indexOf(m.off20, linked) >= 0 {
			t.Errorf("phase C %s: the linked block is a source with graph.enabled off (position %d) — it does not depend on the link", c.name, indexOf(m.off20, linked))
		}
		t.Logf("phase C %s (%s link): linked block at position %d of the limit-5 answer, %d of the limit-20 answer with graph.enabled on (-1 = absent)",
			c.name, c.linkType, indexOf(m.on5, linked), indexOf(m.on20, linked))
	}
	// The factual link (weight 0.9) places its neighbour at 0.54 × the top
	// score, above every block only the semantic arm found: it is the second
	// source of the default-sized answer. The topical one is logged above.
	if f := got["factual"]; indexOf(f.on5, ids[graphCases[1].linkedTitle]) < 0 {
		t.Errorf("phase C factual: the linked block is not among the 5 sources with graph.enabled on — the graph expansion did not bring it")
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
			// Issue #495: the listener's context ending with the SIGTERM is the
			// stop, not a fault — deploy.sh counts ERROR lines after a deploy.
			if w.name == "during the shutdown" && strings.Contains(l, `"msg":"listener: pgxlisten error"`) {
				t.Errorf("phase B: the listener's stop logged at ERROR: %s", l)
			}
		}
	}
}
