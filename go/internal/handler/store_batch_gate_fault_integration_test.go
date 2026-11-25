//go:build integration

// Issue #547 point 7c (bot review of #550): a failure of the resolve in a
// block batch's update gate — its prefix query cut off by a cancelled request
// — was answered 422 unknown_block, as if the block did not exist. It is a
// fault: 500 internal with its line, and nothing written.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// gfCancelAt cancels the request when its pool sends the id prefix resolve.
type gfCancelAt struct{ cancel context.CancelFunc }

func (g *gfCancelAt) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "id::text LIKE $1 || '%'") {
		g.cancel()
	}
	return ctx
}

func (g *gfCancelAt) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestStoreBatchUpdateGateFaultIsNoUnknownBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	id := f.seed(t, "gf-target", "the content")
	rctx, cancel := context.WithCancel(k.ctx)
	defer cancel()
	pool := wcPool(t, f.pool, &gfCancelAt{cancel: cancel})
	logs := installRejectLogCapture(t)
	raw, _ := json.Marshal(sbItems(sbuUpdate(id[:13], "content", "changed")))
	req := httptest.NewRequest(http.MethodPost, "/api/store/batch", bytes.NewReader(raw)).WithContext(rctx)
	rec := httptest.NewRecorder()
	NewStoreHandler(pool, f.cfg, f.reg).HandleStoreBatch(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != http.StatusInternalServerError || m["code"] != "internal" {
		t.Errorf("batch whose update gate's resolve failed = %d %v, want 500 internal", rec.Code, m)
	}
	var lines int
	for _, r := range logs.withMsg("store batch: resolve update target error") {
		if r.level >= slog.LevelError {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("ERROR lines of the gate's fault = %d, want 1 (all: %+v)", lines, logs.all())
	}
	if r := f.row(t, id); r.content != "the content" {
		t.Errorf("content after the failed batch = %q, want it untouched", r.content)
	}
}
