//go:build integration

// Issue #203 integration probes (real PG18 testcontainer): the MCP `update`
// tool takes an optional `type`. Every MCP call goes through the real transport
// by tool NAME (the i201Fixture of mcp_archive_integration_test.go), so the
// probes compile against a server whose update tool has no `type` yet and fail
// there at run time, the way a client fails: the SDK's schema validation
// refuses the property.
//
//   - TestMCPUpdateType: the field re-types with manual provenance; for every
//     builtin type, a tenant's own type, another tenant's type, an unknown and a
//     malformed name the tool reaches the verdict REST manage update reaches
//     (same code, same text); another tenant's type answers byte for byte like
//     a name that never existed; the untrusted marking of a block cannot be
//     lifted; the block counters stay exact; an update without `type` leaves the
//     type alone.
//   - TestMCPUpdateTypeStaged: the staged form for confirm_writes keys — `type`
//     is an update field and hash-bound, and the claim gates and the untrusted
//     rule run at stage AND at confirm (a type that left the registry, a source
//     type whose policy changed in between).
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestMCPUpdateType' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
)

const (
	i203PlainConfig     = `{"v":1,"retrieval":{"policy":"full-pass"}}`
	i203UntrustedConfig = `{"v":1,"retrieval":{"policy":"full-pass","untrusted":true}}`
)

// i203StoreTyped writes one block over the MCP store tool, with an explicit
// type ("" leaves it to the classifier), and returns its id.
func i203StoreTyped(t *testing.T, f *i201Fixture, ctx context.Context, title, typ string) string {
	t.Helper()
	args := map[string]any{
		"category": "test", "title": title, "content": "content of " + title, "sensitivity": "internal",
	}
	if typ != "" {
		args["type"] = typ
	}
	a := f.call(t, ctx, "store", args)
	if a.RPCError != "" || a.IsError {
		t.Fatalf("fixture store %s (type %q): %+v", title, typ, a)
	}
	m := i201StoredID.FindStringSubmatch(a.Text)
	if m == nil {
		t.Fatalf("fixture store %s: no id in %q", title, a.Text)
	}
	return m[1]
}

// i203TypeOf reads the stored type and its provenance.
func i203TypeOf(t *testing.T, pool *pgxpool.Pool, id string) (name, source string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(type_name, ''), type_source FROM context_blocks WHERE id = $1`, id).Scan(&name, &source); err != nil {
		t.Fatalf("read type of %s: %v", id, err)
	}
	return name, source
}

// i203CreateType writes one registry row through the store's attributed write.
func i203CreateType(t *testing.T, pool *pgxpool.Pool, name, scope, config string) {
	t.Helper()
	if _, err := store.CreateBlockType(context.Background(), pool, store.BlockTypeWrite{
		Name: name, Scope: scope, Config: json.RawMessage(config),
	}, nil, ""); err != nil {
		t.Fatalf("create block type %s in %s: %v", name, scope, err)
	}
}

// i203RestUpdate posts REST manage update {type: typ} for ar.
func i203RestUpdate(t *testing.T, h *ManageHandler, ar *auth.AuthResult, id string, data map[string]any) (status int, code, msg string, ok bool) {
	t.Helper()
	rec := typeManageReq(t, h, ar, map[string]any{"action": "update", "id": id, "data": data})
	resp := decodeResp(t, rec)
	ok, _ = resp["success"].(bool)
	code, _ = resp["code"].(string)
	msg, _ = resp["error"].(string)
	return rec.Code, code, msg, ok
}

// i203OpenStages counts the flagged key's unconsumed stage rows (one hash, or
// all of them for "").
func i203OpenStages(t *testing.T, f *i201Fixture, hash string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_pending_writes
		  WHERE api_key_id = $1 AND ($2 = '' OR payload_hash = $2) AND consumed_at IS NULL`,
		f.flagID, hash).Scan(&n); err != nil {
		t.Fatalf("count open stages: %v", err)
	}
	return n
}

// i203HookTenantRegistry wires the production request-scope hook
// (cmd/ctxd/server.go): the registry resolves a request's tenant from the key's
// home scope. Without it no tenant's own type is ever in a request snapshot.
func i203HookTenantRegistry(t *testing.T) {
	t.Helper()
	blocktype.SetRequestScopeHook(RequestTenantScope)
	t.Cleanup(func() { blocktype.SetRequestScopeHook(nil) })
}

