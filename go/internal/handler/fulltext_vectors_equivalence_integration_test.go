//go:build integration

// Issue #343, part 1: migration 178 turns ts_de / ts_en from generated
// columns into ordinary columns kept by a trigger. /api/search reads them for
// its match and its rank, so its answers must be the same before and after:
// the same blocks, written through the same store calls, give the same rows
// with the same ranks in the same rank order on a database at 177 and on one
// with 178 — with and without a type filter, after inserts, upserts, title and
// content updates, metadata writes and archives.
//
// The full-text statement orders by rank and, among rows of one rank, by id
// (store.searchBlocksSQL, issue #368). The ids are generated per write, so the
// same blocks carry different ids on the two databases and their tie order
// differs between them by construction. The comparison is therefore the
// answer's rank groups in order, each group as a set of rows (issue #362).
// The tie order within one database is pinned by TestSearchFullTextTieOrder.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// fulltextFixture writes the same blocks through the store's write paths.
func fulltextFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	scopes := []string{"private"}
	words := []string{"Kompass", "Sextant", "Astrolabium", "Quarzuhr", "Lagerbestand", "compass", "inventory", "ledger"}
	ids := map[string]string{}
	for i := 0; i < 24; i++ {
		var body strings.Builder
		for j := 0; j <= i%7; j++ {
			fmt.Fprintf(&body, "%s %s steht im Regal %d. The %s holds item %d. ", words[(i+j)%len(words)], words[(i*3+j)%len(words)], j, words[(i+2*j)%len(words)], i)
		}
		typeName := []string{"knowledge", "reference", "checkpoint", "system-meta"}[i%4]
		title := fmt.Sprintf("Block %02d %s", i, words[i%len(words)])
		b, err := store.UpsertBlock(ctx, pool, "fixture", title, body.String(), []string{"t"}, nil, "private", true, store.SensitivityWrite{}, typeName)
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	// Writes that change the text, and writes that leave it alone.
	if _, err := store.UpsertBlock(ctx, pool, "fixture", "Block 00 Kompass", "Ein neuer Text über den Sextant und das ledger.", []string{"t"}, nil, "private", true, store.SensitivityWrite{}, "knowledge"); err != nil {
		t.Fatalf("upsert new content: %v", err)
	}
	newTitle := "Block 01 Astrolabium renamed"
	if _, _, err := store.UpdateBlock(ctx, pool, ids["Block 01 Sextant"], store.UpdateBlockData{Title: &newTitle}, scopes); err != nil {
		t.Fatalf("title update: %v", err)
	}
	newContent := "Quarzuhr Quarzuhr Quarzuhr inventory"
	if _, _, err := store.UpdateBlock(ctx, pool, ids["Block 05 compass"], store.UpdateBlockData{Content: &newContent}, scopes); err != nil {
		t.Fatalf("content update: %v", err)
	}
	if _, _, err := store.UpdateBlock(ctx, pool, ids["Block 04 Lagerbestand"], store.UpdateBlockData{Metadata: map[string]any{"k": "v"}}, scopes); err != nil {
		t.Fatalf("metadata update: %v", err)
	}
	if _, err := store.DeleteBlock(ctx, pool, ids["Block 08 Kompass"], scopes); err != nil {
		t.Fatalf("archive: %v", err)
	}
}

func TestSearchFullTextSameAcrossMigration178(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	before := testdb.SetupTestDBUpTo(t, 177)
	after := testdb.SetupTestDB(t)
	fulltextFixture(t, before)
	fulltextFixture(t, after)

	ar := &auth.AuthResult{IsValid: true, ApiKeyID: "00000000-0000-7000-8000-000000000000",
		HomeScope: "private", ReadScopes: []string{"private"}}
	type hit struct{ id, row string }
	search := func(pool *pgxpool.Pool, body map[string]any) []hit {
		t.Helper()
		h := NewSearchHandler(pool, staticConfigStore{cfg: &config.Config{}}, nil)
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(string(raw)))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleSearch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("search %v: status %d (body %s)", body, rec.Code, rec.Body.String())
		}
		var resp struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]hit, 0, len(resp.Results))
		for _, r := range resp.Results {
			out = append(out, hit{fmt.Sprint(r["id"]), fmt.Sprintf("%v|%v", r["title"], r["type"])})
		}
		return out
	}
	// rankGroups is an answer as its rank groups in answer order: each group
	// the rank (the statement's own expression, read per row on the answer's
	// database) and its rows as a sorted set. An answer whose ranks rise is a
	// broken rank order and fails the test.
	rankGroups := func(pool *pgxpool.Pool, body map[string]any, hits []hit) []string {
		t.Helper()
		if len(hits) >= body["limit"].(int) {
			t.Fatalf("search %v answered %d rows, its limit: the last rank group may be cut, the comparison needs whole groups", body, len(hits))
		}
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.id
		}
		rows, err := pool.Query(context.Background(),
			`SELECT id::text, GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $1)), ts_rank_cd(ts_en, plainto_tsquery('english', $1)))::float8
			   FROM context_blocks WHERE id = ANY($2::uuid[])`, body["query"], ids)
		if err != nil {
			t.Fatalf("ranks of %v: %v", body, err)
		}
		rank := map[string]float64{}
		for rows.Next() {
			var id string
			var r float64
			if err := rows.Scan(&id, &r); err != nil {
				t.Fatalf("scan rank: %v", err)
			}
			rank[id] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("ranks of %v: %v", body, err)
		}
		var groups []string
		var group []string
		for i, h := range hits {
			r, ok := rank[h.id]
			if !ok {
				t.Fatalf("search %v: no rank for row %s (%s)", body, h.id, h.row)
			}
			if i > 0 && r > rank[hits[i-1].id] {
				t.Fatalf("search %v: row %d (%s) ranks %v above the row before it (%v) — the answer is not in rank order", body, i, h.row, r, rank[hits[i-1].id])
			}
			// Within one rank the id decides, newest id first (#368).
			if i > 0 && r == rank[hits[i-1].id] && h.id >= hits[i-1].id {
				t.Fatalf("search %v: row %d (%s) has the rank of the row before it and an id not below it (%s after %s) — the tie order is not id DESC", body, i, h.row, h.id, hits[i-1].id)
			}
			group = append(group, h.row)
			if i == len(hits)-1 || rank[hits[i+1].id] != r {
				slices.Sort(group)
				groups = append(groups, fmt.Sprintf("%v %v", r, group))
				group = nil
			}
		}
		return groups
	}

	nonEmpty := 0
	for _, q := range []string{"Kompass", "Sextant", "Quarzuhr", "inventory", "ledger", "Regal", "compass inventory", "Astrolabium renamed", "nirgendwo"} {
		for _, filter := range []map[string]any{
			{},
			{"types": []string{"knowledge"}},
			{"types": []string{"checkpoint"}},
			{"types_exclude": []string{"checkpoint", "system-meta"}},
		} {
			for _, compact := range []bool{false, true} {
				body := map[string]any{"query": q, "limit": 50, "compact": compact}
				for k, v := range filter {
					body[k] = v
				}
				bh, ah := search(before, body), search(after, body)
				b, a := rankGroups(before, body, bh), rankGroups(after, body, ah)
				if !reflect.DeepEqual(b, a) {
					t.Errorf("search %v differs:\n  at 177: %v\n  at 178: %v", body, b, a)
				}
				if len(ah) > 0 {
					nonEmpty++
				}
			}
		}
	}
	if nonEmpty < 40 {
		t.Errorf("only %d of the compared searches returned rows; the comparison proves little", nonEmpty)
	}
}
