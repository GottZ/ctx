//go:build integration

// Issue #196, the half the short probes in mcp_query_filters_test.go cannot
// reach: that `category`, `tags` and `categories_exclude` on the MCP `query`
// tool narrow what the real /api/query pipeline returns.
//
// The blocks are written through POST /api/store (nothing is inserted by hand,
// so category and tags are whatever the write path stored), and every probe
// calls the MCP tool handler with a raw JSON argument object. The tool
// delegates to the real query handler; one test seam sits in between and adds
// `synthesize:false` to the delegated body, because the tool itself always
// asks for synthesis and this fixture has no synthesis backend. The filters
// are applied in the retrieval step, ahead of that switch.
//
// RED before the fields existed: the decoder drops the three keys and every
// filtered probe answers all three blocks. The unfiltered probe passes before
// and after; it pins that the corpus is retrievable at all, so a filtered
// probe cannot pass on an empty answer.
//
//	go test -tags=integration ./internal/handler/ -run TestMCPQueryFilters -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	mqfOpenAlpha  = "mqf ledger entry one"
	mqfOpenBeta   = "mqf ledger entry two"
	mqfClosedBeta = "mqf ledger entry three"
)

// mqfRetrievalOnly forwards the tool's delegated request to the real query
// handler with `synthesize:false` added. Every other key of the body passes
// through untouched, so what the pipeline filters by is what the tool sent.
type mqfRetrievalOnly struct {
	t    *testing.T
	next http.HandlerFunc
}

// ServeHTTP rewrites the delegated body with `synthesize:false` and hands the
// request to the real query handler; a body it cannot read or decode fails
// the test.
func (d mqfRetrievalOnly) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		d.t.Errorf("read delegated body: %v", err)
		return
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		d.t.Errorf("decode delegated body %s: %v", raw, err)
		return
	}
	body["synthesize"] = json.RawMessage("false")
	out, _ := json.Marshal(body)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(out))
	r2.ContentLength = int64(len(out))
	d.next(w, r2)
}

// mqfFixture is one database with three blocks written through the store
// handler, and the MCP tool config whose query delegate is the real handler.
type mqfFixture struct {
	cfg MCPConfig
	ctx context.Context
}

// mqfSetup boots a database, stores the three fixture blocks through the
// store handler under one private key, and returns the tool config wired to
// the real query handler together with that key's context.
func mqfSetup(t *testing.T) *mqfFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	_, plain, err := store.CreateApiKey(ctx, pool, "mqf", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	keyCtx := context.WithValue(ctx, authResultKey, ar)

	// Default sensitivity public: the production default (credentials) would
	// bring the trust gate of the embed backend into probes about filters.
	storeH := NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{
		Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
	}}, reg)
	for _, b := range []struct {
		title, category string
		tags            []string
	}{
		{mqfOpenAlpha, "mqf-open", []string{"alpha"}},
		{mqfOpenBeta, "mqf-open", []string{"beta"}},
		{mqfClosedBeta, "mqf-closed", []string{"beta"}},
	} {
		raw, _ := json.Marshal(map[string]any{
			"category": b.category, "title": b.title, "tags": b.tags,
			"content": "body of " + b.title + ", stored through the write path",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		storeH.HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("store %q: status %d, body %s", b.title, rec.Code, rec.Body.String())
		}
	}

	// bw2Config leaves embed_backfill.sync_cap at 0, so the first query embeds
	// all three blocks inline. The backend answers one fixed vector for blocks
	// and question alike: every block is a semantic hit, and the filters are
	// the only thing that can tell them apart.
	backend := bw2NewBackend(t)
	h := NewQueryHandler(pool, config.NewStore(bw2Config()), bw2Pool(backend.srv.URL), nil, reg, snapshotTestAdmitter(t))
	return &mqfFixture{
		cfg: MCPConfig{Pool: pool, Blocktypes: reg, QueryHandler: mqfRetrievalOnly{t: t, next: h.HandleQuery}},
		ctx: keyCtx,
	}
}

// titles runs the MCP query tool over a raw argument object and returns which
// of the three fixture titles its source list names, sorted.
func (f *mqfFixture) titles(t *testing.T, raw string) []string {
	t.Helper()
	res, _, err := mcpQueryHandler(f.cfg)(f.ctx, nil, typeFilterArgs[queryInput](t, raw))
	if err != nil {
		t.Fatalf("query %s: transport error %v", raw, err)
	}
	text := mcpTextOf(res)
	if res.IsError {
		t.Fatalf("query %s: IsError=true (%s)", raw, text)
	}
	var got []string
	for _, title := range []string{mqfOpenAlpha, mqfOpenBeta, mqfClosedBeta} {
		if strings.Contains(text, title) {
			got = append(got, title)
		}
	}
	slices.Sort(got)
	return got
}

// TestMCPQueryFiltersNarrowThePipeline_Integration: each filter, given to the
// MCP tool, decides which of the three stored blocks the real pipeline names.
// The unfiltered probe runs first and must name all three.
func TestMCPQueryFiltersNarrowThePipeline_Integration(t *testing.T) {
	f := mqfSetup(t)

	// Premise first: unfiltered, all three blocks come back. Otherwise a
	// filtered probe below could pass on an empty answer.
	all := []string{mqfOpenAlpha, mqfClosedBeta, mqfOpenBeta}
	slices.Sort(all)
	if got := f.titles(t, `{"question":"`+bw2GoldenQuery+`"}`); !slices.Equal(got, all) {
		t.Fatalf("unfiltered query names %v, want all of %v: fixture not retrievable, probes inconclusive", got, all)
	}

	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"category", `"category":"mqf-open"`, []string{mqfOpenAlpha, mqfOpenBeta}},
		{"tags", `"tags":["beta"]`, []string{mqfClosedBeta, mqfOpenBeta}},
		{"tags_overlap", `"tags":["alpha","no-such-tag"]`, []string{mqfOpenAlpha}},
		{"categories_exclude", `"categories_exclude":["mqf-closed"]`, []string{mqfOpenAlpha, mqfOpenBeta}},
		{"category_and_tags", `"category":"mqf-open","tags":["beta"]`, []string{mqfOpenBeta}},
		{"category_matches_nothing", `"category":"mqf-no-such-category"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := append([]string(nil), tc.want...)
			slices.Sort(want)
			got := f.titles(t, `{"question":"`+bw2GoldenQuery+`",`+tc.args+`}`)
			if !slices.Equal(got, want) {
				t.Errorf("query with %s names %v, want %v", tc.args, got, want)
			}
		})
	}
}
