//go:build integration

// Issue #343, part 1: migration 178 turns ts_de / ts_en from generated
// columns into ordinary columns kept by a trigger. /api/search reads them for
// its match and its rank, so its answers must be the same before and after:
// the same blocks, written through the same store calls, give the same rows in
// the same order on a database at 177 and on one with 178 — with and without a
// type filter, after inserts, upserts, title and content updates, metadata
// writes and archives.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	search := func(pool *pgxpool.Pool, body map[string]any) []string {
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
		out := make([]string, 0, len(resp.Results))
		for _, r := range resp.Results {
			out = append(out, fmt.Sprintf("%v|%v|%v", r["title"], r["type"], r["score"]))
		}
		return out
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
				b, a := search(before, body), search(after, body)
				if !reflect.DeepEqual(b, a) {
					t.Errorf("search %v differs:\n  at 177: %v\n  at 178: %v", body, b, a)
				}
				if len(a) > 0 {
					nonEmpty++
				}
			}
		}
	}
	if nonEmpty < 40 {
		t.Errorf("only %d of the compared searches returned rows; the comparison proves little", nonEmpty)
	}
}
