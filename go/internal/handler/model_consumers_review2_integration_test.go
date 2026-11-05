//go:build integration

// Review-2 gates of W01-5 (reports/review/W01-5-fremdreview-2.md): the
// escalation matrix 6 roles × 8 actions × {own, foreign, unknown, malformed}
// with byte-identical no-oracle answers, the field tiers on update and create,
// the peer-row tier, expiry and grace, reveal-once, the opaque cursor, the
// 10 000-row page, the redacted audit trail, the hot self-service flip and
// tenant-limit-set — plus which tenant answers carry max_model_consumers.

package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

type cr2Resp struct {
	code int
	body []byte
	hdr  http.Header
	m    map[string]any
}

func cr2Raw(t *testing.T, h *ManageHandler, ar *auth.AuthResult, payload map[string]any) cr2Resp {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	c := context.WithValue(req.Context(), authResultKey, ar)
	c = context.WithValue(c, requestIDKey, "r2")
	req = req.WithContext(c)
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return cr2Resp{rec.Code, rec.Body.Bytes(), rec.Header().Clone(), m}
}

func cr2Call(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action string, data any) cr2Resp {
	t.Helper()
	return cr2Raw(t, h, ar, map[string]any{"action": action, "data": data})
}

func cr2Hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:])[:10] }

func cr2Hdr(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v;", k, h[k])
	}
	return b.String()
}

func cr2Err(r cr2Resp) string { s, _ := r.m["error"].(string); return s }

type cr2Fix struct {
	t     *testing.T
	ctx   context.Context
	hOff  *ManageHandler
	hOn   *ManageHandler
	hDark *ManageHandler
	hSO   *ManageHandler
	admin *auth.AuthResult
	a, b  auTenant
	seq   int
}

func cr2Setup(t *testing.T) (*cr2Fix, func() *auth.AuthResult) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	f := &cr2Fix{t: t, ctx: ctx}
	f.hOff = mcHandler(t, pool, false, false)
	f.hOn = mcHandler(t, pool, true, true)
	f.hDark = mcHandler(t, pool, false, true)
	f.hSO = mcHandler(t, pool, true, false)
	f.admin, _ = kaServerAdmin(ctx, t, pool)
	f.a = auMakeTenant(ctx, t, pool, f.hOff, f.admin, "r2-a")
	f.b = auMakeTenant(ctx, t, pool, f.hOff, f.admin, "r2-b")
	return f, func() *auth.AuthResult { return mcMember(ctx, t, pool, f.hOff, f.a) }
}

func (f *cr2Fix) fresh(tenant string, extra map[string]any) mcConsumer {
	f.seq++
	data := map[string]any{"label": fmt.Sprintf("r2-%d", f.seq), "tenant_id": tenant}
	for k, v := range extra {
		data[k] = v
	}
	return mcCreate(f.t, f.hOff, f.admin, data)
}

