//go:build integration

// Issue #225: the request filters of POST /api/query — category, tags,
// categories_exclude and types_exclude — narrowed the rank fusion only. The
// three stages after it that can ADD a block (cluster injection, graph
// expansion, aggregate fold) did not take them, so a block the request
// excluded came back as a neighbour, a cluster sibling or a folded parent.
//
// One test per stage, each through the real handler. The blocks are written
// through POST /api/store and the links through dream.WriteLinks (the Dream
// cycle's own write path); only the cluster map is seeded by SQL, as every
// cluster-stage test does — its writer is the Louvain rebuild.
//
// Every test carries a positive control: a block that passes the filter and is
// NOT a fusion hit (stored after the embedding pass, with the query-path
// backfill off, so it has no vector and the golden query shares no lexeme with
// it). It must still come in through the stage, so a fix that switches the
// stage off whenever a filter is present cannot pass.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryPostStageFilters -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Fixture titles. pfSeed is the one fusion hit every stage starts from; the
// others are named by what the filters make of them.
const (
	pfSeed      = "pf ledger seed one"         // pf-open, alpha — a fusion hit
	pfOpenBeta  = "pf ledger open two"         // pf-open, beta — a fusion hit
	pfClosed    = "pf ledger closed three"     // pf-closed, beta — excluded by category/tags/categories_exclude
	pfReference = "pf ledger reference four"   // pf-open, alpha, type reference — excluded by types_exclude
	pfLate      = "pf ledger late open five"   // pf-open, alpha — no vector: reachable only through a stage
	pfIssue     = "pf ledger closed issue six" // pf-closed — the parent of pfComment
	pfComment   = "pf ledger open comment"     // pf-open, aggregating type, parent pfIssue

	pfCommentType = "pf-comment"
)

type pfFixture struct {
	pool   *pgxpool.Pool
	reg    *blocktype.Registry
	ctx    context.Context
	ar     *auth.AuthResult
	ids    map[string]string // title -> block id
	embed  string            // embed backend host
	scope  string
	fresh  rrf3Fresh
	config func() *config.Config
}

// rrf3Fresh is the cluster-map freshness seam as a test double (the
// production seam is *events.Scheduler): one map, built a minute ago.
type rrf3Fresh struct{ at time.Time }

func (f rrf3Fresh) ClusterMapComputedAt([]string) (time.Time, bool) { return f.at, !f.at.IsZero() }

// pfSetup boots a database, registers the aggregating type, writes the fusion
// blocks through the store handler, embeds them with one query, and then writes
// pfLate, which keeps no vector.
func pfSetup(t *testing.T, withFold bool) *pfFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if withFold {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_block_types (name, scope, config) VALUES ($1, '_global', $2::jsonb)`,
			pfCommentType, `{"v":1,"retrieval":{"policy":"aggregate-to-parent"},"parent":{"mode":"optional"}}`); err != nil {
			t.Fatalf("register %s: %v", pfCommentType, err)
		}
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	_, plain, err := store.CreateApiKey(ctx, pool, "pf", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	f := &pfFixture{
		pool: pool, reg: reg, ctx: context.WithValue(ctx, authResultKey, ar), ar: ar,
		ids: map[string]string{}, embed: bw2NewBackend(t).srv.URL, scope: "private",
		fresh: rrf3Fresh{at: time.Now().Add(-time.Minute)},
	}
	f.config = func() *config.Config {
		c := bw2Config()
		c.Pool.DefaultBlockSensitivity = backends.SensPublic
		return c
	}

	f.store(t, pfSeed, "pf-open", "", "", "alpha")
	f.store(t, pfOpenBeta, "pf-open", "", "", "beta")
	f.store(t, pfClosed, "pf-closed", "", "", "beta")
	f.store(t, pfReference, "pf-open", "reference", "", "alpha")
	if withFold {
		f.store(t, pfIssue, "pf-closed", "", "", "beta")
		f.store(t, pfComment, "pf-open", pfCommentType, f.ids[pfIssue], "alpha")
	}
	// One query with the query-path backfill on embeds everything above. The
	// backend answers one vector for blocks and question alike, so every
	// embedded block is a semantic hit and only the filters tell them apart.
	if got := f.titles(t, nil, `{}`); !slices.Contains(got, pfSeed) {
		t.Fatalf("embedding pass names %v — the seed is not a fusion hit, probes inconclusive", got)
	}
	f.store(t, pfLate, "pf-open", "", "", "alpha")
	return f
}

// store writes one block through POST /api/store and records its id.
func (f *pfFixture) store(t *testing.T, title, category, typeName, parent string, tags ...string) {
	t.Helper()
	body := map[string]any{
		"category": category, "title": title, "tags": tags,
		"content": "body of " + title + ", stored through the write path",
	}
	if typeName != "" {
		body["type"] = typeName
	}
	if parent != "" {
		body["parent_id"] = parent
	}
	raw, _ := json.Marshal(body)
	storeH := NewStoreHandler(f.pool, staticConfigStore{cfg: f.config()}, f.reg)
	req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(f.ctx)
	rec := httptest.NewRecorder()
	storeH.HandleStore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("store %q: status %d, body %s", title, rec.Code, rec.Body.String())
	}
	var resp struct {
		Block struct {
			ID string `json:"id"`
		} `json:"block"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Block.ID == "" {
		t.Fatalf("store %q: no block id in %s (%v)", title, rec.Body.String(), err)
	}
	f.ids[title] = resp.Block.ID
}

