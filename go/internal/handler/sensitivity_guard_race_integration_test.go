//go:build integration

// Issue #547 point 5: the sensitivity downgrade guard of manage update and of
// a block batch's update items decided on a level it read through the pool,
// before the write's transaction. A raise committed between that read and the
// write let an update without confirm_sensitivity_downgrade lower the block,
// with no audit entry, and a confirmed downgrade recorded a level that no
// longer held. The interleaving is held at the BEGIN of the update's write
// transaction (a query tracer on the handler's pool), where a second writer
// raises the block and commits.
//
// TestSensitivityGuardDifference (CTX_S547_DUMP=<file>) writes 30 updates
// without concurrency and their outcomes to a file, for a comparison of two
// builds: the behaviour without concurrency stays byte-equal.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestSensitivityGuard' -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// sgAtBegin runs fn once, right before the first BEGIN of its pool.
type sgAtBegin struct {
	once sync.Once
	fn   func()
}

func (s *sgAtBegin) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.SQL)), "begin") {
		s.once.Do(s.fn)
	}
	return ctx
}

func (s *sgAtBegin) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// sgState is a block's level and its audit entry (without "at").
func (f *sbFixture) sgState(t *testing.T, id string) (string, string) {
	t.Helper()
	var sens, audit string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT sensitivity, COALESCE((metadata->'sensitivity_audit') - 'at', 'null'::jsonb)::text FROM context_blocks WHERE id = $1`, id).
		Scan(&sens, &audit); err != nil {
		t.Fatal(err)
	}
	return sens, audit
}

func (f *sbFixture) sgSeed(t *testing.T, title string, level backends.Sensitivity) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "learnings", title, title+" content", nil, nil, "private", true,
		store.SensitivityWrite{Value: level, Manual: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func TestSensitivityGuardDecidesUnderTheRowLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	raise := func(id string) func() {
		return func() {
			up := string(backends.SensCredentials)
			if _, _, err := store.UpdateBlock(context.Background(), f.pool, id, store.UpdateBlockData{Sensitivity: &up}, []string{"private"}); err != nil {
				t.Errorf("raise: %v", err)
			}
		}
	}
	call := func(t *testing.T, surface, id string, data map[string]any, before func()) (int, map[string]any) {
		pool := wcPool(t, f.pool, &sgAtBegin{fn: before})
		if surface == "manage" {
			h := NewManageHandler(pool, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
			return wcManage(t, h, k, map[string]any{"action": "update", "id": id, "data": data})
		}
		item := map[string]any{"id": id}
		for key, v := range data {
			item[key] = v
		}
		raw, _ := json.Marshal(sbItems(item))
		req := httptest.NewRequest(http.MethodPost, "/api/store/batch", bytes.NewReader(raw)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, f.cfg, f.reg).HandleStoreBatch(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	for _, surface := range []string{"manage", "batch"} {
		t.Run(surface+" without the confirm", func(t *testing.T) {
			id := f.sgSeed(t, "sg-plain-"+surface, backends.SensInternal)
			status, resp := call(t, surface, id, map[string]any{"sensitivity": "internal"}, raise(id))
			sens, audit := f.sgState(t, id)
			if sens != "credentials" || audit != "null" {
				t.Errorf("block after an unconfirmed write of internal beside a raise to credentials = %s (audit %s), want credentials kept", sens, audit)
			}
			msg, _ := resp["error"].(string)
			if status != http.StatusBadRequest || resp["code"] != "missing_fields" || !strings.Contains(msg, "sensitivity downgrade credentials → internal") {
				t.Errorf("answer = %d %v, want 400 missing_fields naming credentials → internal", status, resp)
			}
		})
		t.Run(surface+" with the confirm", func(t *testing.T) {
			id := f.sgSeed(t, "sg-confirmed-"+surface, backends.SensPersonal)
			status, resp := call(t, surface, id, map[string]any{"sensitivity": "public", "confirm_sensitivity_downgrade": true}, raise(id))
			if status != http.StatusOK {
				t.Fatalf("confirmed downgrade = %d %v, want 200", status, resp)
			}
			sens, audit := f.sgState(t, id)
			want := fmt.Sprintf(`{"by": "%s", "to": "public", "from": "credentials"}`, k.id)
			if sens != "public" || audit != want {
				t.Errorf("block after a confirmed downgrade beside a raise = %s, audit %s; want public, audit %s", sens, audit, want)
			}
		})
	}
}

// TestSensitivityGuardDifference writes 30 manage updates and batch update
// items without concurrency to CTX_S547_DUMP: answer status, code and error,
// and the block's level and audit entry (without its time and key).
func TestSensitivityGuardDifference(t *testing.T) {
	dump := os.Getenv("CTX_S547_DUMP")
	if dump == "" {
		t.Skip("set CTX_S547_DUMP to write the difference probe")
	}
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	h := NewManageHandler(f.pool, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
	levels := []backends.Sensitivity{backends.SensPublic, backends.SensInternal, backends.SensPersonal, backends.SensCredentials}
	var out bytes.Buffer
	n := 0
	for _, from := range levels {
		for _, to := range []string{"public", "internal", "personal", "credentials", "secret"} {
			for _, confirm := range []bool{false, true} {
				if n >= 30 {
					break
				}
				surface := "manage"
				if n%3 == 2 {
					surface = "batch"
				}
				id := f.sgSeed(t, fmt.Sprintf("sg-diff-%d", n), from)
				data := map[string]any{"sensitivity": to, "metadata_merge": map[string]any{"n": n}}
				if confirm {
					data["confirm_sensitivity_downgrade"] = true
				}
				var status int
				var resp map[string]any
				if surface == "manage" {
					status, resp = wcManage(t, h, k, map[string]any{"action": "update", "id": id, "data": data})
				} else {
					item := map[string]any{"id": id}
					for key, v := range data {
						item[key] = v
					}
					status, resp = f.rest(t, k, sbItems(item))
				}
				sens, audit := f.sgState(t, id)
				audit = strings.ReplaceAll(audit, k.id, "<key>")
				var meta string
				if err := f.pool.QueryRow(context.Background(),
					`SELECT ((metadata - 'sensitivity_audit'))::text FROM context_blocks WHERE id = $1`, id).Scan(&meta); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&out, "%d %s %s→%s confirm=%v: %d code=%v error=%v | %s %s %s\n",
					n, surface, from, to, confirm, status, resp["code"], resp["error"], sens, audit, meta)
				n++
			}
		}
	}
	if err := os.WriteFile(dump, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d updates written to %s", n, dump)
}