// TestModelConsumerR2Matrix: 8 actions x roles x {own, foreign, unknown, malformed}.
func TestModelConsumerR2Matrix(t *testing.T) {
	f, mkMember := cr2Setup(t)
	member := mkMember()
	type role struct {
		name string
		h    *ManageHandler
		ar   *auth.AuthResult
	}
	roles := []role{
		{"member(on)", f.hOn, member},
		{"ta-off", f.hOff, f.a.ar},
		{"ta-enabled-noSS", f.hSO, f.a.ar},
		{"ta-on", f.hOn, f.a.ar},
		{"ta-dark(SS,!enabled)", f.hDark, f.a.ar},
		{"server-admin", f.hOff, f.admin},
	}
	actions := []string{"model-consumer-create", "model-consumer-list", "model-consumer-get", "model-key-history",
		"model-consumer-update", "model-consumer-delete", "model-key-rotate", "model-key-revoke"}
	targets := []string{"own", "foreign", "unknown", "malformed"}
	payload := func(action, target string) map[string]any {
		f.seq++
		lbl := fmt.Sprintf("r2-m-%d", f.seq)
		var c mcConsumer
		switch target {
		case "own":
			c = f.fresh(f.a.id, nil)
		case "foreign":
			c = f.fresh(f.b.id, nil)
		case "unknown":
			c = mcConsumer{id: uuid.NewString(), key: uuid.NewString()}
		case "malformed":
			c = mcConsumer{id: "not-a-uuid", key: "not-a-uuid"}
		}
		tenantFor := map[string]string{"own": "", "foreign": f.b.id, "unknown": uuid.NewString(), "malformed": "xyz"}[target]
		switch action {
		case "model-consumer-create":
			d := map[string]any{"label": lbl}
			if tenantFor != "" {
				d["tenant_id"] = tenantFor
			}
			return d
		case "model-consumer-list":
			d := map[string]any{}
			if tenantFor != "" {
				d["tenant_id"] = tenantFor
			}
			return d
		case "model-consumer-get", "model-consumer-delete":
			return map[string]any{"id": c.id}
		case "model-consumer-update":
			return map[string]any{"id": c.id, "description": "x"}
		case "model-key-history", "model-key-rotate":
			return map[string]any{"consumer_id": c.id}
		case "model-key-revoke":
			return map[string]any{"key_id": c.key}
		}
		return nil
	}
	fails := 0
	for _, ro := range roles {
		for _, act := range actions {
			res := map[string]cr2Resp{}
			line := fmt.Sprintf("MATRIX | %-22s | %-22s |", ro.name, act)
			for _, tg := range targets {
				r := cr2Call(t, ro.h, ro.ar, act, payload(act, tg))
				res[tg] = r
				line += fmt.Sprintf(" %s=%d/%s %q |", tg, r.code, cr2Hash(r.body), truncate(cr2Err(r), 40))
			}
			fu := bytes.Equal(res["foreign"].body, res["unknown"].body) && res["foreign"].code == res["unknown"].code && cr2Hdr(res["foreign"].hdr) == cr2Hdr(res["unknown"].hdr)
			mu := bytes.Equal(res["malformed"].body, res["unknown"].body) && res["malformed"].code == res["unknown"].code
			line += fmt.Sprintf(" foreign==unknown:%v malformed==unknown:%v", fu, mu)
			t.Log(line)
			if ro.name != "server-admin" && !fu && act != "model-consumer-create" && act != "model-consumer-list" {
				fails++
				t.Errorf("ORACLE %s %s: foreign %d %s vs unknown %d %s", ro.name, act, res["foreign"].code, res["foreign"].body, res["unknown"].code, res["unknown"].body)
			}
			if ro.name != "server-admin" && (act == "model-consumer-create" || act == "model-consumer-list") && !fu {
				t.Errorf("ORACLE tenant_id %s %s: %d %s vs %d %s", ro.name, act, res["foreign"].code, res["foreign"].body, res["unknown"].code, res["unknown"].body)
			}
			// Expectations.
			own := res["own"].code
			switch {
			case ro.name == "member(on)":
				if own != 403 || res["foreign"].code != 403 {
					t.Errorf("member %s own %d foreign %d, want 403", act, own, res["foreign"].code)
				}
			case ro.name == "server-admin":
				if own != 200 || res["foreign"].code != 200 || (res["unknown"].code != 404 && act != "model-consumer-list") {
					t.Errorf("server-admin %s own %d foreign %d unknown %d", act, own, res["foreign"].code, res["unknown"].code)
				}
			case act == "model-consumer-create" || act == "model-key-rotate":
				want := 403
				if ro.name == "ta-on" {
					want = 200
				}
				if own != want {
					t.Errorf("%s %s own %d want %d", ro.name, act, own, want)
				}
				if want == 403 && (res["foreign"].code != 403 || res["unknown"].code != 403 || cr2Err(res["unknown"]) != "admin key required") {
					t.Errorf("%s %s table-tier refusal not uniform: %d %d %s", ro.name, act, res["foreign"].code, res["unknown"].code, res["unknown"].body)
				}
			default:
				if own != 200 {
					t.Errorf("%s %s own %d want 200 (%s)", ro.name, act, own, res["own"].body)
				}
			}
		}
	}
	t.Logf("oracle failures: %d", fails)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// TestModelConsumerR2FieldTiers: each granting/revoking mutation, boundaries.
func TestModelConsumerR2FieldTiers(t *testing.T) {
	f, _ := cr2Setup(t)
	ta := f.a.ar
	type row struct {
		name    string
		h       *ManageHandler
		ar      *auth.AuthResult
		fixture map[string]any
		deact   bool
		patch   map[string]any
		want    int
		wantErr string
	}
	ar := "admin key required"
	sc := f.a.scope
	rows := []row{
		{"fw +1 noSS", f.hSO, ta, map[string]any{"fair_weight": 5}, false, map[string]any{"fair_weight": 6}, 403, ar},
		{"fw +1 SS", f.hOn, ta, map[string]any{"fair_weight": 5}, false, map[string]any{"fair_weight": 6}, 403, ar},
		{"fw same noSS", f.hSO, ta, map[string]any{"fair_weight": 5}, false, map[string]any{"fair_weight": 5}, 200, ""},
		{"fw -1 noSS", f.hSO, ta, map[string]any{"fair_weight": 5}, false, map[string]any{"fair_weight": 4}, 200, ""},
		{"fw 0", f.hOff, f.admin, nil, false, map[string]any{"fair_weight": 0}, 400, ""},
		{"fw 101", f.hOff, f.admin, nil, false, map[string]any{"fair_weight": 101}, 400, ""},
		{"fw 100 admin", f.hOff, f.admin, nil, false, map[string]any{"fair_weight": 100}, 200, ""},
		{"lane bulk->urgent noSS", f.hSO, ta, nil, false, map[string]any{"admission_lane": "urgent"}, 403, ar},
		{"lane bulk->urgent SS", f.hOn, ta, nil, false, map[string]any{"admission_lane": "urgent"}, 403, ar},
		{"lane urgent->bulk noSS", f.hSO, ta, map[string]any{"admission_lane": "urgent"}, false, map[string]any{"admission_lane": "bulk"}, 200, ""},
		{"lane urgent->urgent noSS", f.hSO, ta, map[string]any{"admission_lane": "urgent"}, false, map[string]any{"admission_lane": "urgent"}, 200, ""},
		{"lane interactive", f.hOff, f.admin, nil, false, map[string]any{"admission_lane": "interactive"}, 400, ""},
		{"cidr narrow noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"10.1.0.0/16"}}, 200, ""},
		{"cidr widen noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8", "192.168.0.0/16"}}, 403, ar},
		{"cidr widen SS", f.hOn, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8", "192.168.0.0/16"}}, 200, ""},
		{"cidr equal noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, 200, ""},
		{"cidr remove noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": nil}, 403, ar},
		{"cidr remove SS", f.hOn, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": nil}, 200, ""},
		{"cidr empty", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{}}, 400, ""},
		{"cidr any4 noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"0.0.0.0/0"}}, 403, ar},
		{"cidr /7 noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"10.0.0.0/7"}}, 403, ar},
		{"cidr hostbits", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"10.0.0.1/8"}}, 400, ""},
		{"cidr NULL->set noSS", f.hSO, ta, nil, false, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, 200, ""},
		{"cidr v6 narrow noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"fd00::/8"}}, false, map[string]any{"allowed_cidrs": []string{"fd00:1::/32"}}, 200, ""},
		{"cidr v4in6 noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allowed_cidrs": []string{"::ffff:10.0.0.0/104"}}, 403, ar},
		{"cidr v6 any under v6 noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"::/0"}}, false, map[string]any{"allowed_cidrs": []string{"::ffff:0.0.0.0/96"}}, 200, ""},
		{"scope set noSS", f.hSO, ta, nil, false, map[string]any{"tenant_scope": sc}, 403, ar},
		{"scope set SS", f.hOn, ta, nil, false, map[string]any{"tenant_scope": sc}, 200, ""},
		{"scope ->null noSS", f.hSO, ta, map[string]any{"tenant_scope": sc}, false, map[string]any{"tenant_scope": nil}, 200, ""},
		{"scope same noSS", f.hSO, ta, map[string]any{"tenant_scope": sc}, false, map[string]any{"tenant_scope": sc}, 200, ""},
		{"scope foreign SS", f.hOn, ta, nil, false, map[string]any{"tenant_scope": f.b.scope}, 403, "cannot create keys outside your tenant"},
		{"scope foreign noSS", f.hSO, ta, nil, false, map[string]any{"tenant_scope": f.b.scope}, 403, ar},
		{"scope _global", f.hOn, ta, nil, false, map[string]any{"tenant_scope": "_global"}, 400, ""},
		{"scope empty", f.hOn, ta, nil, false, map[string]any{"tenant_scope": ""}, 400, ""},
		{"scope foreign admin F1", f.hOff, f.admin, nil, false, map[string]any{"tenant_scope": f.b.scope}, 403, "cannot create keys outside your tenant"},
		{"sens lower SS", f.hOn, ta, nil, false, map[string]any{"prompt_sensitivity": "public"}, 403, ar},
		{"sens lower one step SS", f.hOn, ta, nil, false, map[string]any{"prompt_sensitivity": "personal"}, 403, ar},
		{"sens raise noSS", f.hSO, ta, map[string]any{"prompt_sensitivity": "public"}, false, map[string]any{"prompt_sensitivity": "credentials"}, 200, ""},
		{"sens same noSS", f.hSO, ta, nil, false, map[string]any{"prompt_sensitivity": "credentials"}, 200, ""},
		{"plaintext true SS", f.hOn, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}}, false, map[string]any{"allow_plaintext": true}, 403, ar},
		{"plaintext false noSS", f.hSO, ta, map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}, "allow_plaintext": true}, false, map[string]any{"allow_plaintext": false}, 200, ""},
		{"all_models true noSS", f.hSO, ta, nil, false, map[string]any{"all_models": true}, 403, ar},
		{"all_models true SS", f.hOn, ta, nil, false, map[string]any{"all_models": true}, 200, ""},
		{"all_models false noSS", f.hSO, ta, map[string]any{"all_models": true}, false, map[string]any{"all_models": false}, 200, ""},
		{"reactivate noSS", f.hSO, ta, nil, true, map[string]any{"active": true}, 403, ar},
		{"reactivate dark", f.hDark, ta, nil, true, map[string]any{"active": true}, 403, ar},
		{"reactivate SS", f.hOn, ta, nil, true, map[string]any{"active": true}, 200, ""},
		{"deactivate noSS", f.hSO, ta, nil, false, map[string]any{"active": false}, 200, ""},
		{"expires_at admin", f.hOff, f.admin, nil, false, map[string]any{"expires_at": time.Now().Add(time.Hour)}, 400, ""},
		{"expires_at ta", f.hOn, ta, nil, false, map[string]any{"expires_at": time.Now().Add(time.Hour)}, 400, ""},
		{"kind admin", f.hOff, f.admin, nil, false, map[string]any{"kind": "ctx-peer"}, 400, ""},
		{"tenant_id admin", f.hOff, f.admin, nil, false, map[string]any{"tenant_id": f.b.id}, 400, ""},
		{"peer_instance_id admin", f.hOff, f.admin, nil, false, map[string]any{"peer_instance_id": uuid.NewString()}, 400, ""},
		{"models admin", f.hOff, f.admin, nil, false, map[string]any{"models": []string{"chat"}}, 400, ""},
		{"mixed narrow+widen noSS", f.hSO, ta, map[string]any{"fair_weight": 5}, false, map[string]any{"fair_weight": 4, "all_models": true}, 403, ar},
		{"null fair_weight", f.hSO, ta, nil, false, map[string]any{"fair_weight": nil}, 400, ""},
	}
	for _, r := range rows {
		c := f.fresh(f.a.id, r.fixture)
		if r.deact {
			if x := cr2Call(t, f.hOff, f.admin, "model-consumer-delete", map[string]any{"id": c.id}); x.code != 200 {
				t.Fatalf("deact: %d %s", x.code, x.body)
			}
		}
		p := map[string]any{"id": c.id}
		for k, v := range r.patch {
			p[k] = v
		}
		got := cr2Call(t, r.h, r.ar, "model-consumer-update", p)
		ok := got.code == r.want && (r.wantErr == "" || cr2Err(got) == r.wantErr)
		t.Logf("FIELD | %-28s | got %d %q | want %d %q | ok=%v", r.name, got.code, truncate(cr2Err(got), 60), r.want, r.wantErr, ok)
		if !ok {
			t.Errorf("FIELD %s: got %d %s want %d %q", r.name, got.code, got.body, r.want, r.wantErr)
		}
	}
	// Reactivation against the cap.
	c := f.fresh(f.a.id, nil)
	_ = cr2Call(t, f.hOff, f.admin, "model-consumer-delete", map[string]any{"id": c.id})
	var active int
	_ = f.a.ar
	lim := cr2Raw(t, f.hOff, f.admin, map[string]any{"action": "tenant-limit-set", "id": f.a.id, "data": map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": 0}})
	t.Logf("CAP | tenant-limit-set 0: %d %s", lim.code, lim.body)
	got := cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": c.id, "active": true})
	t.Logf("CAP | reactivate SS at cap 0: %d %s (active=%d)", got.code, got.body, active)
	if got.code != 429 {
		t.Errorf("reactivation over cap: %d %s", got.code, got.body)
	}
	got = cr2Call(t, f.hOn, ta, "model-consumer-create", map[string]any{"label": "r2-over-cap"})
	t.Logf("CAP | create SS at cap 0: %d %s", got.code, got.body)
	_ = cr2Raw(t, f.hOff, f.admin, map[string]any{"action": "tenant-limit-set", "id": f.a.id, "data": map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": nil}})
}

// TestModelConsumerR2CreateTiers: create-time grants and F1.
func TestModelConsumerR2CreateTiers(t *testing.T) {
	f, _ := cr2Setup(t)
	ta := f.a.ar
	type row struct {
		name    string
		h       *ManageHandler
		ar      *auth.AuthResult
		data    map[string]any
		want    int
		wantErr string
	}
	ar := "admin key required"
	n := 0
	lbl := func() string { n++; return fmt.Sprintf("r2-c-%d", n) }
	rows := []row{
		{"ta SS plain", f.hOn, ta, map[string]any{}, 200, ""},
		{"ta SS kind peer", f.hOn, ta, map[string]any{"kind": "ctx-peer"}, 403, ar},
		{"ta SS kind bogus", f.hOn, ta, map[string]any{"kind": "x"}, 400, ""},
		{"ta SS sens public", f.hOn, ta, map[string]any{"prompt_sensitivity": "public"}, 403, ar},
		{"ta SS sens credentials", f.hOn, ta, map[string]any{"prompt_sensitivity": "credentials"}, 200, ""},
		{"ta SS fw 1", f.hOn, ta, map[string]any{"fair_weight": 1}, 200, ""},
		{"ta SS fw 2", f.hOn, ta, map[string]any{"fair_weight": 2}, 403, ar},
		{"ta SS lane urgent", f.hOn, ta, map[string]any{"admission_lane": "urgent"}, 403, ar},
		{"ta SS lane bulk", f.hOn, ta, map[string]any{"admission_lane": "bulk"}, 200, ""},
		{"ta SS plaintext", f.hOn, ta, map[string]any{"allow_plaintext": true, "allowed_cidrs": []string{"10.0.0.0/8"}}, 403, ar},
		{"ta SS all_models", f.hOn, ta, map[string]any{"all_models": true}, 200, ""},
		{"ta SS own scope", f.hOn, ta, map[string]any{"tenant_scope": f.a.scope}, 200, ""},
		{"ta SS foreign scope", f.hOn, ta, map[string]any{"tenant_scope": f.b.scope}, 403, "cannot create keys outside your tenant"},
		{"ta SS tenant_id own", f.hOn, ta, map[string]any{"tenant_id": f.a.id}, 403, "tenant_id is server-admin only"},
		{"ta SS models", f.hOn, ta, map[string]any{"models": []string{"x"}}, 400, ""},
		{"ta SS exp beyond", f.hOn, ta, map[string]any{"expires_at": time.Now().Add(366 * 24 * time.Hour)}, 400, ""},
		{"ta dark", f.hDark, ta, map[string]any{}, 403, ar},
		{"admin F1 no tenant foreign scope", f.hOff, f.admin, map[string]any{"tenant_scope": f.b.scope}, 403, "cannot create keys outside your tenant"},
		{"admin F1 tenant a scope b", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "tenant_scope": f.b.scope}, 403, "cannot create keys outside your tenant"},
		{"admin tenant a scope a", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "tenant_scope": f.a.scope}, 200, ""},
		{"admin malformed tenant", f.hOff, f.admin, map[string]any{"tenant_id": "zzz"}, 404, "tenant not found"},
		{"admin unknown tenant", f.hOff, f.admin, map[string]any{"tenant_id": uuid.NewString()}, 404, "tenant not found"},
		{"admin peer", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "kind": "ctx-peer"}, 200, ""},
		{"admin plaintext public cidr", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "allow_plaintext": true, "allowed_cidrs": []string{"8.8.8.0/24"}}, 400, ""},
		{"admin dup label", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "label": "r2-c-1"}, 409, ""},
		{"admin label 101", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "label": strings.Repeat("x", 101)}, 400, ""},
		{"admin desc 1001", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "description": strings.Repeat("x", 1001)}, 400, ""},
		{"admin case-variant Kind", f.hOff, f.admin, map[string]any{"tenant_id": f.a.id, "Kind": "ctx-peer"}, 200, ""},
		{"ta SS case-variant Kind", f.hOn, ta, map[string]any{"Kind": "ctx-peer"}, 403, ar},
		{"ta SS case-variant FAIR_WEIGHT", f.hOn, ta, map[string]any{"FAIR_WEIGHT": 9}, 403, ar},
	}
	for _, r := range rows {
		d := map[string]any{}
		for k, v := range r.data {
			d[k] = v
		}
		if _, ok := d["label"]; !ok {
			d["label"] = lbl()
		}
		got := cr2Call(t, r.h, r.ar, "model-consumer-create", d)
		ok := got.code == r.want && (r.wantErr == "" || cr2Err(got) == r.wantErr)
		t.Logf("CREATE | %-34s | got %d %q | want %d | ok=%v", r.name, got.code, truncate(cr2Err(got), 60), r.want, ok)
		if !ok {
			t.Errorf("CREATE %s: got %d %s want %d %q", r.name, got.code, got.body, r.want, r.wantErr)
		}
		if got.code != 200 {
			if _, has := got.m["api_key"]; has {
				t.Errorf("CREATE %s: failed answer carries api_key", r.name)
			}
		}
	}
}

