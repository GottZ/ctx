//go:build integration

// W05.5 wiring gate (design/05 §4.2/§4.6): the handler half of the ego cache
// arm. Three states are pinned, because each of them is a different way to end
// up on the SQL path:
//
//	unwired source          → SQL (nil tolerance — pre-wire boot, tests)
//	wired + flag off        → SQL (the wave ships dark, §4.7)
//	wired + flag on + Fresh → cache (source="cache" in the envelope)
//	wired + flag on + stale → SQL (state gate says no)

package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hgFakeCacheSource is a hand-wired egoCacheSource: it answers the state gate
// without standing up a scheduler.
type hgFakeCacheSource struct {
	snap *graphcache.Snapshot
	age  time.Duration
	ok   bool
}

func (f *hgFakeCacheSource) GraphCacheServe() (*graphcache.Snapshot, time.Duration, bool) {
	return f.snap, f.age, f.ok
}

// hgDoWith runs HandleEgo on a PREBUILT handler (hgDo builds its own with a
// zero config — this variant is what lets the flag and the source vary).
func hgDoWith(t *testing.T, h *GraphHandler, ar *auth.AuthResult, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/graph/ego?"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleEgo(rec, req)
	return rec
}

// hgEgoSource runs HandleEgo with an injected cache source + serve flag and
// returns the budget_report.source of the response envelope.
func hgEgoSource(t *testing.T, pool *pgxpool.Pool, src egoCacheSource, serveEgo bool, block string) string {
	t.Helper()
	cfg := &config.Config{}
	cfg.GraphCache.ServeEgo = serveEgo
	h := NewGraphHandler(pool, config.NewStore(cfg), blocktype.NewRegistry())
	if src != nil {
		h.SetGraphCache(src)
	}
	rec := hgDoWith(t, h, hgAuth(hgKeyA, "private"), "block="+block)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		BudgetReport struct {
			Source     string `json:"source"`
			CacheAgeMs int64  `json:"cache_age_ms"`
		} `json:"budget_report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env.BudgetReport.Source
}

func TestHandleEgo_CacheArmWiring(t *testing.T) {
	pool := hgSetup(t)
	snap, err := graphcache.Build(context.Background(), pool)
	if err != nil {
		t.Fatalf("graphcache build: %v", err)
	}
	fresh := &hgFakeCacheSource{snap: snap, age: 3 * time.Second, ok: true}
	notFresh := &hgFakeCacheSource{snap: snap, ok: false} // Degraded/Failed/Empty

	cases := []struct {
		name string
		src  egoCacheSource
		flag bool
		want string
		why  string
	}{
		{"unwired_source", nil, true, graphcache.SourceSQL, "no source wired ⇒ SQL, whatever the flag says"},
		{"flag_off", fresh, false, graphcache.SourceSQL, "serve_ego=false is the shipped default"},
		{"flag_on_fresh", fresh, true, graphcache.SourceCache, "state Fresh + flag on ⇒ the arm answers"},
		{"flag_on_not_fresh", notFresh, true, graphcache.SourceSQL, "Degraded/Failed/Empty ⇒ transparent SQL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hgEgoSource(t, pool, tc.src, tc.flag, hgShared); got != tc.want {
				t.Errorf("source = %q, want %q (%s)", got, tc.want, tc.why)
			}
		})
	}
}