// link writes positive Dream links from the seed to the named targets through
// dream.WriteLinks.
func (f *pfFixture) link(t *testing.T, targets ...string) {
	t.Helper()
	links := make([]dream.Link, 0, len(targets))
	for _, title := range targets {
		links = append(links, dream.Link{TargetID: f.ids[title], Relationship: "factual", Confidence: 0.95})
	}
	n, err := dream.WriteLinks(context.Background(), f.pool, f.reg.Snapshot(), f.ids[pfSeed], f.scope, 1.0, links)
	if err != nil || n != len(targets) {
		t.Fatalf("write links to %v: written=%d err=%v", targets, n, err)
	}
}

// cluster puts the named blocks into one cluster of the map, as the Louvain
// rebuild would leave it.
func (f *pfFixture) cluster(t *testing.T, members ...string) {
	t.Helper()
	const clusterID = "019e2250-0000-7000-9000-000000000001"
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts)
		 VALUES ($1::uuid, $2::text, $3, $4::uuid, 'repr', 1, '{"pf-open":1}'::jsonb)`,
		clusterID, f.scope, len(members), f.ids[members[0]]); err != nil {
		t.Fatalf("insert cluster node: %v", err)
	}
	for _, title := range members {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, $3)`,
			f.ids[title], clusterID, f.scope); err != nil {
			t.Fatalf("insert member %q: %v", title, err)
		}
	}
}

