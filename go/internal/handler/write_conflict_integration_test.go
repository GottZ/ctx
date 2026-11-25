//go:build integration

// Issue #547 point 4: a write that lost to concurrent writes and wrote
// nothing answers the retryable 409 write_conflict on every surface, not
// 500 internal with an ERROR line:
//
//   - the bounded re-runs of a write whose block changed scope before its
//     lock pass, each run (an archive, a scope move, a block batch's update
//     item — the item named): held by a query tracer on the handler's pool,
//     which moves the block to the next of four scopes right before the
//     write takes its first supersedes lock;
//
//   - a deadlock (40P01) PostgreSQL ends the write's transaction with: a
//     block batch over two blocks and a second transaction that holds the
//     second and then asks for the first.
//
//     go test -tags=integration -p 1 ./internal/handler/ -run 'TestWriteConflict' -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// wcMover is a pgx tracer that runs move right before the first statement of
// each transaction of its pool that takes a supersedes scope lock — between
// the write's scope read and its lock — at most limit times.
type wcMover struct {
	mu    sync.Mutex
	inTx  bool
	moved bool
	limit int
	runs  int
	move  func()
}

func (m *wcMover) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(strings.TrimSpace(d.SQL))
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case strings.HasPrefix(sql, "begin"):
		m.inTx, m.moved = true, false
	case strings.HasPrefix(sql, "commit"), strings.HasPrefix(sql, "rollback"):
		m.inTx = false
	case m.inTx && !m.moved && m.runs < m.limit && strings.Contains(d.SQL, "pg_advisory_xact_lock(hashtext('ctx:supersedes:'"):
		m.moved = true
		m.runs++
		m.move()
	}
	return ctx
}

func (m *wcMover) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func wcPool(t *testing.T, base *pgxpool.Pool, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config()
	cfg.ConnConfig.Tracer = tracer
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// wcManage posts one /api/manage body as k.
func wcManage(t *testing.T, h *ManageHandler, k sbKey, payload map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body)).WithContext(k.ctx)
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// wcNoErrorLine fails when a record at ERROR was written, and returns the
// store's lines of a write that gave up after its re-runs.
func wcNoErrorLine(t *testing.T, logs *rejectLogCapture) []rejectLogRecord {
	t.Helper()
	for _, r := range logs.all() {
		if r.level >= slog.LevelError {
			t.Errorf("ERROR line for a write conflict: %+v", r)
		}
	}
	return logs.withMsg("store: write gave up, its block kept changing scope before its lock; nothing written")
}