// TestModelConsumerR2PeerRowTier: V-01-b.
func TestModelConsumerR2PeerRowTier(t *testing.T) {
	f, _ := cr2Setup(t)
	ta := f.a.ar
	ar := "admin key required"
	peer := func(extra map[string]any) mcConsumer {
		e := map[string]any{"kind": "ctx-peer"}
		for k, v := range extra {
			e[k] = v
		}
		return f.fresh(f.a.id, e)
	}
	check := func(name string, r cr2Resp, want int, wantErr string) {
		ok := r.code == want && (wantErr == "" || cr2Err(r) == wantErr)
		t.Logf("PEER | %-36s | got %d %q | want %d | ok=%v", name, r.code, truncate(cr2Err(r), 50), want, ok)
		if !ok {
			t.Errorf("PEER %s: got %d %s", name, r.code, r.body)
		}
	}
	p := peer(nil)
	check("rotate SS", cr2Call(t, f.hOn, ta, "model-key-rotate", map[string]any{"consumer_id": p.id}), 403, ar)
	check("update all_models SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": p.id, "all_models": true}), 403, ar)
	check("update scope SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": p.id, "tenant_scope": f.a.scope}), 403, ar)
	check("update label SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": p.id, "label": "r2-peer-relabel"}), 200, "")
	check("update label noSS", cr2Call(t, f.hSO, ta, "model-consumer-update", map[string]any{"id": p.id, "label": "r2-peer-relabel2"}), 200, "")
	pc := peer(map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}})
	check("update cidr widen SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": pc.id, "allowed_cidrs": []string{"0.0.0.0/0"}}), 403, ar)
	check("update cidr narrow SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": pc.id, "allowed_cidrs": []string{"10.1.0.0/16"}}), 200, "")
	pu := peer(map[string]any{"admission_lane": "urgent"})
	check("update lane urgent->bulk SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": pu.id, "admission_lane": "bulk"}), 200, "")
	pd := peer(nil)
	check("revoke key SS", cr2Call(t, f.hOn, ta, "model-key-revoke", map[string]any{"key_id": pd.key}), 200, "")
	check("delete SS", cr2Call(t, f.hOn, ta, "model-consumer-delete", map[string]any{"id": pd.id}), 200, "")
	check("reactivate SS", cr2Call(t, f.hOn, ta, "model-consumer-update", map[string]any{"id": pd.id, "active": true}), 403, ar)
	check("reactivate admin", cr2Call(t, f.hOff, f.admin, "model-consumer-update", map[string]any{"id": pd.id, "active": true}), 200, "")
	check("rotate admin", cr2Call(t, f.hOff, f.admin, "model-key-rotate", map[string]any{"consumer_id": p.id}), 200, "")
}

