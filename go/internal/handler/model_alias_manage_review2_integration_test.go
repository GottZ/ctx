//go:build integration

// Review-2 gates of W02-04 (reports/review/W02-04-fremdreview-2.md): the
// escalation matrix with byte-identical no-oracle bodies, confirm flag only on
// an actual lowering (R-2), the floor counts disabled rows (R-3), update takes
// the name lock and re-reads after it (R-1), and the floor under a write storm.
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

func r2Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

// P1 — escalation matrix: 4 actions × 4 callers × 4 targets, status + body hash.
func TestModelAliasR2_EscalationMatrix(t *testing.T) {
	m := setupModelAliasHarness(t)
	const missing = "01900000-0000-7000-8000-00000000abcd"
	type caller struct {
		name string
		ar   *auth.AuthResult
	}
	callers := []caller{{"member-A", m.member}, {"tadmin-A", m.ten}, {"tadmin-B", m.tenB2}, {"server-admin", m.admin}}
	targets := []string{"ma-a", "ma-b", "_global", "missing"}
	n := 0
	seed := func(target string) string {
		n++
		if target == "missing" {
			return missing
		}
		return m.seed(target, fmt.Sprintf("p%d", n), "chat", "chat", "credentials", "{}")
	}
	bodies := map[string]string{}
	var lines []string
	for _, c := range callers {
		for _, action := range []string{"model-alias-update", "model-alias-delete"} {
			for _, tg := range targets {
				id := seed(tg)
				var data map[string]any
				if action == "model-alias-update" {
					data = map[string]any{"description": "probe"}
				}
				code, _, body := m.do(c.ar, action, id, data)
				key := c.name + "|" + action + "|" + tg
				bodies[key] = body
				show := body
				if code == 200 {
					show = "(200 row)"
				}
				lines = append(lines, fmt.Sprintf("%s | %d | %s | %s", key, code, r2Hash(body), show))
			}
		}
		for _, tg := range []string{"ma-a", "ma-b", "_global", "nowhere"} {
			n++
			d := chatSpec(fmt.Sprintf("c%d", n), map[string]any{"scope": tg})
			if c.ar == m.admin && tg != "_global" {
				d["tenant_id"] = m.tenA
			}
			code, _, body := m.do(c.ar, "model-alias-create", "", d)
			key := c.name + "|model-alias-create|" + tg
			bodies[key] = body
			show := body
			if code == 200 {
				show = "(200 row)"
			}
			lines = append(lines, fmt.Sprintf("%s | %d | %s | %s", key, code, r2Hash(body), show))
		}
		code, out, body := m.do(c.ar, "model-alias-list", "", nil)
		var seen []string
		if items, ok := out["model_aliases"].([]any); ok {
			scopes := map[string]int{}
			for _, it := range items {
				a := it.(map[string]any)
				w := "r"
				if a["writable"] == true {
					w = "w"
				}
				scopes[a["scope"].(string)+"="+w]++
			}
			for k, v := range scopes {
				seen = append(seen, fmt.Sprintf("%s×%d", k, v))
			}
			sort.Strings(seen)
		}
		lines = append(lines, fmt.Sprintf("%s|model-alias-list|- | %d | %s | %s", c.name, code, r2Hash(body), strings.Join(seen, ",")))
	}
	for _, l := range lines {
		t.Log(l)
	}
	// No-oracle assertions.
	same := func(a, b string) {
		if bodies[a] != bodies[b] {
			t.Errorf("ORACLE: %s = %s ≠ %s = %s", a, bodies[a], b, bodies[b])
		}
	}
	for _, act := range []string{"model-alias-update", "model-alias-delete"} {
		same("tadmin-A|"+act+"|ma-b", "tadmin-A|"+act+"|missing")
		same("tadmin-A|"+act+"|_global", "tadmin-A|"+act+"|missing")
		same("tadmin-B|"+act+"|ma-a", "tadmin-B|"+act+"|missing")
		same("tadmin-B|"+act+"|_global", "tadmin-B|"+act+"|missing")
		same("server-admin|"+act+"|missing", "tadmin-A|"+act+"|missing")
	}
	same("tadmin-A|model-alias-create|ma-b", "tadmin-A|model-alias-create|nowhere")
	same("tadmin-A|model-alias-create|_global", "tadmin-A|model-alias-create|nowhere")
	same("tadmin-B|model-alias-create|ma-a", "tadmin-B|model-alias-create|nowhere")
	same("tadmin-B|model-alias-create|_global", "tadmin-B|model-alias-create|nowhere")
	same("tadmin-A|model-alias-update|missing", "tadmin-A|model-alias-create|nowhere")
}