// TestWriteConflictAfterExhaustedRuns: the block moves before every lock of
// the write, three times — each surface answers 409 write_conflict (the batch
// naming its item), without an ERROR line, and nothing is written.
func TestWriteConflictAfterExhaustedRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	scopes := []string{"wc-s1", "wc-s2", "wc-s3", "wc-s4"}
	k := f.key(t, false, scopes...)

	surfaces := []struct {
		name string
		call func(t *testing.T, pool *pgxpool.Pool, id string) (status int, code, msg string)
	}{
		{"REST batch update item", func(t *testing.T, pool *pgxpool.Pool, id string) (int, string, string) {
			raw, _ := json.Marshal(sbItems(sbuUpdate(id, "content", "changed by the batch")))
			req := httptest.NewRequest(http.MethodPost, "/api/store/batch", bytes.NewReader(raw)).WithContext(k.ctx)
			rec := httptest.NewRecorder()
			NewStoreHandler(pool, f.cfg, f.reg).HandleStoreBatch(rec, req)
			var m map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &m)
			c, _ := m["code"].(string)
			e, _ := m["error"].(string)
			return rec.Code, c, e
		}},
		{"MCP store_batch update item", func(t *testing.T, pool *pgxpool.Pool, id string) (int, string, string) {
			h := NewMCPHandler(MCPConfig{Pool: pool, Cfg: f.cfg, Blocktypes: f.reg})
			g := &sbFixture{pool: pool, cfg: f.cfg, reg: f.reg, mcp: h}
			a := g.mcpCall(t, k, "store_batch", map[string]any{"items": []any{map[string]any{"id": id, "content": "changed by the batch"}}})
			st := http.StatusOK
			if a.IsError {
				st = http.StatusConflict
			}
			return st, a.Code, a.Text
		}},
		{"REST manage delete", func(t *testing.T, pool *pgxpool.Pool, id string) (int, string, string) {
			h := NewManageHandler(pool, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
			st, m := wcManage(t, h, k, map[string]any{"action": "delete", "id": id})
			c, _ := m["code"].(string)
			e, _ := m["error"].(string)
			return st, c, e
		}},
		{"REST manage update scope move", func(t *testing.T, pool *pgxpool.Pool, id string) (int, string, string) {
			h := NewManageHandler(pool, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
			st, m := wcManage(t, h, k, map[string]any{"action": "update", "id": id, "data": map[string]any{"scope": "private"}})
			c, _ := m["code"].(string)
			e, _ := m["error"].(string)
			return st, c, e
		}},
		{"MCP archive", func(t *testing.T, pool *pgxpool.Pool, id string) (int, string, string) {
			h := NewMCPHandler(MCPConfig{Pool: pool, Cfg: f.cfg, Blocktypes: f.reg})
			g := &sbFixture{pool: pool, cfg: f.cfg, reg: f.reg, mcp: h}
			a := g.mcpCall(t, k, "archive", map[string]any{"id": id})
			st := http.StatusOK
			if a.IsError {
				st = http.StatusConflict
			}
			return st, a.Code, a.Text
		}},
	}
	for i, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			id := f.seedIn(t, scopes[0], fmt.Sprintf("wc-exhausted-%d", i), "the content")
			before := f.row(t, id)
			next := 0
			mover := &wcMover{limit: 3, move: func() {
				next++
				to := scopes[next]
				if _, _, err := store.UpdateBlock(context.Background(), f.pool, id, store.UpdateBlockData{Scope: &to}, scopes); err != nil {
					t.Errorf("mover: %v", err)
				}
			}}
			pool := wcPool(t, f.pool, mover)
			logs := installRejectLogCapture(t)
			status, code, msg := s.call(t, pool, id)
			if status != http.StatusConflict || code != "write_conflict" {
				t.Errorf("%s after three moves = %d %q %q, want 409 write_conflict", s.name, status, code, msg)
			}
			if strings.Contains(s.name, "batch") && !strings.HasPrefix(msg, "items[0]: ") {
				t.Errorf("the batch's conflict does not name its item: %q", msg)
			}
			if lines := wcNoErrorLine(t, logs); len(lines) != 1 || lines[0].level != slog.LevelInfo {
				t.Errorf("store lines of the exhausted write = %+v, want one at INFO", lines)
			}
			if mover.runs != 3 {
				t.Errorf("the write ran %d times before its last lock, want 3", mover.runs)
			}
			after := f.row(t, id)
			if after.content != before.content || after.scope != scopes[3] {
				t.Errorf("row after the refused write = %+v, want the content untouched in %s", after, scopes[3])
			}
			var archived bool
			if err := f.pool.QueryRow(context.Background(), `SELECT is_archived FROM context_blocks WHERE id = $1`, id).Scan(&archived); err != nil || archived {
				t.Errorf("archived after the refused write = %v (%v)", archived, err)
			}
		})
	}
}

// TestWriteConflictOfADeadlock: a block batch over A and B locks A and waits
// for B, which a second transaction holds and then asks for A. PostgreSQL ends
// the batch (it waited first) with 40P01; the batch answers 409
// write_conflict naming B's item, writes nothing, and logs no ERROR line.
func TestWriteConflictOfADeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, false)
	a := f.seed(t, "wc-dl-a", "alpha")
	b := f.seed(t, "wc-dl-b", "beta")

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var pid uint32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR NO KEY UPDATE`, b); err != nil {
		t.Fatal(err)
	}
	logs := installRejectLogCapture(t)
	done := make(chan struct{})
	var status int
	var resp map[string]any
	go func() {
		defer close(done)
		status, resp = f.restNoFatal(k, sbItems(sbuUpdate(a, "content", "alpha by the batch"), sbuUpdate(b, "content", "beta by the batch")))
	}()
	testdb.WaitBlockedBy(t, f.pool, pid, done, "the batch", "")
	if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR NO KEY UPDATE`, a); err != nil {
		t.Fatalf("holder: %v (the batch should have been the deadlock's victim)", err)
	}
	<-done
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusConflict || resp["code"] != "write_conflict" {
		t.Fatalf("batch beside the deadlock = %d %v, want 409 write_conflict", status, resp)
	}
	if msg, _ := resp["error"].(string); !strings.HasPrefix(msg, "items[1]: ") {
		t.Errorf("the deadlock's conflict does not name the item that waited: %q", msg)
	}
	wcNoErrorLine(t, logs)
	if r := f.row(t, a); r.content != "alpha" {
		t.Errorf("A after the refused batch = %q, want it untouched", r.content)
	}
}