// TestModelConsumerR2Expiry: expiry per kind, grace, V-01-d.
func TestModelConsumerR2Expiry(t *testing.T) {
	f, _ := cr2Setup(t)
	adm := f.admin
	now := time.Now()
	n := 0
	lbl := func() string { n++; return fmt.Sprintf("r2-e-%d", n) }
	keyExp := func(r cr2Resp) any {
		c, _ := r.m["consumer"].(map[string]any)
		ks, _ := c["keys"].([]any)
		if len(ks) == 0 {
			if k, ok := r.m["key"].(map[string]any); ok {
				return k["expires_at"]
			}
			return "<nokey>"
		}
		k, _ := ks[0].(map[string]any)
		return k["expires_at"]
	}
	log := func(name string, r cr2Resp, want int) {
		ok := r.code == want
		t.Logf("EXPIRY | %-34s | got %d %q exp=%v | want %d | ok=%v", name, r.code, truncate(cr2Err(r), 70), keyExp(r), want, ok)
		if !ok {
			t.Errorf("EXPIRY %s: %d %s", name, r.code, r.body)
		}
	}
	log("app past", cr2Call(t, f.hOff, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "expires_at": now.Add(-time.Hour)}), 400)
	log("app now+366d", cr2Call(t, f.hOff, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "expires_at": now.Add(366 * 24 * time.Hour)}), 400)
	log("app now+364d", cr2Call(t, f.hOff, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "expires_at": now.Add(364 * 24 * time.Hour)}), 200)
	r := cr2Call(t, f.hOff, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id})
	log("app none", r, 200)
	if s, _ := keyExp(r).(string); s != "" {
		at, _ := time.Parse(time.RFC3339Nano, s)
		d := at.Sub(now.Add(365 * 24 * time.Hour))
		t.Logf("EXPIRY | app none delta to now+365d: %v", d)
		if d < -time.Minute || d > time.Minute {
			t.Errorf("app none expiry %v off", at)
		}
	} else {
		t.Errorf("app none: no expiry %v", keyExp(r))
	}
	appC, _ := r.m["consumer"].(map[string]any)
	appID, _ := appC["id"].(string)
	rp := cr2Call(t, f.hOff, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "kind": "ctx-peer"})
	log("peer none (setting 0)", rp, 200)
	if keyExp(rp) != nil {
		t.Errorf("peer none: expiry %v, want null", keyExp(rp))
	}
	log("peer now+10d (setting 0)", cr2Call(t, f.hOff, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "kind": "ctx-peer", "expires_at": now.Add(240 * time.Hour)}), 200)
	h0 := mcHandlerOv(t, f.hOff.pool, map[string]string{"gateway.key_max_lifetime_app": "0"})
	r0 := cr2Call(t, h0, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id})
	log("app none, lifetime_app 0", r0, 200)
	hp := mcHandlerOv(t, f.hOff.pool, map[string]string{"gateway.key_max_lifetime_peer": "3600"})
	log("peer none, lifetime_peer 3600", cr2Call(t, hp, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "kind": "ctx-peer"}), 200)
	log("peer +2h, lifetime_peer 3600", cr2Call(t, hp, adm, "model-consumer-create", map[string]any{"label": lbl(), "tenant_id": f.a.id, "kind": "ctx-peer", "expires_at": now.Add(2 * time.Hour)}), 400)
	// Rotation.
	log("rotate none", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID}), 200)
	log("rotate past", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "expires_at": now.Add(-time.Minute)}), 400)
	log("rotate beyond", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "expires_at": now.Add(400 * 24 * time.Hour)}), 400)
	log("rotate grace -1", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "grace": -1}), 400)
	log("rotate grace 604801", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "grace": 604801}), 400)
	log("rotate grace 1.5", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "grace": 1.5}), 400)
	log("rotate grace \"10\"", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "grace": "10"}), 400)
	log("rotate grace 604800", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "grace": 604800}), 200)
	log("rotate grace 0", cr2Call(t, f.hOff, adm, "model-key-rotate", map[string]any{"consumer_id": appID, "grace": 0}), 200)
	g := cr2Call(t, f.hOff, adm, "model-consumer-get", map[string]any{"id": appID})
	c, _ := g.m["consumer"].(map[string]any)
	ks, _ := c["keys"].([]any)
	var sts []string
	for _, k := range ks {
		km, _ := k.(map[string]any)
		sts = append(sts, fmt.Sprint(km["status"]))
	}
	t.Logf("EXPIRY | chain after grace 0: status=%v consumer=%v keys_total=%v", sts, c["status"], c["keys_total"])
	live := 0
	for _, s := range sts {
		if s == "active" || s == "expiring" || s == "superseded" {
			live++
		}
	}
	if live != 1 {
		t.Errorf("after grace 0: %d live keys, want 1 (%v)", live, sts)
	}
}

