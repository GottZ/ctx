//go:build integration

// manage get answers graph_visible (PR #48) so the UI can hide "Open in graph"
// for blocks the ego route would 404 on. The promise is parity: for every block
// manage get returns, graph_visible must predict the ego route's answer for
// that block as focus — true ⇔ 200, false ⇔ 404. Both handlers share ONE
// registry here, exactly as cmd/ctxd/server.go wires them, so a drift between
// Set.GraphVisible and the ego allowlist (visibility.Predicate type arm) shows
// up as a mismatch, not as two separately green unit tests.
//
//	go test -tags=integration ./internal/handler/ -run TestManageGraphVisible -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestManageGraphVisible_ParityWithEgo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	cases := []struct {
		id, typeName string
		want         bool
	}{
		{"019e0048-1111-7000-9000-000000000001", "knowledge", true},    // default, full-pass
		{"019e0048-1111-7000-9000-000000000002", "reference", true},    // full-pass
		{"019e0048-1111-7000-9000-000000000003", "audit-trail", true},  // damped
		{"019e0048-1111-7000-9000-000000000004", "system-meta", false}, // excluded (topic map)
		{"019e0048-1111-7000-9000-000000000005", "checkpoint", false},  // excluded
	}
	for _, c := range cases {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, type_name)
			 VALUES ($1::uuid, 'graphvis', 'gv-'||$2, 'content', 'private', $2)`,
			c.id, c.typeName,
		); err != nil {
			t.Fatalf("insert %s block: %v", c.typeName, err)
		}
	}

	// A real key row: both handlers write context_access_log (FK on api_key_id).
	if _, err := pool.Exec(ctx,
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ('graphvis') RETURNING id
		 )
		 INSERT INTO context_api_keys (id, key_hash, label, home_scope, allowed_scopes, principal_id)
		 SELECT $1::uuid, 'graph-visible-test-hash', 'graphvis', 'private', '{shared}', p.id FROM p`,
		hgKeyA,
	); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	ar := hgAuth(hgKeyA, "private")
	mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	gh := NewGraphHandler(pool, config.NewStore(&config.Config{}), reg)

	for _, c := range cases {
		t.Run(c.typeName, func(t *testing.T) {
			code, resp := callManage(t, mh, ar, "get", c.id, nil)
			if code != http.StatusOK {
				t.Fatalf("manage get: status = %d, body %v", code, resp)
			}
			got, ok := resp["graph_visible"].(bool)
			if !ok {
				t.Fatalf("manage get: graph_visible missing or not a bool: %v", resp["graph_visible"])
			}
			if got != c.want {
				t.Errorf("graph_visible = %v, want %v", got, c.want)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/graph/ego?block="+c.id, nil)
			req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
			rec := httptest.NewRecorder()
			gh.HandleEgo(rec, req)
			wantEgo := http.StatusNotFound
			if got {
				wantEgo = http.StatusOK
			}
			if rec.Code != wantEgo {
				t.Errorf("parity: graph_visible = %v but ego focus answered %d, want %d (body %s)",
					got, rec.Code, wantEgo, rec.Body.String())
			}
		})
	}

	// Unwired registry (test wiring only — production always passes one): the
	// flag answers true, the pre-flag behavior, so the link never disappears.
	t.Run("nil_registry", func(t *testing.T) {
		bare := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
		code, resp := callManage(t, bare, ar, "get", cases[3].id, nil)
		if code != http.StatusOK {
			t.Fatalf("manage get: status = %d, body %v", code, resp)
		}
		if got, _ := resp["graph_visible"].(bool); !got {
			t.Errorf("nil registry: graph_visible = %v, want true", resp["graph_visible"])
		}
	})
}