// P2 — no oracle across payload shapes: foreign / _global / missing answer the
// same bytes for every update payload, whatever the payload would do on an own row.
func TestModelAliasR2_NoOraclePayloads(t *testing.T) {
	m := setupModelAliasHarness(t)
	own := m.seed("ma-a", "own", "chat", "chat", "credentials", "{}")
	foreign := m.seed("ma-b", "fx", "chat", "chat", "credentials", "{}")
	global := m.seed("_global", "gx", "chat", "chat", "credentials", "{}")
	const missing = "01900000-0000-7000-8000-00000000beef"
	payloads := []string{
		`{"sensitivity":"public"}`,
		`{"sensitivity":"public","confirm_sensitivity_downgrade":true}`,
		`{"sensitivity":"bogus"}`,
		`{"policy":{"max_n":99}}`,
		`{"policy":{"zz":1}}`,
		`{"role":"BAD ROLE"}`,
		`{"role":"emb-mixed"}`,
		`{"enabled":false}`,
		`{"description":"x\u0000y"}`,
		`{"enabled":null}`,
		`{"name":"gx"}`,
		`{"scope":"ma-a","enabled":true}`,
		`{}`,
		`{"Enabled":true}`,
	}
	for _, p := range payloads {
		var got []string
		for _, id := range []string{foreign, global, missing, "nope"} {
			code, _, body := m.doRaw(m.ten, `{"action":"model-alias-update","id":"`+id+`","data":`+p+`}`)
			got = append(got, fmt.Sprintf("%d %s", code, body))
		}
		code, _, ownBody := m.doRaw(m.ten, `{"action":"model-alias-update","id":"`+own+`","data":`+p+`}`)
		t.Logf("payload %-62s → foreign/global/missing/malformed: %s | own: %d %s", p, got[0], code, ownBody)
		for i := 1; i < len(got); i++ {
			if got[i] != got[0] {
				t.Errorf("ORACLE payload %s: %q vs %q", p, got[0], got[i])
			}
		}
	}
	// delete too
	var got []string
	for _, id := range []string{foreign, global, missing, "nope", ""} {
		code, _, body := m.doRaw(m.ten, `{"action":"model-alias-delete","id":"`+id+`"}`)
		got = append(got, fmt.Sprintf("%d %s", code, body))
	}
	t.Logf("delete foreign/global/missing/malformed/empty: %v", got)
}

// P3 — confirm without an actual lowering never reaches the audit row.
func TestModelAliasR2_ConfirmWithoutLowering(t *testing.T) {
	m := setupModelAliasHarness(t)
	flag := func(id string) []any {
		var out []any
		for _, md := range maAuditMeta(t, m, id) {
			out = append(out, md["confirm_sensitivity_downgrade"])
		}
		return out
	}
	code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("cf1", map[string]any{"confirm_sensitivity_downgrade": true}))
	if code != 200 {
		t.Fatalf("create cf1 = %d %s", code, body)
	}
	cf1 := m.createdID(out)
	t.Logf("create credentials + confirm: %s; audit flags %v", body, flag(cf1))
	code, out, body = m.do(m.admin, "model-alias-create", "", chatSpec("cf2", map[string]any{"sensitivity": "public", "confirm_sensitivity_downgrade": true}))
	if code != 200 {
		t.Fatalf("create cf2 = %d %s", code, body)
	}
	cf2 := m.createdID(out)
	_, _, b2 := m.do(m.admin, "model-alias-update", cf2, map[string]any{"sensitivity": "credentials", "confirm_sensitivity_downgrade": true})
	_, _, b3 := m.do(m.admin, "model-alias-update", cf2, map[string]any{"description": "d", "confirm_sensitivity_downgrade": true})
	_, _, b4 := m.do(m.admin, "model-alias-update", cf2, map[string]any{"sensitivity": "credentials", "confirm_sensitivity_downgrade": true})
	t.Logf("raise+confirm: %s | desc+confirm: %s | same+confirm: %s; audit flags of cf2 %v", b2, b3, b4, flag(cf2))
	code, out, body = m.do(m.ten, "model-alias-create", "", chatSpec("tcf", map[string]any{"confirm_sensitivity_downgrade": true}))
	if code != 200 {
		t.Fatalf("tenant create tcf = %d %s", code, body)
	}
	tcf := m.createdID(out)
	_, _, b5 := m.do(m.ten, "model-alias-update", tcf, map[string]any{"description": "t", "confirm_sensitivity_downgrade": true})
	t.Logf("tenant create+confirm %s | tenant desc+confirm %s; audit flags %v", body, b5, flag(tcf))
	if f := flag(cf1); len(f) != 1 || f[0] != nil {
		t.Errorf("cf1 audit flags %v, want [nil]", f)
	}
	if f := flag(cf2); len(f) < 3 || f[0] != true {
		t.Errorf("cf2 audit flags %v, want [true nil…]", f)
	} else {
		for _, x := range f[1:] {
			if x != nil {
				t.Errorf("cf2 audit flags %v: a non-lowering write carries the flag", f)
			}
		}
	}
	if f := flag(tcf); len(f) != 2 || f[0] != nil || f[1] != nil {
		t.Errorf("tcf audit flags %v, want [nil nil]", f)
	}
}