// cr2LogBuf captures slog output.
type cr2LogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *cr2LogBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }

// TestModelConsumerR2RevealOnce: plaintext once; nowhere else; no hash anywhere.
func TestModelConsumerR2RevealOnce(t *testing.T) {
	f, _ := cr2Setup(t)
	buf := &cr2LogBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	cr := cr2Call(t, f.hOn, f.a.ar, "model-consumer-create", map[string]any{"label": "r2-rev"})
	p1, _ := cr.m["api_key"].(string)
	if cr.code != 200 || p1 == "" || bytes.Count(cr.body, []byte(p1)) != 1 {
		t.Fatalf("create: %d count=%d", cr.code, bytes.Count(cr.body, []byte(p1)))
	}
	c, _ := cr.m["consumer"].(map[string]any)
	id, _ := c["id"].(string)
	ro := cr2Call(t, f.hOn, f.a.ar, "model-key-rotate", map[string]any{"consumer_id": id})
	p2, _ := ro.m["api_key"].(string)
	if ro.code != 200 || p2 == "" || bytes.Count(ro.body, []byte(p2)) != 1 || bytes.Contains(ro.body, []byte(p1)) {
		t.Fatalf("rotate: %d", ro.code)
	}
	// failing paths
	dup := cr2Call(t, f.hOn, f.a.ar, "model-consumer-create", map[string]any{"label": "r2-rev"})
	bad := cr2Call(t, f.hOn, f.a.ar, "model-key-rotate", map[string]any{"consumer_id": id, "grace": 9999999})
	_ = cr2Call(t, f.hOff, f.admin, "model-consumer-delete", map[string]any{"id": id})
	inact := cr2Call(t, f.hOff, f.admin, "model-key-rotate", map[string]any{"consumer_id": id})
	t.Logf("REVEAL | dup %d %s | bad grace %d | rotate inactive %d %s", dup.code, dup.body, bad.code, inact.code, inact.body)
	for _, r := range []cr2Resp{dup, bad, inact} {
		if _, has := r.m["api_key"]; has {
			t.Errorf("failed path carries api_key: %s", r.body)
		}
	}
	var hashes []string
	rows, err := f.hOff.pool.Query(f.ctx, `SELECT key_hash FROM context_model_keys`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		hashes = append(hashes, h)
	}
	rows.Close()
	type rd struct {
		name string
		r    cr2Resp
	}
	var reads []rd
	for _, who := range []struct {
		n  string
		h  *ManageHandler
		ar *auth.AuthResult
	}{{"ta", f.hOn, f.a.ar}, {"admin", f.hOff, f.admin}} {
		reads = append(reads,
			rd{who.n + " list", cr2Call(t, who.h, who.ar, "model-consumer-list", map[string]any{"status": []string{"all"}})},
			rd{who.n + " get", cr2Call(t, who.h, who.ar, "model-consumer-get", map[string]any{"id": id})},
			rd{who.n + " history", cr2Call(t, who.h, who.ar, "model-key-history", map[string]any{"consumer_id": id})})
	}
	ah := auditHandler(f.hOff.pool)
	for _, e := range []string{"model_key", "model_consumer"} {
		for _, who := range []*auth.AuthResult{f.a.ar, f.admin} {
			code, body, _ := serveAudit(t, ah, who, "?entity="+e)
			reads = append(reads, rd{"audit " + e, cr2Resp{code: code, body: body}})
		}
	}
	for _, x := range reads {
		leak := bytes.Contains(x.r.body, []byte(p1)) || bytes.Contains(x.r.body, []byte(p2)) || bytes.Contains(x.r.body, []byte(`"api_key"`)) || bytes.Contains(x.r.body, []byte("key_hash"))
		for _, h := range hashes {
			if bytes.Contains(x.r.body, []byte(h)) {
				leak = true
			}
		}
		t.Logf("REVEAL | %-22s | %d | %d bytes | leak=%v", x.name, x.r.code, len(x.r.body), leak)
		if leak || x.r.code != 200 {
			t.Errorf("REVEAL %s: code %d leak %v", x.name, x.r.code, leak)
		}
	}
	logs := buf.b.String()
	re := regexp.MustCompile(`ctxm_[A-Za-z0-9]{20,}`)
	lk := strings.Contains(logs, p1) || strings.Contains(logs, p2) || re.MatchString(logs)
	for _, h := range hashes {
		if strings.Contains(logs, h) {
			lk = true
		}
	}
	t.Logf("REVEAL | slog %d bytes, leak=%v, hashes=%d", len(logs), lk, len(hashes))
	if lk {
		t.Errorf("slog leak")
	}
}

