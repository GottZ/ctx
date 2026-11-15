//go:build integration

// Issue #197, the read and the guard side of a declared supersedes.
//
//   - TestQueryDeclaredSupersedes: a block superseded through a PINNED
//     supersedes link is not returned by a query without a date, whether or not
//     its successor made it into the result set — the successor here has no
//     vector and shares no words with the question, the window a fresh
//     correction lives in. A question with a date still returns it. A block
//     superseded by an UNPINNED (dream-guessed) link keeps the presence rule.
//   - TestGuardDeclaredSupersedes: the guard does not archive the successor as
//     a duplicate of the block it declares to supersede.
//   - TestDreamKeepsPinnedSupersedes: dream does not re-classify a pinned link
//     and does not write the reverse of one.
//
//	go test -tags=integration ./internal/handler/ -run 'TestQueryDeclaredSupersedes|TestGuardDeclaredSupersedes|TestDreamKeepsPinnedSupersedes' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

// sdQuestionMarker is the one input the fake embed backend answers; every
// other input (a block's content in the pre-search backfill) fails, so a block
// without a vector stays without one for the whole test.
const sdQuestionMarker = "sdq rotation"

// sdQueryVector is the vector the fake backend returns for the question.
func sdQueryVector() []float32 {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = float32((i % 2) * 2)
	}
	return v
}

func sdQueryEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	vec := sdQueryVector()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte(sdQuestionMarker)) {
			http.Error(w, "sd: only the question embeds", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *sdFixture) query(t *testing.T, question string) []sourceResponse {
	t.Helper()
	srv := sdQueryEmbedServer(t)
	h := NewQueryHandler(f.pool, config.NewStore(w023Config(false)), embedPool(srv.URL), nil, f.reg, snapshotTestAdmitter(t))
	body, _ := json.Marshal(map[string]any{"query": question, "synthesize": false, "limit": 10})
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		ApiKeyID: f.ar.ApiKeyID, HomeScope: sdHome, ReadScopes: []string{sdHome}, IsValid: true,
	}))
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query %q: status %d body %s", question, rec.Code, rec.Body.String())
	}
	var resp queryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return resp.Sources
}

func sdFind(sources []sourceResponse, id string) (sourceResponse, bool) {
	for _, s := range sources {
		if s.ID == id {
			return s, true
		}
	}
	return sourceResponse{}, false
}

func TestQueryDeclaredSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()

	embed := func(id string) {
		t.Helper()
		if err := store.StoreEmbedding(ctx, f.pool, id, "test-embed", sdQueryVector()); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}

	// Declared: the old block carries the question's vector; its successor has
	// none and shares no word with the question.
	declaredOld := f.put(t, "sdq-declared-old", "the key turnover happens every six weeks", nil)
	embed(declaredOld)
	declaredNew := f.put(t, "fortnight-cycle", "credentials now cycle each fortnight",
		map[string]any{"supersedes": declaredOld})

	// Dream-guessed: same shape, the link written by dream itself (unpinned).
	guessedOld := f.put(t, "badge-renewal", "the badge renewal happens yearly", nil)
	embed(guessedOld)
	guessedNew := f.put(t, "badge-renewal-update", "passes now renew biannually", nil)
	if n, err := dream.WriteLinks(ctx, f.pool, f.reg.Snapshot(), guessedNew, sdHome, 1.0,
		[]dream.Link{{TargetID: guessedOld, Relationship: "supersedes", Confidence: 0.9}}); err != nil || n != 1 {
		t.Fatalf("dream link: n=%d err=%v", n, err)
	}

	t.Run("dateless_drops_declared_target", func(t *testing.T) {
		src := f.query(t, "what is the "+sdQuestionMarker+" policy")
		if _, ok := sdFind(src, declaredNew); ok {
			t.Fatalf("fixture broken: the successor was retrieved, the window is not reproduced")
		}
		if _, ok := sdFind(src, declaredOld); ok {
			t.Errorf("the declared-superseded block %s was returned by a dateless query", declaredOld)
		}
	})

	t.Run("dateless_keeps_presence_rule_for_dream_edges", func(t *testing.T) {
		// Regression pin, green before and after: an unpinned dream edge only
		// drops its target when the successor is in the result set.
		src := f.query(t, "what is the "+sdQuestionMarker+" policy")
		if _, ok := sdFind(src, guessedNew); ok {
			t.Fatalf("fixture broken: the dream successor was retrieved")
		}
		if _, ok := sdFind(src, guessedOld); !ok {
			t.Errorf("a block superseded only by a dream guess vanished: %v", src)
		}
	})

	t.Run("dated_question_keeps_history", func(t *testing.T) {
		src := f.query(t, "what was the "+sdQuestionMarker+" policy on 2026-11-10")
		s, ok := sdFind(src, declaredOld)
		if !ok {
			t.Fatalf("a dated question lost the superseded block: %v", src)
		}
		if s.SupersededBy == nil || *s.SupersededBy != declaredNew {
			t.Errorf("superseded_by = %v, want %s", s.SupersededBy, declaredNew)
		}
	})

	t.Run("archived_successor_releases_target", func(t *testing.T) {
		// The pinned link of an archived successor stays until the daily
		// sweep; it no longer settles the target, which is retrievable again.
		if _, err := store.DeleteBlock(ctx, f.pool, declaredNew, []string{sdHome}); err != nil {
			t.Fatalf("archive successor: %v", err)
		}
		if _, ok := f.link(t, declaredNew, declaredOld); !ok {
			t.Fatalf("premise: the link of the archived successor is gone before the sweep")
		}
		src := f.query(t, "what is the "+sdQuestionMarker+" policy")
		if _, ok := sdFind(src, declaredOld); !ok {
			t.Errorf("the target of an archived successor stayed hidden: %v", src)
		}
	})
}