// P4 — policy edges and ceilings ±1.
func TestModelAliasR2_PolicyEdges(t *testing.T) {
	m := setupModelAliasHarness(t)
	c := maDefaultCeilings()
	c.MaxRequestBytes = 4 << 20
	m.setCeilings(c)
	type pc struct{ kind, role, policy, want string }
	huge := strings.Repeat("k", 1<<20)
	cases := []pc{
		{"chat", "chat", `{"max\u0000n":1}`, "422"},
		{"chat", "chat", `{"` + huge + `":1}`, "422"},
		{"chat", "chat", `{"MAX_N":1}`, "422"},
		{"chat", "chat", `{"tools":{"max_count":5}}`, "422"},
		{"chat", "chat", `{"max_n":0}`, "422"},
		{"chat", "chat", `{"max_n":-1}`, "422"},
		{"chat", "chat", `{"max_n":1.0}`, "422"},
		{"chat", "chat", `{"max_n":2147483648}`, "422"},
		{"chat", "chat", `{"max_n":1e400}`, "422"},
		{"chat", "chat", `{"max_n":1, "max_n":1}`, "422"},
		{"chat", "chat", `{"max_n":true}`, "422"},
		{"chat", "chat", `{"vision":"true"}`, "422"},
		{"chat", "chat", `{"reasoning":"OFF"}`, "422"},
		{"chat", "chat", `{"embedding.max_inputs":4}`, "422"},
		{"chat", "chat", `{"vision_max_bytes":4194304}`, "200"},
		{"chat", "chat", `{"vision_max_bytes":4194305}`, "422"},
		{"chat", "chat", `{"max_n":8}`, "200"},
		{"chat", "chat", `{"max_n":9}`, "422"},
		{"chat", "chat", `{"timeout_s":3600}`, "200"},
		{"chat", "chat", `{"timeout_s":3601}`, "422"},
		{"chat", "chat", `{"max_output_tokens":32768,"default_max_tokens":32768}`, "200"},
		{"chat", "chat", `{"default_max_tokens":32769}`, "422"},
		{"chat", "chat", `{"tools.max_count":256}`, "200"},
		{"chat", "chat", `{"tools.max_count":257}`, "422"},
		{"embedding", "emb-ok", `{"embedding.native_dims":4096,"embedding.max_inputs":256}`, "200"},
		{"embedding", "emb-ok", `{"embedding.native_dims":4097,"embedding.max_inputs":256}`, "422"},
		{"embedding", "emb-ok", `{"embedding.native_dims":32768}`, "200"},
		{"embedding", "emb-ok", `{"embedding.native_dims":32769}`, "422"},
		{"rerank", "chat", `{"rerank.max_documents":1000,"rerank.sub_batch":125}`, "200"},
		{"rerank", "chat", `{"rerank.max_documents":1001}`, "422"},
	}
	for i, cs := range cases {
		body := fmt.Sprintf(`{"action":"model-alias-create","data":{"name":"pe%d","kind":%q,"role":%q,"enabled":true,"policy":%s}}`, i, cs.kind, cs.role, cs.policy)
		code, out, resp := m.doRaw(m.admin, body)
		shown := cs.policy
		if len(shown) > 70 {
			shown = shown[:20] + fmt.Sprintf("…(%d B)", len(shown))
		}
		r := resp
		if len(r) > 300 {
			r = r[:300] + fmt.Sprintf("…(%d B)", len(resp))
		}
		t.Logf("%-70s → %d fields=%q len=%d %s", shown, code, fieldsOf(out), len(resp), r)
		if fmt.Sprint(code) != cs.want {
			t.Errorf("policy %s = %d, want %s", shown, code, cs.want)
		}
	}
	// tenant ceiling 0 for tools.max_count
	c.Tenant.ToolsMaxCount = 0
	m.setCeilings(c)
	code, out, resp := m.do(m.ten, "model-alias-create", "", chatSpec("t0", map[string]any{"policy": map[string]any{"tools.max_count": 1}}))
	t.Logf("tenant tools.max_count 1 under ceiling 0 → %d %s %s", code, fieldsOf(out), resp)
	if code != 422 {
		t.Errorf("tenant tools.max_count under ceiling 0 = %d", code)
	}
}