// titles runs one retrieval-only query with the given extra body fields and
// returns the fixture titles its sources name, sorted. mutate adjusts the
// config of this one request (nil = the plain fixture config).
func (f *pfFixture) titles(t *testing.T, mutate func(*config.Config), fields string) []string {
	t.Helper()
	cfg := f.config()
	if mutate != nil {
		mutate(cfg)
	}
	h := NewQueryHandler(f.pool, config.NewStore(cfg), bw2Pool(f.embed), nil, f.reg, snapshotTestAdmitter(t))
	h.SetClusterFreshness(f.fresh)

	var body map[string]any
	if err := json.Unmarshal([]byte(fields), &body); err != nil {
		t.Fatalf("fields %s: %v", fields, err)
	}
	body["query"] = bw2GoldenQuery
	body["synthesize"] = false
	body["limit"] = 20
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(string(raw))).WithContext(f.ctx)
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query %s: status %d, body %s", raw, rec.Code, rec.Body.String())
	}
	var resp struct {
		Sources []struct {
			Title string `json:"title"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	var got []string
	for _, s := range resp.Sources {
		got = append(got, s.Title)
	}
	slices.Sort(got)
	return got
}

// noBackfill keeps pfLate without a vector for the probes.
func noBackfill(c *config.Config) { c.EmbedBackfill.SyncDisabled = true }

// pfExpect checks that every name in want is present and every name in absent
// is not.
func pfExpect(t *testing.T, what string, got, want, absent []string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s: %q missing from %v", what, w, got)
		}
	}
	for _, a := range absent {
		if slices.Contains(got, a) {
			t.Errorf("%s: %q in %v — the request excluded it", what, a, got)
		}
	}
}

// TestQueryPostStageFiltersGraphExpansion: the seed links to pfClosed,
// pfReference and pfLate. Graph expansion may bring pfLate (it passes every
// filter below) and must not bring a neighbour the filter excludes.
//
// RED before the fix: the excluded neighbour is in the sources.
func TestQueryPostStageFiltersGraphExpansion(t *testing.T) {
	f := pfSetup(t, false)
	f.link(t, pfClosed, pfReference, pfLate)
	graphOn := func(c *config.Config) {
		noBackfill(c)
		c.Graph = config.Defaults().Graph
		c.Graph.Enabled = true
	}

	// Premise: with the stage off pfLate is not reachable; with it on it is.
	if got := f.titles(t, noBackfill, `{}`); slices.Contains(got, pfLate) {
		t.Fatalf("graph off names %v — pfLate is a fusion hit, the positive control proves nothing", got)
	}
	pfExpect(t, "graph on, no filter", f.titles(t, graphOn, `{}`), []string{pfLate, pfClosed}, nil)

	for _, tc := range []struct {
		name, fields   string
		want, excluded []string
	}{
		{"categories_exclude", `{"categories_exclude":["pf-closed"]}`, []string{pfSeed, pfLate}, []string{pfClosed}},
		{"category", `{"category":"pf-open"}`, []string{pfSeed, pfLate}, []string{pfClosed}},
		{"tags", `{"tags":["alpha"]}`, []string{pfSeed, pfLate}, []string{pfClosed, pfOpenBeta}},
		{"types_exclude", `{"types_exclude":["reference"]}`, []string{pfSeed, pfLate}, []string{pfReference}},
		{"block_roles_exclude", `{"block_roles_exclude":["reference"]}`, []string{pfSeed, pfLate}, []string{pfReference}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pfExpect(t, tc.fields, f.titles(t, graphOn, tc.fields), tc.want, tc.excluded)
		})
	}
}

// TestQueryPostStageFiltersClusterInjection: the seed, pfOpenBeta, pfClosed,
// pfReference and pfLate form one cluster. With the injection armed the stage may add the
// unseen pfLate and must not add pfClosed when the request excluded it.
//
// RED before the fix: pfClosed is injected.
func TestQueryPostStageFiltersClusterInjection(t *testing.T) {
	f := pfSetup(t, false)
	f.cluster(t, pfSeed, pfOpenBeta, pfClosed, pfReference, pfLate)
	clusterOn := func(c *config.Config) {
		noBackfill(c)
		c.Cluster = config.Defaults().Cluster
		c.Cluster.Enabled = true
		c.Cluster.InjectMax = 5
		c.Cluster.MinShare = 0.1
		// One cluster is the whole visible map; the size damping would zero
		// its share by construction (see the C9 fixtures).
		c.Cluster.SizeDamping = false
		c.ClusterOps.MaxStaleness = 24 * time.Hour
	}

	if got := f.titles(t, noBackfill, `{}`); slices.Contains(got, pfLate) {
		t.Fatalf("cluster off names %v — pfLate is a fusion hit, the positive control proves nothing", got)
	}
	pfExpect(t, "cluster on, no filter", f.titles(t, clusterOn, `{}`), []string{pfLate}, nil)

	for _, tc := range []struct {
		name, fields   string
		want, excluded []string
	}{
		{"categories_exclude", `{"categories_exclude":["pf-closed"]}`, []string{pfSeed, pfLate}, []string{pfClosed}},
		{"category", `{"category":"pf-open"}`, []string{pfSeed, pfLate}, []string{pfClosed}},
		{"tags", `{"tags":["alpha"]}`, []string{pfSeed, pfLate}, []string{pfClosed, pfOpenBeta}},
		{"types_exclude", `{"types_exclude":["reference"]}`, []string{pfSeed, pfLate}, []string{pfReference}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pfExpect(t, tc.fields, f.titles(t, clusterOn, tc.fields), tc.want, tc.excluded)
		})
	}
}

// TestQueryPostStageFiltersAggregateFold: pfComment (pf-open) folds onto its
// parent pfIssue (pf-closed). A request that excludes pf-closed must not get
// pfIssue through the fold; its comment, which passes, stays itself.
//
// RED before the fix: pfIssue is delivered and pfComment folded away.
func TestQueryPostStageFiltersAggregateFold(t *testing.T) {
	f := pfSetup(t, true)

	// Premise: unfiltered, the fold delivers the issue instead of the comment.
	pfExpect(t, "no filter", f.titles(t, noBackfill, `{}`), []string{pfIssue}, []string{pfComment})

	for _, tc := range []struct {
		name, fields string
	}{
		{"categories_exclude", `{"categories_exclude":["pf-closed"]}`},
		{"category", `{"category":"pf-open"}`},
		{"tags", `{"tags":["alpha"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pfExpect(t, tc.fields, f.titles(t, noBackfill, tc.fields), []string{pfComment}, []string{pfIssue})
		})
	}
}