// TestModelConsumerR2Cursor: malformed / foreign cursors, limits.
func TestModelConsumerR2Cursor(t *testing.T) {
	f, _ := cr2Setup(t)
	for i := 0; i < 3; i++ {
		f.fresh(f.a.id, nil)
	}
	cb := f.fresh(f.b.id, nil)
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	rows := []struct {
		name string
		act  string
		data map[string]any
		want int
	}{
		{"list cursor !!!", "model-consumer-list", map[string]any{"cursor": "!!!"}, 400},
		{"list cursor x|y", "model-consumer-list", map[string]any{"cursor": b64("x|y")}, 400},
		{"list cursor ts|notuuid", "model-consumer-list", map[string]any{"cursor": b64(time.Now().UTC().Format(time.RFC3339Nano) + "|nope")}, 400},
		{"list cursor nosep", "model-consumer-list", map[string]any{"cursor": b64("abc")}, 400},
		{"list cursor std-b64 padded", "model-consumer-list", map[string]any{"cursor": base64.StdEncoding.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano) + "|" + uuid.NewString()))}, 0},
		{"list cursor far future valid", "model-consumer-list", map[string]any{"cursor": b64("9999-01-01T00:00:00Z|" + uuid.NewString())}, 200},
		{"list limit 0", "model-consumer-list", map[string]any{"limit": 0}, 400},
		{"list limit -1", "model-consumer-list", map[string]any{"limit": -1}, 400},
		{"list limit 1e9", "model-consumer-list", map[string]any{"limit": 1000000000}, 200},
		{"list status bogus", "model-consumer-list", map[string]any{"status": []string{"bogus"}}, 400},
		{"list q 101", "model-consumer-list", map[string]any{"q": strings.Repeat("x", 101)}, 400},
		{"list q %", "model-consumer-list", map[string]any{"q": "%"}, 200},
		{"history cursor !!!", "model-key-history", map[string]any{"consumer_id": cb.id, "cursor": "!!!"}, 400},
		{"history limit 0", "model-key-history", map[string]any{"consumer_id": cb.id, "limit": 0}, 400},
	}
	for _, r := range rows {
		got := cr2Call(t, f.hOn, f.a.ar, r.act, r.data)
		n := -1
		if cs, ok := got.m["consumers"].([]any); ok {
			n = len(cs)
		}
		t.Logf("CURSOR | %-30s | got %d %q n=%d next=%v | want %d", r.name, got.code, truncate(cr2Err(got), 50), n, got.m["next_cursor"], r.want)
		if r.want != 0 && got.code != r.want {
			t.Errorf("CURSOR %s: %d %s", r.name, got.code, got.body)
		}
	}
	// paging: limit 1 walk, foreign cursor
	seen := map[string]bool{}
	cur := ""
	pages := 0
	for {
		d := map[string]any{"limit": 1}
		if cur != "" {
			d["cursor"] = cur
		}
		got := cr2Call(t, f.hOn, f.a.ar, "model-consumer-list", d)
		cs, _ := got.m["consumers"].([]any)
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			seen[fmt.Sprint(cm["id"])] = true
			if cm["tenant_id"] != f.a.id {
				t.Errorf("foreign consumer in list: %v", cm)
			}
		}
		pages++
		nx, ok := got.m["next_cursor"].(string)
		if !ok || pages > 10 {
			break
		}
		cur = nx
	}
	t.Logf("CURSOR | limit-1 walk: %d pages, %d distinct", pages, len(seen))
	if len(seen) != 3 {
		t.Errorf("walk saw %d, want 3", len(seen))
	}
	// foreign tenant's cursor: B's consumer position
	var at time.Time
	_ = f.hOff.pool.QueryRow(f.ctx, `SELECT created_at FROM context_model_consumers WHERE id = $1::uuid`, cb.id).Scan(&at)
	fc := encodeKeysetCursor(at, cb.id)
	got := cr2Call(t, f.hOn, f.a.ar, "model-consumer-list", map[string]any{"cursor": fc})
	cs, _ := got.m["consumers"].([]any)
	t.Logf("CURSOR | foreign cursor: %d n=%d", got.code, len(cs))
}

