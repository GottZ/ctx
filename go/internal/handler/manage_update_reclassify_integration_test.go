//go:build integration

// WF T4 gate (design/01 §4.5, seam 5): the manage-update path re-runs the
// auto-classifier. Pre-T4 this path never classified — a title edit that
// gained an audit pattern left the block 'knowledge' forever (seam 5 in
// inventory/block-types.md §9). The manual-override probe pins that
// type_source='manual' survives the same edit untouched.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

func reclassifyTestBlock(t *testing.T, pool *pgxpool.Pool, title, typeName, typeSource string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, created_at, updated_at)
		 VALUES ('test_reclassify', $1, 'reclassify-content', 'private', $2, $3, $4, $4)
		 RETURNING id::text`,
		title, typeName, typeSource, time.Now().UTC(),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert reclassify block: %v", err)
	}
	return id
}

func TestManageUpdateReclassify_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	ar := &auth.AuthResult{IsValid: true, HomeScope: "private", ReadScopes: []string{"private"}}

	callUpdate := func(id string, data map[string]any) map[string]any {
		raw, _ := json.Marshal(data)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
		h.handleUpdate(rec, req, ar, manageRequest{Action: "update", ID: id, Data: raw})
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode update response: %v (body %s)", err, rec.Body.String())
		}
		return resp
	}

	typeOf := func(id string) (name, source string) {
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).Scan(&name, &source); err != nil {
			t.Fatalf("read type: %v", err)
		}
		return name, source
	}

	// Auto block gains an audit pattern via title update → re-classified.
	// RED pre-T4: the update path had no classify hook at all.
	t.Run("auto_block_reclassified_on_title_update", func(t *testing.T) {
		id := reclassifyTestBlock(t, pool, "Plain Notes", "knowledge", "auto")
		resp := callUpdate(id, map[string]any{"title": "Session 9 Handover Protokoll"})
		if ok, _ := resp["success"].(bool); !ok {
			t.Fatalf("update failed: %v", resp)
		}
		name, source := typeOf(id)
		if name != "audit-trail" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (audit-trail, auto)", name, source)
		}
	})

	// Manual block with the SAME kind of edit keeps its type (manual wins).
	t.Run("manual_block_keeps_type_on_title_update", func(t *testing.T) {
		id := reclassifyTestBlock(t, pool, "Referenzdokument", "reference", "manual")
		resp := callUpdate(id, map[string]any{"title": "Session 10 Handover Referenz"})
		if ok, _ := resp["success"].(bool); !ok {
			t.Fatalf("update failed: %v", resp)
		}
		name, source := typeOf(id)
		if name != "reference" || source != "manual" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (reference, manual)", name, source)
		}
	})

	// Content-only update: no title/metadata change ⇒ hook does not run
	// (match-only asymmetry documented in ClassifyBlockAfterUpsert).
	t.Run("content_only_update_skips_hook", func(t *testing.T) {
		id := reclassifyTestBlock(t, pool, "Session Alt", "audit-trail", "auto")
		resp := callUpdate(id, map[string]any{"content": "neuer inhalt ohne titelwechsel"})
		if ok, _ := resp["success"].(bool); !ok {
			t.Fatalf("update failed: %v", resp)
		}
		name, _ := typeOf(id)
		if name != "audit-trail" {
			t.Errorf("type_name = %q, want audit-trail (content-only edit must not touch the type)", name)
		}
	})
}