// sdGuardUntilChecked runs guard batches until every id carries a guard
// verdict. The pick is FOR UPDATE SKIP LOCKED, and HandleStore's detached
// temporal enrichment updates the row it just wrote, so a batch right after a
// store can skip the block while that update holds its lock.
func (f *sdFixture) guardUntilChecked(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 100; attempt++ {
		if _, err := guard.RunGuardBatch(ctx, f.pool, f.reg.Snapshot(), 100); err != nil {
			t.Fatalf("guard batch: %v", err)
		}
		var pending int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks
			  WHERE id = ANY($1::uuid[]) AND (metadata->>'guard_checked_at') IS NULL`, ids).Scan(&pending); err != nil {
			t.Fatalf("pending: %v", err)
		}
		if pending == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("blocks %v never got a guard verdict", ids)
}

func TestGuardDeclaredSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()

	vec := w023Embedding(4)
	near := append([]float32(nil), vec...)
	near[600] = 0.13 // one component moved: cosine similarity well above 0.98

	old := f.put(t, "sdg-original", "the retention period is thirty days", nil)
	if err := store.StoreEmbedding(ctx, f.pool, old, "test-embed", vec); err != nil {
		t.Fatalf("embed old: %v", err)
	}
	f.guardUntilChecked(t, old)
	if st := f.state(t, old); st.guardStatus != "clean" {
		t.Fatalf("premise: original checked %q, want clean", st.guardStatus)
	}

	correction := f.put(t, "sdg-correction", "the retention period is thirty-one days",
		map[string]any{"supersedes": old})
	if err := store.StoreEmbedding(ctx, f.pool, correction, "test-embed", near); err != nil {
		t.Fatalf("embed correction: %v", err)
	}
	var sim float64
	if err := f.pool.QueryRow(ctx,
		`SELECT 1 - (a.embedding::halfvec(1024) <=> b.embedding::halfvec(1024))
		   FROM context_blocks a, context_blocks b WHERE a.id = $1::uuid AND b.id = $2::uuid`,
		old, correction).Scan(&sim); err != nil || sim < 0.98 {
		t.Fatalf("premise: similarity %.4f (err %v), want >= 0.98", sim, err)
	}
	f.guardUntilChecked(t, correction)

	c := f.state(t, correction)
	if c.archived || c.guardStatus == "archived_dup" || c.guardStatus == "possible_duplicate" {
		t.Errorf("the guard decided the correction against the block it supersedes: %+v", c)
	}
	if c.guardStatus != "clean" {
		t.Errorf("correction guard_status = %q, want clean", c.guardStatus)
	}
	if o := f.state(t, old); o.archived || o.lifecycle != "snapshot" || o.supersededBy != correction {
		t.Errorf("original = %+v, want active snapshot superseded by %s", o, correction)
	}
}

// TestGuardPinnedPairBothDirections: the checked block may be either end of a
// pinned supersedes link. Here the TARGET stays knowledge (an operator pinned a
// dream guess below the snapshot confidence) and is checked first; it must not
// be archived against its successor, nor the successor against it.
func TestGuardPinnedPairBothDirections(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()

	vec := w023Embedding(5)
	near := append([]float32(nil), vec...)
	near[700] = 0.13

	older := f.put(t, "sdgb-note", "the backup window opens at two", nil)
	newer := f.put(t, "sdgb-note-v2", "the backup window opens at three", nil)
	if n, err := dream.WriteLinks(ctx, f.pool, f.reg.Snapshot(), newer, sdHome, 1.0,
		[]dream.Link{{TargetID: older, Relationship: "supersedes", Confidence: 0.5}}); err != nil || n != 1 {
		t.Fatalf("dream link: n=%d err=%v", n, err)
	}
	if res, err := store.DreamLinkResolve(ctx, f.pool, newer, older, "supersedes", "confirm", "", []string{sdHome}); err != nil || res == nil || !res.Pinned {
		t.Fatalf("confirm: %+v %v", res, err)
	}
	if st := f.state(t, older); st.lifecycle != "knowledge" {
		t.Fatalf("premise: target %+v, want knowledge (below the snapshot confidence)", st)
	}
	for id, v := range map[string][]float32{older: vec, newer: near} {
		if err := store.StoreEmbedding(ctx, f.pool, id, "test-embed", v); err != nil {
			t.Fatalf("embed: %v", err)
		}
	}
	f.guardUntilChecked(t, older, newer)
	for _, id := range []string{older, newer} {
		if st := f.state(t, id); st.archived || st.guardStatus != "clean" {
			t.Errorf("block %s after the guard = %+v, want active and clean", id, st)
		}
	}
}

func TestDreamKeepsPinnedSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()
	set := f.reg.Snapshot()

	// The writer upserts an OLDER block to supersede a newer one — no
	// direction rule over created_at.
	older := f.put(t, "sdd-n", "the older block, now the authority", nil)
	newer := f.put(t, "sdd-t", "the newer block, now outdated", nil)
	f.put(t, "sdd-n", "the older block, now the authority", map[string]any{"supersedes": newer})

	t.Run("no_reclassification", func(t *testing.T) {
		if _, err := dream.WriteLinks(ctx, f.pool, set, older, sdHome, 1.0,
			[]dream.Link{{TargetID: newer, Relationship: "topical", Confidence: 0.8}}); err != nil {
			t.Fatalf("write links: %v", err)
		}
		l, ok := f.link(t, older, newer)
		if !ok || l.relationship != "supersedes" || !l.pinned || l.raw != 1 {
			t.Errorf("pinned declaration after a dream cycle = %+v (ok=%v)", l, ok)
		}
		if st := f.state(t, newer); st.lifecycle != "snapshot" || st.supersededBy != older {
			t.Errorf("target after a dream cycle = %+v", st)
		}
	})

	t.Run("no_reverse_edge", func(t *testing.T) {
		if _, err := dream.WriteLinks(ctx, f.pool, set, newer, sdHome, 1.0,
			[]dream.Link{{TargetID: older, Relationship: "supersedes", Confidence: 0.9}}); err != nil {
			t.Fatalf("write links: %v", err)
		}
		if l, ok := f.link(t, newer, older); ok && l.relationship == "supersedes" {
			t.Errorf("dream wrote the reverse of a pinned supersedes: %+v", l)
		}
		if st := f.state(t, older); st.lifecycle == "snapshot" {
			t.Errorf("the declared successor became a snapshot: %+v", st)
		}
	})
}