// TestModelConsumerR2BigList: 10 000 inactive + 5 active.
func TestModelConsumerR2BigList(t *testing.T) {
	f, _ := cr2Setup(t)
	if _, err := f.hOff.pool.Exec(f.ctx, `INSERT INTO context_model_consumers (tenant_id, label, active) SELECT $1::uuid, 'r2-in-'||g, false FROM generate_series(1,10000) g`, f.a.id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		f.fresh(f.a.id, nil)
	}
	t0 := time.Now()
	got := cr2Call(t, f.hOn, f.a.ar, "model-consumer-list", map[string]any{"limit": 5})
	el := time.Since(t0)
	cs, _ := got.m["consumers"].([]any)
	t.Logf("BIG | limit 5: %d n=%d next=%v in %v", got.code, len(cs), got.m["next_cursor"], el)
	if got.code != 200 || len(cs) != 5 || got.m["next_cursor"] != nil {
		t.Errorf("big list: %d n=%d next=%v", got.code, len(cs), got.m["next_cursor"])
	}
	got = cr2Call(t, f.hOn, f.a.ar, "model-consumer-list", map[string]any{})
	cs, _ = got.m["consumers"].([]any)
	t.Logf("BIG | default: %d n=%d next=%v", got.code, len(cs), got.m["next_cursor"])
	got = cr2Call(t, f.hOn, f.a.ar, "model-consumer-list", map[string]any{"status": []string{"no_key", "active"}, "limit": 5})
	cs, _ = got.m["consumers"].([]any)
	t.Logf("BIG | status no_key,active limit 5: %d n=%d next=%v", got.code, len(cs), got.m["next_cursor"])
	got = cr2Call(t, f.hOn, f.a.ar, "model-consumer-list", map[string]any{"status": []string{"inactive"}, "limit": 3})
	cs, _ = got.m["consumers"].([]any)
	t.Logf("BIG | inactive limit 3: %d n=%d next!=nil:%v", got.code, len(cs), got.m["next_cursor"] != nil)
}

// TestModelConsumerR2AuditTrail: redaction of the trail; peer binding invisible.
func TestModelConsumerR2AuditTrail(t *testing.T) {
	f, _ := cr2Setup(t)
	ta := f.a.ar
	x := f.fresh(f.a.id, nil)
	_ = cr2Call(t, f.hOff, f.admin, "model-key-rotate", map[string]any{"consumer_id": x.id})
	_ = cr2Call(t, f.hOff, f.admin, "model-key-revoke", map[string]any{"key_id": x.key})
	g := cr2Call(t, f.hOn, ta, "model-consumer-get", map[string]any{"id": x.id})
	trail, _ := g.m["audit"].([]any)
	t.Logf("TRAIL | ta get admin-made: %d rows", len(trail))
	for _, r := range trail {
		rm, _ := r.(map[string]any)
		md, _ := rm["metadata"].(map[string]any)
		hasActorMeta := false
		for k := range md {
			if strings.HasPrefix(k, "actor_") {
				hasActorMeta = true
			}
		}
		t.Logf("TRAIL |   %v %v actor_label=%v api_key_id=%v actor_meta=%v", rm["entity_type"], rm["action"], rm["actor_label"], rm["api_key_id"], hasActorMeta)
		if rm["actor_label"] != "server-admin" || rm["api_key_id"] != nil || hasActorMeta {
			t.Errorf("trail row not redacted: %v", rm)
		}
	}
	if len(trail) < 4 {
		t.Errorf("trail short: %d", len(trail))
	}
	// Own actor keeps label.
	y := cr2Call(t, f.hOn, ta, "model-consumer-create", map[string]any{"label": "r2-own"})
	yc, _ := y.m["consumer"].(map[string]any)
	yid, _ := yc["id"].(string)
	gy := cr2Call(t, f.hOn, ta, "model-consumer-get", map[string]any{"id": yid})
	ty, _ := gy.m["audit"].([]any)
	for _, r := range ty {
		rm, _ := r.(map[string]any)
		t.Logf("TRAIL | own: %v %v actor_label=%v", rm["entity_type"], rm["action"], rm["actor_label"])
	}
	cv, _ := gy.m["consumer"].(map[string]any)
	t.Logf("TRAIL | own created_by_label=%v; admin-made created_by_label=%v", cv["created_by_label"], func() any { c, _ := g.m["consumer"].(map[string]any); return c["created_by_label"] }())
	// Peer binding.
	p := f.fresh(f.a.id, map[string]any{"kind": "ctx-peer"})
	inst := uuid.NewString()
	if _, err := f.hOff.pool.Exec(f.ctx, `UPDATE context_model_consumers SET peer_instance_id = $2::uuid, peer_bound_at = now() WHERE id = $1::uuid`, p.id, inst); err != nil {
		t.Fatal(err)
	}
	ah := auditHandler(f.hOff.pool)
	for _, who := range []struct {
		n  string
		h  *ManageHandler
		ar *auth.AuthResult
	}{{"ta", f.hOn, ta}, {"admin", f.hOff, f.admin}} {
		gp := cr2Call(t, who.h, who.ar, "model-consumer-get", map[string]any{"id": p.id})
		lp := cr2Call(t, who.h, who.ar, "model-consumer-list", map[string]any{})
		_, ab, ar := serveAudit(t, ah, who.ar, "?entity=model_consumer&id="+p.id)
		pc, _ := gp.m["consumer"].(map[string]any)
		tr, _ := gp.m["audit"].([]any)
		updates := 0
		for _, r := range tr {
			if rm, _ := r.(map[string]any); rm["action"] == "update" {
				updates++
			}
		}
		aupd := 0
		for _, r := range ar.Rows {
			if r.Action == "update" {
				aupd++
			}
		}
		t.Logf("PEERBIND | %-5s | view.peer=%v | get has inst:%v peer_instance_id:%v | list has inst:%v | audit rows=%d updates=%d has inst:%v field:%v | trail updates=%d",
			who.n, pc["peer"], bytes.Contains(gp.body, []byte(inst)), bytes.Contains(gp.body, []byte("peer_instance_id")),
			bytes.Contains(lp.body, []byte(inst)), len(ar.Rows), aupd, bytes.Contains(ab, []byte(inst)), bytes.Contains(ab, []byte("peer_instance_id")), updates)
		if who.n == "ta" && (pc["peer"] != nil || bytes.Contains(gp.body, []byte(inst)) || bytes.Contains(lp.body, []byte(inst)) || bytes.Contains(ab, []byte(inst)) || bytes.Contains(ab, []byte("peer_bound_at")) || updates != 0 || aupd != 0) {
			t.Errorf("peer binding visible to tenant-admin")
		}
		if who.n == "admin" && (pc["peer"] == nil || !bytes.Contains(ab, []byte(inst)) || updates != 1) {
			t.Errorf("positive control failed for admin")
		}
	}
}