// P5 — the floor counts disabled rows on both sides.
func TestModelAliasR2_FloorDisabledRows(t *testing.T) {
	m := setupModelAliasHarness(t)
	// disabled _global at credentials; server-admin tenant shadow at public + confirm.
	code, _, body := m.do(m.admin, "model-alias-create", "", map[string]any{"name": "dg", "kind": "chat", "role": "chat", "enabled": false})
	if code != 200 {
		t.Fatalf("create disabled _global = %d %s", code, body)
	}
	code, _, body = m.do(m.admin, "model-alias-create", "", chatSpec("dg", map[string]any{"tenant_id": m.tenA, "scope": "ma-a", "sensitivity": "public", "confirm_sensitivity_downgrade": true}))
	t.Logf("shadow below a DISABLED _global floor → %d %s", code, body)
	if code != 422 {
		t.Errorf("shadow below a disabled _global floor = %d, want 422", code)
	}
	// disabled tenant shadow at public; then _global at credentials.
	code, _, body = m.do(m.admin, "model-alias-create", "", map[string]any{"name": "ds", "kind": "chat", "role": "chat", "enabled": false,
		"tenant_id": m.tenA, "scope": "ma-a", "sensitivity": "public", "confirm_sensitivity_downgrade": true})
	if code != 200 {
		t.Fatalf("create disabled shadow = %d %s", code, body)
	}
	code, _, body = m.do(m.admin, "model-alias-create", "", chatSpec("ds", nil))
	t.Logf("_global over a DISABLED public shadow → %d %s", code, body)
	if code != 422 {
		t.Errorf("_global over a disabled shadow = %d, want 422", code)
	}
}

