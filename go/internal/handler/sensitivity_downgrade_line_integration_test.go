//go:build integration

// Issue #547, review round 1 (F8): the WARN line "confirmed sensitivity
// downgrade" names the level the store held the row at, once per downgrade
// the write committed. It came from the handler's read before the write: a
// raise committed between that read and the store's lock left the line
// naming the earlier level (the audit entry named the right one), and a
// downgrade that only the store's decision made got no line. The early
// refusal of an unconfirmed downgrade carries its code, as the store's does.
// The raise (B) commits at the start of the store's locking read of A.
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// sgStartHook runs hook once right before the first statement match accepts.
type sgStartHook struct {
	match func(sql string) bool
	hook  func()
	done  bool
}

func (h *sgStartHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !h.done && h.hook != nil && h.match(d.SQL) {
		h.done = true
		h.hook()
	}
	return ctx
}

func (h *sgStartHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

const sgLine = "manage: confirmed sensitivity downgrade"

func TestSensitivityDowngradeLineNamesTheHeldLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	tr := &sgStartHook{match: func(sql string) bool {
		return strings.Contains(sql, "sensitivity FROM context_blocks") && strings.Contains(sql, "metadata ?")
	}}
	poolA := wcPool(t, f.pool, tr)
	h := NewManageHandler(poolA, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
	raiseAtTheLock := func(id string) {
		tr.done = false
		tr.hook = func() {
			up := "credentials"
			if _, _, err := store.UpdateBlock(context.Background(), f.pool, id, store.UpdateBlockData{Sensitivity: &up}, []string{"private"}); err != nil {
				t.Errorf("raise: %v", err)
			}
		}
	}
	update := func(id, level string) (int, map[string]any) {
		return wcManage(t, h, k, map[string]any{"action": "update", "id": id,
			"data": map[string]any{"sensitivity": level, "confirm_sensitivity_downgrade": true}})
	}

	t.Run("a raise before the lock", func(t *testing.T) {
		id := f.sgSeed(t, "sg-line raised", backends.SensInternal)
		raiseAtTheLock(id)
		logs := installRejectLogCapture(t)
		if st, m := update(id, "public"); st != http.StatusOK {
			t.Fatalf("update: %d %v", st, m)
		}
		lines := logs.withMsg(sgLine)
		if len(lines) != 1 || lines[0].attrs["from"] != "credentials" || lines[0].attrs["to"] != "public" {
			t.Errorf("lines %+v, want one naming credentials → public (the level the store held)", lines)
		}
	})
	t.Run("a downgrade only the store's decision makes", func(t *testing.T) {
		id := f.sgSeed(t, "sg-line only store", backends.SensInternal)
		raiseAtTheLock(id) // the handler's read sees internal → internal: no downgrade
		logs := installRejectLogCapture(t)
		if st, m := update(id, "internal"); st != http.StatusOK {
			t.Fatalf("update: %d %v", st, m)
		}
		if sens, audit := f.sgState(t, id); sens != "internal" || !strings.Contains(audit, `"from": "credentials"`) {
			t.Fatalf("block %s, audit %s — want the store's confirmed downgrade from credentials", sens, audit)
		}
		lines := logs.withMsg(sgLine)
		if len(lines) != 1 || lines[0].attrs["from"] != "credentials" || lines[0].attrs["to"] != "internal" {
			t.Errorf("lines %+v, want one naming credentials → internal", lines)
		}
	})
	t.Run("a batch update item, a raise before the lock", func(t *testing.T) {
		id := f.sgSeed(t, "sg-line batch", backends.SensInternal)
		raiseAtTheLock(id)
		logs := installRejectLogCapture(t)
		if st, m := f.restOn(t, poolA, k, sbItems(map[string]any{"id": id, "sensitivity": "public", "confirm_sensitivity_downgrade": true})); st != http.StatusOK {
			t.Fatalf("batch: %d %v", st, m)
		}
		lines := logs.withMsg(sgLine)
		if len(lines) != 1 || lines[0].attrs["from"] != "credentials" || lines[0].attrs["to"] != "public" {
			t.Errorf("lines %+v, want one naming credentials → public", lines)
		}
	})
	t.Run("no downgrade, no line", func(t *testing.T) {
		tr.hook = nil
		id := f.sgSeed(t, "sg-line none", backends.SensInternal)
		logs := installRejectLogCapture(t)
		if st, m := update(id, "personal"); st != http.StatusOK {
			t.Fatalf("update: %d %v", st, m)
		}
		if lines := logs.withMsg(sgLine); len(lines) != 0 {
			t.Errorf("lines %+v for a raise, want none", lines)
		}
	})
	t.Run("the early refusal carries its code", func(t *testing.T) {
		tr.hook = nil
		id := f.sgSeed(t, "sg-line refused", backends.SensCredentials)
		st, m := wcManage(t, h, k, map[string]any{"action": "update", "id": id, "data": map[string]any{"sensitivity": "public"}})
		if st != http.StatusBadRequest || m["code"] != "missing_fields" {
			t.Errorf("unconfirmed downgrade: %d %v, want 400 missing_fields", st, m)
		}
	})
}