func TestMCPUpdateType(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()
	i203HookTenantRegistry(t)

	// Tenant rows, written before any request resolves a tenant set: two of the
	// direct key's own tenant (home scope private), one of the second tenant.
	i203CreateType(t, f.pool, "t203-own", "private", i203PlainConfig)
	i203CreateType(t, f.pool, "t203-own-untrusted", "private", i203UntrustedConfig)
	i203CreateType(t, f.pool, "t203-foreign", "i201-b", i203PlainConfig)

	directAR := AuthResultFromContext(f.direct)
	mh := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)

	t.Run("type re-types the block with manual provenance", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "t203-retype", "")
		if name, source := i203TypeOf(t, f.pool, id); name != "knowledge" || source != "auto" {
			t.Fatalf("fixture type = (%q, %q), want (knowledge, auto)", name, source)
		}
		a := f.call(t, f.direct, "update", map[string]any{"id": id, "type": "reference"})
		if a.RPCError != "" || a.IsError {
			t.Fatalf("update with type: %+v", a)
		}
		if want := "Updated: t203-retype (id: " + id + ", category: test)"; a.Text != want {
			t.Errorf("answer = %q, want %q", a.Text, want)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "reference" || source != "manual" {
			t.Errorf("type = (%q, %q), want (reference, manual)", name, source)
		}
	})

	t.Run("an update without type leaves the type alone", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "t203-no-type", "")
		a := f.call(t, f.direct, "update", map[string]any{"id": id, "content": "new content"})
		if a.RPCError != "" || a.IsError {
			t.Fatalf("update without type: %+v", a)
		}
		if want := "Updated: t203-no-type (id: " + id + ", category: test)"; a.Text != want {
			t.Errorf("answer = %q, want %q", a.Text, want)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "knowledge" || source != "auto" {
			t.Errorf("type = (%q, %q), want (knowledge, auto) — an update without type must not touch it", name, source)
		}
	})

	t.Run("claim verdicts equal REST manage update for every type", func(t *testing.T) {
		// "" = accepted; otherwise the code BOTH surfaces must answer. The
		// builtin rows are pinned by name: a builtin added later fails here
		// until somebody decides its row.
		expect := map[string]string{
			"knowledge": "", "reference": "", "audit-trail": "", "system-meta": "", "issue": "",
			"tool-evidence": "", "tool-overview": "", "checkpoint": "",
			"comment": "parent_required",
			"insight": "reserved_type", "catalog": "reserved_type",
			"t203-own": "", "t203-own-untrusted": "",
			"t203-foreign": "unknown_type", "t203-no-such-type": "unknown_type", "Not A Type!": "unknown_type",
		}
		base := f.cfg.Blocktypes.Snapshot()
		for _, n := range base.Names() {
			if pol, _ := base.Resolve(n); !pol.Builtin {
				continue
			}
			if _, ok := expect[n]; !ok {
				t.Fatalf("builtin type %q has no row in this table — decide its verdict", n)
			}
		}
		names := make([]string, 0, len(expect))
		for n := range expect {
			names = append(names, n)
		}
		slices.Sort(names)

		for i, name := range names {
			restID := i203StoreTyped(t, f, f.direct, fmt.Sprintf("t203-rest-%02d", i), "")
			mcpID := i203StoreTyped(t, f, f.direct, fmt.Sprintf("t203-mcp-%02d", i), "")
			status, code, msg, ok := i203RestUpdate(t, mh, directAR, restID, map[string]any{"type": name})
			a := f.call(t, f.direct, "update", map[string]any{"id": mcpID, "type": name})
			if a.RPCError != "" {
				t.Fatalf("%q: MCP update answered a JSON-RPC error: %s", name, a.RPCError)
			}
			want := expect[name]
			if want == "" {
				if status != http.StatusOK || !ok {
					t.Errorf("%q: REST refused (status %d, code %q, %q), want accepted", name, status, code, msg)
				}
				if a.IsError {
					t.Errorf("%q: MCP refused (code %q, %q), want accepted", name, a.Code, a.Text)
				}
				for _, id := range []string{restID, mcpID} {
					if got, source := i203TypeOf(t, f.pool, id); got != name || source != "manual" {
						t.Errorf("%q: block %s type = (%q, %q), want (%q, manual)", name, id, got, source, name)
					}
				}
				continue
			}
			if ok || status != http.StatusUnprocessableEntity || code != want {
				t.Errorf("%q: REST = (status %d, code %q, success %v), want (422, %q, false)", name, status, code, ok, want)
			}
			if !a.IsError || a.Code != want {
				t.Errorf("%q: MCP = (isError %v, code %q), want (true, %q)", name, a.IsError, a.Code, want)
			}
			if a.Text != msg {
				t.Errorf("%q: the same payload got two texts:\n  MCP  %q\n  REST %q", name, a.Text, msg)
			}
			for _, id := range []string{restID, mcpID} {
				if got, source := i203TypeOf(t, f.pool, id); got != "knowledge" || source != "auto" {
					t.Errorf("%q: refused update changed block %s to (%q, %q)", name, id, got, source)
				}
			}
		}
	})

	t.Run("another tenant's type answers exactly like a name that never existed", func(t *testing.T) {
		// Positive control: the type exists and is claimable in its own tenant.
		otherID := i203StoreTyped(t, f, f.other, "t203-other-own", "")
		if a := f.call(t, f.other, "update", map[string]any{"id": otherID, "type": "t203-foreign"}); a.RPCError != "" || a.IsError {
			t.Fatalf("the owning tenant could not claim its own type: %+v", a)
		}
		if name, _ := i203TypeOf(t, f.pool, otherID); name != "t203-foreign" {
			t.Fatalf("owning tenant's block type = %q, want t203-foreign", name)
		}

		id := i203StoreTyped(t, f, f.direct, "t203-oracle", "")
		norm := func(a i201Answer, name string) i201Answer {
			a.Text = strings.ReplaceAll(a.Text, `"`+name+`"`, `"<name>"`)
			return a
		}
		for keyName, keyCtx := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
			foreign := norm(f.call(t, keyCtx, "update", map[string]any{"id": id, "type": "t203-foreign"}), "t203-foreign")
			never := norm(f.call(t, keyCtx, "update", map[string]any{"id": id, "type": "t203-nowhere"}), "t203-nowhere")
			if foreign != never {
				t.Errorf("%s key: another tenant's type is distinguishable from an unknown name:\n  foreign %+v\n  never   %+v", keyName, foreign, never)
			}
			if foreign.Code != "unknown_type" {
				t.Errorf("%s key: foreign type code = %q, want unknown_type", keyName, foreign.Code)
			}
		}
		if n := i203OpenStages(t, f, ""); n != 0 {
			t.Errorf("a refused type claim left %d stage rows, want 0", n)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "knowledge" || source != "auto" {
			t.Errorf("block type = (%q, %q), want unchanged (knowledge, auto)", name, source)
		}
	})

	t.Run("the untrusted marking of a block cannot be lifted", func(t *testing.T) {
		for _, src := range []string{"tool-evidence", "checkpoint", "t203-own-untrusted"} {
			id := i203StoreTyped(t, f, f.direct, "t203-untrusted-"+src, src)
			for keyName, keyCtx := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
				a := f.call(t, keyCtx, "update", map[string]any{"id": id, "type": "knowledge"})
				if a.RPCError != "" || !a.IsError || a.Code != "reserved_type" {
					t.Errorf("%s key, %s → knowledge: %+v, want a reserved_type refusal", keyName, src, a)
				}
				if !strings.Contains(a.Text, `"`+src+`"`) || !strings.Contains(a.Text, "untrusted") {
					t.Errorf("%s key, %s → knowledge: text %q names neither the type nor the marking", keyName, src, a.Text)
				}
			}
			if name, source := i203TypeOf(t, f.pool, id); name != src || source != "manual" {
				t.Errorf("%s block type = (%q, %q), want unchanged (%q, manual)", src, name, source, src)
			}
		}
		if n := i203OpenStages(t, f, ""); n != 0 {
			t.Errorf("a refused re-type left %d stage rows, want 0 — the flagged key must get no card", n)
		}

		// A move between two untrusted types keeps the marking and is allowed.
		id := i203StoreTyped(t, f, f.direct, "t203-untrusted-move", "tool-evidence")
		if a := f.call(t, f.direct, "update", map[string]any{"id": id, "type": "tool-overview"}); a.RPCError != "" || a.IsError {
			t.Errorf("tool-evidence → tool-overview: %+v, want accepted", a)
		}
		if name, _ := i203TypeOf(t, f.pool, id); name != "tool-overview" {
			t.Errorf("type = %q, want tool-overview", name)
		}

		// A block of the key's own trusted tenant type re-types freely: the
		// current type resolves against the request's snapshot, not the base.
		own := i203StoreTyped(t, f, f.direct, "t203-own-source", "t203-own")
		if a := f.call(t, f.direct, "update", map[string]any{"id": own, "type": "knowledge"}); a.RPCError != "" || a.IsError {
			t.Errorf("t203-own → knowledge: %+v, want accepted", a)
		}
		if name, _ := i203TypeOf(t, f.pool, own); name != "knowledge" {
			t.Errorf("type = %q, want knowledge", name)
		}
	})

	t.Run("a current type outside the key's registry is not re-typed", func(t *testing.T) {
		// No production write leaves a block on a name the caller's registry
		// view lacks except a type owned under another home scope, so the
		// orphan is planted by SQL: it stands for both.
		id := i203StoreTyped(t, f, f.direct, "t203-orphan-source", "")
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET type_name = 't203-orphan' WHERE id = $1`, id); err != nil {
			t.Fatalf("plant orphaned type: %v", err)
		}
		for keyName, keyCtx := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
			a := f.call(t, keyCtx, "update", map[string]any{"id": id, "type": "knowledge"})
			if a.RPCError != "" || !a.IsError || a.Code != "reserved_type" || !strings.Contains(a.Text, `"t203-orphan"`) {
				t.Errorf("%s key, orphan → knowledge: %+v, want a reserved_type refusal naming the type", keyName, a)
			}
		}
		if n := i203OpenStages(t, f, ""); n != 0 {
			t.Errorf("a refused re-type left %d stage rows, want 0", n)
		}
		if name, _ := i203TypeOf(t, f.pool, id); name != "t203-orphan" {
			t.Errorf("type = %q, want unchanged t203-orphan", name)
		}
	})

	t.Run("a re-type of a derived block answers provenance_protected on both surfaces", func(t *testing.T) {
		// insight is the one type that is untrusted AND derived: the
		// provenance refusal REST answers must win over the untrusted rule.
		mkDerived := func(title string) string {
			t.Helper()
			b, err := store.UpsertBlock(ctx, f.pool, "learnings", title, "derived body", nil,
				map[string]any{"provenance": map[string]any{
					"v": derived.ContractVersion, "stratum": int(derived.StratumDerived),
				}}, "private", false, store.SensitivityWrite{}, derived.TypeInsight)
			if err != nil {
				t.Fatalf("seed derived block %s: %v", title, err)
			}
			return b.ID
		}
		restID, mcpID := mkDerived("t203-derived-rest"), mkDerived("t203-derived-mcp")
		status, code, msg, ok := i203RestUpdate(t, mh, directAR, restID, map[string]any{"type": "knowledge"})
		if ok || status != http.StatusForbidden || code != "provenance_protected" {
			t.Fatalf("REST = (status %d, code %q, success %v), want (403, provenance_protected, false)", status, code, ok)
		}
		for keyName, keyCtx := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
			a := f.call(t, keyCtx, "update", map[string]any{"id": mcpID, "type": "knowledge"})
			if a.RPCError != "" || !a.IsError || a.Code != "provenance_protected" || a.Text != msg {
				t.Errorf("%s key: %+v, want provenance_protected with REST's text %q", keyName, a, msg)
			}
		}
		for _, id := range []string{restID, mcpID} {
			if name, _ := i203TypeOf(t, f.pool, id); name != derived.TypeInsight {
				t.Errorf("derived block %s type = %q, want unchanged insight", id, name)
			}
		}
	})

	t.Run("block counters stay exact across re-types", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "t203-counted", "")
		for _, args := range []map[string]any{
			{"id": id, "type": "system-meta"},
			{"id": id, "type": "reference", "category": "t203-moved"},
			{"id": id, "type": "checkpoint"},
		} {
			if a := f.call(t, f.direct, "update", args); a.RPCError != "" || a.IsError {
				t.Fatalf("update %v: %+v", args, a)
			}
		}
		res, err := store.ReconcileBlockCounts(ctx, f.pool, "private")
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(res.Corrections) != 0 {
			t.Errorf("counters drifted after re-types: %+v", res.Corrections)
		}
	})
}

func TestMCPUpdateTypeStaged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()
	i203HookTenantRegistry(t)
	reg := f.cfg.Blocktypes

	i203CreateType(t, f.pool, "t203-vanish", "_global", i203PlainConfig)
	i203CreateType(t, f.pool, "t203-src", "_global", i203PlainConfig)
	i203CreateType(t, f.pool, "t203-src-own", "private", i203PlainConfig)
	i203CreateType(t, f.pool, "t203-src-own-flip", "private", i203PlainConfig)
	if err := reg.Reload(ctx, f.pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	reload := func(t *testing.T) {
		t.Helper()
		if err := reg.Reload(ctx, f.pool); err != nil {
			t.Fatalf("registry reload: %v", err)
		}
	}
	globalRow := func(t *testing.T, name string) *store.BlockTypeRow {
		t.Helper()
		row, err := store.GetBlockType(ctx, f.pool, name, []string{"_global"})
		if err != nil || row == nil {
			t.Fatalf("read type row %s: %v", name, err)
		}
		return row
	}
	stage := func(t *testing.T, args map[string]any) string {
		t.Helper()
		a := f.call(t, f.flagged, "update", args)
		if a.RPCError != "" || !a.IsError || a.Code != "" || !strings.Contains(a.Text, "STAGED") {
			t.Fatalf("stage %v: %+v, want an uncoded STAGED answer", args, a)
		}
		m := hashRe.FindStringSubmatch(a.Text)
		if m == nil {
			t.Fatalf("stage %v: no payload_hash in %q", args, a.Text)
		}
		return m[1]
	}
	confirm := func(t *testing.T, hash string) i201Answer {
		t.Helper()
		a := f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": hash})
		if a.RPCError != "" {
			t.Fatalf("confirm %s: JSON-RPC error %s", hash, a.RPCError)
		}
		return a
	}

	t.Run("type is a hash-bound update field and executes on confirm", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "t203-staged", "")
		hRef := stage(t, map[string]any{"id": id, "type": "reference"})
		hAudit := stage(t, map[string]any{"id": id, "type": "audit-trail"})
		if hRef == hAudit {
			t.Fatalf("two different types staged to one payload_hash %s — the type is not hash-bound", hRef)
		}
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT payload::text FROM context_pending_writes WHERE payload_hash = $1`, hRef).Scan(&raw); err != nil {
			t.Fatalf("read staged payload: %v", err)
		}
		var cw store.CanonicalWrite
		if err := json.Unmarshal(raw, &cw); err != nil {
			t.Fatalf("decode staged payload: %v", err)
		}
		if cw.Type != "reference" || !slices.Contains(cw.UpdateFields, "type") {
			t.Errorf("staged payload type = %q, update_fields = %v, want reference and a listed type", cw.Type, cw.UpdateFields)
		}
		if name, _ := i203TypeOf(t, f.pool, id); name != "knowledge" {
			t.Fatalf("staging changed the type to %q", name)
		}

		if a := confirm(t, hRef); a.IsError {
			t.Fatalf("confirm: %+v", a)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "reference" || source != "manual" {
			t.Errorf("after confirm type = (%q, %q), want (reference, manual)", name, source)
		}
		// The other card was rendered against the block before the confirm: it
		// cannot execute its type any more.
		if a := confirm(t, hAudit); !a.IsError || !strings.Contains(a.Text, "changed since this update was staged") {
			t.Errorf("stale card: %+v, want the lost-update refusal", a)
		}
		if name, _ := i203TypeOf(t, f.pool, id); name != "reference" {
			t.Errorf("stale card changed the type to %q", name)
		}
	})

	t.Run("a type that left the registry after staging is refused at confirm", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "t203-vanish-target", "")
		h := stage(t, map[string]any{"id": id, "type": "t203-vanish"})
		if _, err := store.DeleteBlockType(ctx, f.pool, globalRow(t, "t203-vanish").ID, []string{"_global"}, nil, ""); err != nil {
			t.Fatalf("delete type: %v", err)
		}
		reload(t)
		a := confirm(t, h)
		if !a.IsError || a.Code != "unknown_type" {
			t.Errorf("confirm after the type left: %+v, want an unknown_type refusal", a)
		}
		if n := i203OpenStages(t, f, h); n != 1 {
			t.Errorf("a refusal before the consume spent the token (open = %d, want 1)", n)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "knowledge" || source != "auto" {
			t.Errorf("type = (%q, %q), want unchanged (knowledge, auto)", name, source)
		}
	})

	t.Run("a source type that turned untrusted after staging is refused at confirm", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "t203-src-target", "t203-src")
		h := stage(t, map[string]any{"id": id, "type": "knowledge"})
		if _, err := store.UpdateBlockType(ctx, f.pool, globalRow(t, "t203-src").ID, []string{"_global"},
			store.BlockTypeUpdate{Config: json.RawMessage(i203UntrustedConfig)}, nil, ""); err != nil {
			t.Fatalf("mark the source type untrusted: %v", err)
		}
		reload(t)
		a := confirm(t, h)
		if !a.IsError || a.Code != "reserved_type" || !strings.Contains(a.Text, "untrusted") {
			t.Errorf("confirm after the source type turned untrusted: %+v, want a reserved_type refusal naming the marking", a)
		}
		if n := i203OpenStages(t, f, h); n != 1 {
			t.Errorf("a refusal before the consume spent the token (open = %d, want 1)", n)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "t203-src" || source != "manual" {
			t.Errorf("type = (%q, %q), want unchanged (t203-src, manual)", name, source)
		}
	})

	t.Run("the confirm decides over the key's own tenant types", func(t *testing.T) {
		// A trusted tenant type confirms; a tenant type marked untrusted after
		// staging refuses. Both resolve only in the request's snapshot.
		keep := i203StoreTyped(t, f, f.direct, "t203-src-own-target", "t203-src-own")
		flip := i203StoreTyped(t, f, f.direct, "t203-src-own-flip-target", "t203-src-own-flip")
		hKeep := stage(t, map[string]any{"id": keep, "type": "knowledge"})
		hFlip := stage(t, map[string]any{"id": flip, "type": "knowledge"})
		row, err := store.GetBlockType(ctx, f.pool, "t203-src-own-flip", []string{"private"})
		if err != nil || row == nil {
			t.Fatalf("read tenant type row: %v", err)
		}
		if _, err := store.UpdateBlockType(ctx, f.pool, row.ID, []string{"private"},
			store.BlockTypeUpdate{Config: json.RawMessage(i203UntrustedConfig)}, nil, ""); err != nil {
			t.Fatalf("mark the tenant type untrusted: %v", err)
		}
		reload(t)
		reg.InvalidateTenant("private")

		if a := confirm(t, hKeep); a.IsError {
			t.Errorf("confirm over a trusted tenant type: %+v, want executed", a)
		}
		if name, _ := i203TypeOf(t, f.pool, keep); name != "knowledge" {
			t.Errorf("type = %q, want knowledge", name)
		}
		a := confirm(t, hFlip)
		if !a.IsError || a.Code != "reserved_type" || !strings.Contains(a.Text, `"t203-src-own-flip"`) {
			t.Errorf("confirm after the tenant type turned untrusted: %+v, want a reserved_type refusal naming it", a)
		}
		if n := i203OpenStages(t, f, hFlip); n != 1 {
			t.Errorf("a refusal before the consume spent the token (open = %d, want 1)", n)
		}
		if name, _ := i203TypeOf(t, f.pool, flip); name != "t203-src-own-flip" {
			t.Errorf("type = %q, want unchanged t203-src-own-flip", name)
		}
	})
}