// P6 — update path under the name lock: a concurrent _global raise committed
// while the update waits is the floor the update compares with.
func TestModelAliasR2_UpdateWaitsForTheNameLock(t *testing.T) {
	m := setupModelAliasHarness(t)
	gid := m.seed("_global", "ul", "chat", "chat", "public", "{}")
	sid := m.seed("ma-a", "ul", "chat", "chat", "personal", "{}")
	tx, err := m.pool.Begin(m.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(m.ctx) }()
	if err := store.LockModelAliasName(m.ctx, tx, "ul"); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		code, _, body := m.do(m.admin, "model-alias-update", sid, map[string]any{"sensitivity": "public", "confirm_sensitivity_downgrade": true})
		done <- fmt.Sprintf("%d %s", code, body)
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := tx.Exec(m.ctx, `UPDATE context_model_aliases SET sensitivity='personal' WHERE id=$1::uuid`, gid); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(m.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		t.Logf("shadow lowered to public while _global was raised to personal under the lock → %s; shadow now %s, _global %s", r, m.col(sid, "sensitivity"), m.col(gid, "sensitivity"))
		if !strings.HasPrefix(r, "422") {
			t.Errorf("update did not see the floor committed under the lock: %s", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("update never finished")
	}
}

// P7 — update re-reads after the lock: no lost update of a field written by a
// writer that held the name lock.
func TestModelAliasR2_UpdateRereadsAfterTheLock(t *testing.T) {
	m := setupModelAliasHarness(t)
	id := m.seed("ma-a", "rr", "chat", "chat", "credentials", "{}")
	tx, err := m.pool.Begin(m.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(m.ctx) }()
	if err := store.LockModelAliasName(m.ctx, tx, "rr"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(m.ctx, `UPDATE context_model_aliases SET description='by-first-writer' WHERE id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		code, _, body := m.do(m.ten, "model-alias-update", id, map[string]any{"role": "emb-ok"})
		done <- fmt.Sprintf("%d %s", code, body)
	}()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(m.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		t.Logf("role update after a locked description write → %s; description=%s role=%s", r[:3], m.col(id, "description"), m.col(id, "role"))
		if m.col(id, "description") != "by-first-writer" || m.col(id, "role") != "emb-ok" {
			t.Errorf("lost update: description=%s role=%s", m.col(id, "description"), m.col(id, "role"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("update never finished")
	}
}

// P8 — storm: concurrent creates/updates/deletes of one name across _global
// and two tenant scopes; the committed state never breaks the floor, no 500,
// no deadlock.
func TestModelAliasR2_FloorStorm(t *testing.T) {
	m := setupModelAliasHarness(t)
	sens := []string{"public", "internal", "personal", "credentials"}
	rank := map[string]int{"public": 0, "internal": 1, "personal": 2, "credentials": 3}
	var viol, fivexx, ops int64
	var mu sync.Mutex
	codes := map[int]int{}
	stop := make(chan struct{})
	var cw sync.WaitGroup
	cw.Add(1)
	go func() {
		defer cw.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := m.pool.Query(m.ctx, `SELECT scope, sensitivity FROM context_model_aliases WHERE name='storm'`)
			if err != nil {
				continue
			}
			g := -1
			var ts []int
			for rows.Next() {
				var sc, s string
				_ = rows.Scan(&sc, &s)
				if sc == "_global" {
					g = rank[s]
				} else {
					ts = append(ts, rank[s])
				}
			}
			rows.Close()
			for _, r := range ts {
				if g >= 0 && r < g {
					atomic.AddInt64(&viol, 1)
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			for i := 0; i < 40; i++ {
				scope := []string{"_global", "ma-a", "ma-b"}[rnd.Intn(3)]
				s := sens[rnd.Intn(4)]
				var code int
				switch rnd.Intn(3) {
				case 0:
					d := chatSpec("storm", map[string]any{"sensitivity": s, "confirm_sensitivity_downgrade": true})
					if scope != "_global" {
						d["scope"] = scope
						d["tenant_id"] = map[string]string{"ma-a": m.tenA, "ma-b": m.tenB}[scope]
					}
					code, _, _ = m.do(m.admin, "model-alias-create", "", d)
				case 1:
					var id string
					_ = m.pool.QueryRow(m.ctx, `SELECT id::text FROM context_model_aliases WHERE name='storm' AND scope=$1`, scope).Scan(&id)
					if id == "" {
						continue
					}
					code, _, _ = m.do(m.admin, "model-alias-update", id, map[string]any{"sensitivity": s, "confirm_sensitivity_downgrade": true})
				case 2:
					var id string
					_ = m.pool.QueryRow(m.ctx, `SELECT id::text FROM context_model_aliases WHERE name='storm' AND scope=$1`, scope).Scan(&id)
					if id == "" || rnd.Intn(3) != 0 {
						continue
					}
					code, _, _ = m.do(m.admin, "model-alias-delete", id, nil)
				}
				atomic.AddInt64(&ops, 1)
				if code >= 500 {
					atomic.AddInt64(&fivexx, 1)
				}
				mu.Lock()
				codes[code]++
				mu.Unlock()
			}
		}(int64(w + 1))
	}
	wg.Wait()
	close(stop)
	cw.Wait()
	t.Logf("storm: ops=%d codes=%v floor violations seen=%d 5xx=%d", ops, codes, viol, fivexx)
	if viol > 0 || fivexx > 0 {
		t.Errorf("storm broke: violations=%d 5xx=%d", viol, fivexx)
	}
}