// TestModelConsumerR2HotFlip: gateway.tenant_self_service flips at runtime.
func TestModelConsumerR2HotFlip(t *testing.T) {
	f, _ := cr2Setup(t)
	t.Setenv("CONTEXT_DB_PASSWORD", "mc-test")
	build := func(en, ss string) *config.Config {
		c, _ := config.Build([]config.Override{{Key: "gateway.enabled", Value: en}, {Key: "gateway.tenant_self_service", Value: ss}}, nil)
		return c
	}
	st := config.NewStore(build("false", "false"))
	h := NewManageHandler(f.hOff.pool, st, nil, nil, nil, nil, nil, nil)
	r1 := cr2Call(t, h, f.a.ar, "model-consumer-create", map[string]any{"label": "r2-hot-1"})
	if err := st.Replace(build("true", "true")); err != nil {
		t.Fatal(err)
	}
	r2 := cr2Call(t, h, f.a.ar, "model-consumer-create", map[string]any{"label": "r2-hot-2"})
	if err := st.Replace(build("false", "true")); err != nil {
		t.Fatal(err)
	}
	r3 := cr2Call(t, h, f.a.ar, "model-consumer-create", map[string]any{"label": "r2-hot-3"})
	t.Logf("HOT | off %d %q | on %d | dark %d %q", r1.code, cr2Err(r1), r2.code, r3.code, cr2Err(r3))
	if r1.code != 403 || r2.code != 200 || r3.code != 403 {
		t.Errorf("hot flip: %d %d %d", r1.code, r2.code, r3.code)
	}
	nilH := NewManageHandler(f.hOff.pool, nil, nil, nil, nil, nil, nil, nil)
	r4 := cr2Call(t, nilH, f.a.ar, "model-consumer-create", map[string]any{"label": "r2-hot-4"})
	t.Logf("HOT | nil cfg store: %d %q", r4.code, cr2Err(r4))
	if r4.code != 403 {
		t.Errorf("nil cfg: %d", r4.code)
	}
}

// TestModelConsumerR2TenantLimit: echo of max_model_consumers.
func TestModelConsumerR2TenantLimit(t *testing.T) {
	f, _ := cr2Setup(t)
	set := func(name string, data map[string]any, ar *auth.AuthResult) cr2Resp {
		r := cr2Raw(t, f.hOff, ar, map[string]any{"action": "tenant-limit-set", "id": f.a.id, "data": data})
		tn, _ := r.m["tenant"].(map[string]any)
		v, present := tn["max_model_consumers"]
		var col *int
		_ = f.hOff.pool.QueryRow(f.ctx, `SELECT max_model_consumers FROM context_tenants WHERE id = $1::uuid`, f.a.id).Scan(&col)
		cs := "NULL"
		if col != nil {
			cs = fmt.Sprint(*col)
		}
		t.Logf("LIMIT | %-24s | %d %q | echo present=%v value=%v | column=%s", name, r.code, truncate(cr2Err(r), 60), present, v, cs)
		return r
	}
	set("start absent", map[string]any{"max_scopes": nil, "max_keys": nil}, f.admin)
	set("set 3", map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": 3}, f.admin)
	set("absent keeps", map[string]any{"max_scopes": 5, "max_keys": 5}, f.admin)
	set("null", map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": nil}, f.admin)
	set("-1", map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": -1}, f.admin)
	set("1000001", map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": 1000001}, f.admin)
	set("string", map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": "3"}, f.admin)
	set("ta", map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": 99}, f.a.ar)
	set("only consumers", map[string]any{"max_model_consumers": 7}, f.admin)
}

// TestModelConsumerR2TenantGetCap: which tenant answers carry max_model_consumers.
func TestModelConsumerR2TenantGetCap(t *testing.T) {
	f, _ := cr2Setup(t)
	for _, act := range []string{"tenant-get", "tenant-list"} {
		r := cr2Raw(t, f.hOff, f.admin, map[string]any{"action": act, "id": f.a.id})
		t.Logf("TENANTCAP | %s: %d has max_model_consumers=%v | %s", act, r.code, bytes.Contains(r.body, []byte("max_model_consumers")), truncate(string(r.body), 240))
	}
	r := cr2Raw(t, f.hOff, f.admin, map[string]any{"action": "tenant-create", "data": map[string]any{"slug": "r2-c", "display_name": "C"}})
	t.Logf("TENANTCAP | tenant-create: %d has=%v", r.code, bytes.Contains(r.body, []byte("max_model_consumers")))
	r = cr2Raw(t, f.hOff, f.admin, map[string]any{"action": "tenant-limit-set", "id": f.a.id, "data": map[string]any{"max_scopes": nil, "max_keys": nil}})
	t.Logf("TENANTCAP | tenant-limit-set (cap absent, column 64): %d has=%v", r.code, bytes.Contains(r.body, []byte("max_model_consumers")))
}
