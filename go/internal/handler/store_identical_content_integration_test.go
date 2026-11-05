//go:build integration

// Issue #64 — POST /api/store answered a write whose CONTENT matched the live
// block of the same (category, title, scope) with
// {success:true, action:"noop"} before the upsert ran, and the comparison
// covered the content hash only. The request's tags and metadata were dropped
// while the answer said success: two `ctx persist --hook` runs from two
// projects left the block tagged with the first project only, and the second
// run counted it as saved.
//
// The noop now holds only when the upsert would write nothing: tags,
// metadata, an explicit sensitivity upgrade and an explicit type are compared
// too. Otherwise the write takes the ordinary upsert and the answer names it:
// action "updated", reason "identical_content", changed [...]. Tags are the
// request's, as on every upsert. On identical content the request's metadata
// is MERGED into the stored document — keys other writers put there survive a
// byte-identical re-save — and the duplicate guard's verdict stays: the
// guard_* keys decide nothing but "is this vector a duplicate", so a
// tags-only rewrite must neither forge them (the request's guard_checked_at
// and guard_resolution) nor wipe them (a block a person KEPT would re-enter
// the guard queue and could be archived by the re-check, operations.md
// "A guard-resolve trace"). The embedding stays as well (W04-8).
//
// The fixture's guard state comes from the production writers —
// guard.RunGuardBatch for the verdict, store.GuardResolve for the person's
// "keep" — not from a hand-written jsonb literal, so the probe cannot share an
// assumption about the key set with the code under test (W10). The one
// hand-written key is supersedes_repair in e: its only writer is migration
// 154, and the merge it probes is key-agnostic.
//
// Subtests run in order on ONE block (f on its own); RED before the fix
// unless marked:
//
//	a tags_and_metadata_reach_the_store — second write with identical content
//	                                       and new tags/metadata ⇒ written,
//	                                       action "updated", guard verdict and
//	                                       embedding untouched, not re-queued.
//	                                       RED: action "noop", tags/metadata of
//	                                       the first write.
//	b identical_request_stays_noop       — the same request again ⇒ noop and
//	                                       nothing written, although the stored
//	                                       metadata carries guard_* keys the
//	                                       request does not. GREEN before AND
//	                                       after: a pin, not a gate (W19) — it
//	                                       is what keeps every guard-checked
//	                                       block from being rewritten on each
//	                                       identical save.
//	c changed_names_the_differing_field  — tags-only ⇒ changed ["tags"],
//	                                       metadata-only ⇒ changed ["metadata"].
//	                                       RED: noop.
//	e server_keys_survive                — a key the request never names
//	                                       (supersedes_repair, a client key the
//	                                       request drops) survives a noop AND a
//	                                       metadata-changing write. RED: noop
//	                                       drops the new session_id.
//	g tag_order_counts                   — reordered tags ⇒ changed ["tags"],
//	                                       order stored. RED: noop.
//	h sensitivity_and_type               — explicit sensitivity upgrade ⇒
//	                                       changed ["sensitivity"], explicit
//	                                       type ⇒ changed ["type"], each a noop
//	                                       when repeated; a downgrade request ⇒
//	                                       noop, level kept. RED: noop, block
//	                                       stays public / auto.
//	f unchecked_block_client_marker      — a block the guard has not checked,
//	                                       saved twice with a client
//	                                       guard_checked_at ⇒ second is noop,
//	                                       marker never stored. GREEN before AND
//	                                       after: a pin on the strip inside the
//	                                       comparison.
//	d content_change_resets_the_guard    — new content ⇒ ordinary upsert answer,
//	                                       guard_checked_at gone, embedding
//	                                       cleared. GREEN before AND after: pins
//	                                       that the merge and the carry-over are
//	                                       bound to identical content.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestStoreIdenticalContentTagsMetadata -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStoreIdenticalContentTagsMetadata(t *testing.T) {
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
	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}

	_, plain, err := store.CreateApiKey(ctx, pool, "i64-rest", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v (valid=%v)", err, ar != nil && ar.IsValid)
	}
	keyCtx := context.WithValue(ctx, authResultKey, ar)

	post := func(t *testing.T, payload map[string]any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /api/store = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode answer: %v (body %s)", err, rec.Body.String())
		}
		if decoded["success"] != true {
			t.Fatalf("answer without success:true: %v", decoded)
		}
		return decoded
	}

	type state struct {
		tags       []string
		meta       map[string]any
		content    string
		hasEmbed   bool
		model      string
		updated    time.Time
		sens       string
		sensSource string
		typeName   string
		typeSource string
	}
	read := func(t *testing.T, id string) state {
		t.Helper()
		var s state
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(tags, '{}'), COALESCE(metadata, '{}'::jsonb), content,
			        embedding IS NOT NULL, COALESCE(embed_model, ''), updated_at,
			        sensitivity, sensitivity_source, type_name, type_source
			   FROM context_blocks WHERE id = $1::uuid`, id,
		).Scan(&s.tags, &s.meta, &s.content, &s.hasEmbed, &s.model, &s.updated,
			&s.sens, &s.sensSource, &s.typeName, &s.typeSource); err != nil {
			t.Fatalf("read block %s: %v", id, err)
		}
		return s
	}
	// guardKeys is the guard's part of a metadata document, computed in Go
	// from the decoded map — independent of the SQL the fix uses.
	guardKeys := func(t *testing.T, meta map[string]any) string {
		t.Helper()
		sub := map[string]any{}
		for k, v := range meta {
			if strings.HasPrefix(k, "guard_") {
				sub[k] = v
			}
		}
		raw, err := json.Marshal(sub) // map keys marshal sorted
		if err != nil {
			t.Fatalf("marshal guard keys: %v", err)
		}
		return string(raw)
	}
	keysOf := func(m map[string]any) string {
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	changedOf := func(resp map[string]any) []string {
		raw, _ := resp["changed"].([]any)
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			s, _ := v.(string)
			out = append(out, s)
		}
		return out
	}

	const (
		category = "learnings"
		title    = "i64-persist-block"
		content  = "[PERSIST:i64] identical body that two hook runs emit"
		forged   = "1999-01-01T00:00:00Z"
	)
	// guardPicks runs the production guard batch several times and sums what
	// it processed. The handler's fire-and-forget writers (access log,
	// content_times, context_temporal) briefly lock the row after every write,
	// and the batch picks FOR UPDATE SKIP LOCKED — one run can skip a pending
	// block. Over several spaced runs a pending block is picked; a block that
	// is not pending never is.
	guardPicks := func(t *testing.T, attempts int) int {
		t.Helper()
		total := 0
		for i := 0; i < attempts && total == 0; i++ {
			if i > 0 {
				time.Sleep(100 * time.Millisecond)
			}
			n, err := guard.RunGuardBatch(ctx, pool, reg.Snapshot(), 10)
			if err != nil {
				t.Fatalf("guard batch: %v", err)
			}
			total += n
		}
		return total
	}
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = 0.25
	}

	// Run A (project /compose/n8n), then the guard decides and a person keeps
	// the block — the strongest guard state a block can carry.
	first := post(t, map[string]any{
		"category": category, "title": title, "content": content,
		"tags":     []string{"/compose/n8n"},
		"metadata": map[string]any{"project": "/compose/n8n", "session_id": "run-a", "agent_type": "general-purpose"},
	})
	blk, _ := first["block"].(map[string]any)
	id, _ := blk["id"].(string)
	if id == "" {
		t.Fatalf("seed write returned no block id: %v", first)
	}
	if err := store.StoreEmbedding(ctx, pool, id, "i64-model", vec); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if n := guardPicks(t, 20); n != 1 {
		t.Fatalf("precondition: guard batch processed %d block(s), want exactly the fixture — it is not guard-eligible, so the probe would not isolate the guard state", n)
	}
	if _, err := store.GuardResolve(ctx, pool, id, "keep", []string{"private"}); err != nil {
		t.Fatalf("GuardResolve keep: %v", err)
	}
	seeded := read(t, id)
	seededGuard := guardKeys(t, seeded.meta)
	for _, k := range []string{"guard_checked_at", "guard_status", "guard_resolution"} {
		if _, ok := seeded.meta[k]; !ok {
			t.Fatalf("precondition: fixture metadata lacks %s after guard batch + keep: %v", k, seeded.meta)
		}
	}
	if !seeded.hasEmbed {
		t.Fatalf("precondition: fixture lost its embedding before the probe")
	}

	runB := map[string]any{
		"category": category, "title": title, "content": content,
		"tags": []string{"/root"},
		"metadata": map[string]any{
			"project": "/root", "session_id": "run-b", "agent_type": "general-purpose",
			"guard_checked_at": forged, "guard_resolution": "forged-by-client",
		},
	}

	t.Run("a_tags_and_metadata_reach_the_store", func(t *testing.T) {
		resp := post(t, runB)
		if resp["action"] != "updated" || resp["reason"] != "identical_content" {
			t.Errorf("answer action=%v reason=%v, want updated/identical_content — the request's tags and metadata were dropped behind a success answer (#64): %v",
				resp["action"], resp["reason"], resp)
		}
		if got := changedOf(resp); !slices.Equal(got, []string{"tags", "metadata"}) {
			t.Errorf("changed = %v, want [tags metadata]", got)
		}
		if b, _ := resp["block"].(map[string]any); b == nil || b["id"] != id {
			t.Errorf("answer block = %v, want the existing block %s", resp["block"], id)
		}

		got := read(t, id)
		if !slices.Equal(got.tags, []string{"/root"}) {
			t.Errorf("stored tags = %v, want [/root] — the second request is the truth for tags, as on every upsert", got.tags)
		}
		if got.meta["project"] != "/root" || got.meta["session_id"] != "run-b" {
			t.Errorf("stored metadata project=%v session_id=%v, want /root / run-b (attribution still points at run A)",
				got.meta["project"], got.meta["session_id"])
		}
		if g := guardKeys(t, got.meta); g != seededGuard {
			t.Errorf("guard verdict changed by a tags/metadata-only write:\nbefore = %s\nafter  = %s", seededGuard, g)
		}
		if got.meta["guard_checked_at"] == forged || got.meta["guard_resolution"] == "forged-by-client" {
			t.Errorf("the request's guard keys landed in the store: guard_checked_at=%v guard_resolution=%v",
				got.meta["guard_checked_at"], got.meta["guard_resolution"])
		}
		if !got.hasEmbed || got.model != seeded.model {
			t.Errorf("embedding touched by an identical-content write: present=%v model=%q (want true / %q)", got.hasEmbed, got.model, seeded.model)
		}
		if got.content != content {
			t.Errorf("content changed: %q", got.content)
		}
		if n := guardPicks(t, 10); n != 0 {
			t.Errorf("guard batch after the write processed %d block(s), want 0 — a kept block re-entered the guard queue", n)
		}
	})

	t.Run("b_identical_request_stays_noop", func(t *testing.T) {
		before := read(t, id)
		resp := post(t, runB)
		if resp["action"] != "noop" {
			t.Fatalf("identical request answered %v, want noop: %v", resp["action"], resp)
		}
		if got := keysOf(resp); got != "action,existing_id,reason,success" {
			t.Errorf("noop envelope keys = %s, want action,existing_id,reason,success", got)
		}
		if resp["existing_id"] != id {
			t.Errorf("existing_id = %v, want %s", resp["existing_id"], id)
		}
		if after := read(t, id); !after.updated.Equal(before.updated) {
			t.Errorf("noop wrote the row: updated_at %s -> %s", before.updated, after.updated)
		}
	})

	t.Run("c_changed_names_the_differing_field", func(t *testing.T) {
		tagsOnly := map[string]any{
			"category": category, "title": title, "content": content,
			"tags":     []string{"/root", "/srv"},
			"metadata": runB["metadata"],
		}
		resp := post(t, tagsOnly)
		if resp["action"] != "updated" || !slices.Equal(changedOf(resp), []string{"tags"}) {
			t.Errorf("tags-only write: action=%v changed=%v, want updated [tags]", resp["action"], changedOf(resp))
		}
		if got := read(t, id); !slices.Equal(got.tags, []string{"/root", "/srv"}) {
			t.Errorf("tags-only write: stored tags = %v", got.tags)
		}

		metaOnly := map[string]any{
			"category": category, "title": title, "content": content,
			"tags":     []string{"/root", "/srv"},
			"metadata": map[string]any{"project": "/root", "session_id": "run-c", "agent_type": "general-purpose"},
		}
		resp = post(t, metaOnly)
		if resp["action"] != "updated" || !slices.Equal(changedOf(resp), []string{"metadata"}) {
			t.Errorf("metadata-only write: action=%v changed=%v, want updated [metadata]", resp["action"], changedOf(resp))
		}
		got := read(t, id)
		if got.meta["session_id"] != "run-c" {
			t.Errorf("metadata-only write: stored session_id = %v, want run-c", got.meta["session_id"])
		}
		if g := guardKeys(t, got.meta); g != seededGuard {
			t.Errorf("metadata-only write changed the guard verdict:\nbefore = %s\nafter  = %s", seededGuard, g)
		}
	})

	// current is the request the block answers noop to after c.
	current := map[string]any{
		"category": category, "title": title, "content": content,
		"tags":     []string{"/root", "/srv"},
		"metadata": map[string]any{"project": "/root", "session_id": "run-c", "agent_type": "general-purpose"},
	}

	t.Run("e_server_keys_survive", func(t *testing.T) {
		// Migration 154 is the only writer of this key; the merge under test
		// does not look at key names.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET metadata = metadata || '{"supersedes_repair": {"migration": 154}}'::jsonb
			  WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("seed supersedes_repair: %v", err)
		}
		if resp := post(t, current); resp["action"] != "noop" {
			t.Errorf("byte-identical re-save of a block carrying a server-written key answered %v, want noop: %v", resp["action"], resp)
		}
		if got := read(t, id); got.meta["supersedes_repair"] == nil {
			t.Errorf("noop path lost supersedes_repair: %v", got.meta)
		}

		resp := post(t, map[string]any{
			"category": category, "title": title, "content": content,
			"tags":     []string{"/root", "/srv"},
			"metadata": map[string]any{"project": "/root", "session_id": "run-e"},
		})
		if resp["action"] != "updated" || !slices.Equal(changedOf(resp), []string{"metadata"}) {
			t.Errorf("metadata-changing write: action=%v changed=%v, want updated [metadata]", resp["action"], changedOf(resp))
		}
		got := read(t, id)
		if got.meta["session_id"] != "run-e" {
			t.Errorf("stored session_id = %v, want run-e (the request is the truth for the keys it names)", got.meta["session_id"])
		}
		if got.meta["supersedes_repair"] == nil || got.meta["agent_type"] != "general-purpose" {
			t.Errorf("keys the request did not name were dropped on identical content: supersedes_repair=%v agent_type=%v",
				got.meta["supersedes_repair"], got.meta["agent_type"])
		}
		if g := guardKeys(t, got.meta); g != seededGuard {
			t.Errorf("guard verdict changed:\nbefore = %s\nafter  = %s", seededGuard, g)
		}
	})

	t.Run("g_tag_order_counts", func(t *testing.T) {
		resp := post(t, map[string]any{
			"category": category, "title": title, "content": content,
			"tags":     []string{"/srv", "/root"},
			"metadata": map[string]any{"project": "/root", "session_id": "run-e"},
		})
		if resp["action"] != "updated" || !slices.Equal(changedOf(resp), []string{"tags"}) {
			t.Errorf("reordered tags: action=%v changed=%v, want updated [tags]", resp["action"], changedOf(resp))
		}
		if got := read(t, id); !slices.Equal(got.tags, []string{"/srv", "/root"}) {
			t.Errorf("stored tags = %v, want [/srv /root] — the order is part of the stored value", got.tags)
		}
	})

	t.Run("h_sensitivity_and_type", func(t *testing.T) {
		same := func(extra map[string]any) map[string]any {
			m := map[string]any{
				"category": category, "title": title, "content": content,
				"tags":     []string{"/srv", "/root"},
				"metadata": map[string]any{"project": "/root", "session_id": "run-e"},
			}
			for k, v := range extra {
				m[k] = v
			}
			return m
		}
		before := read(t, id)
		if before.sens != "public" || before.typeSource != "auto" {
			t.Fatalf("precondition: block is %s/%s type %s/%s, want public and an auto type", before.sens, before.sensSource, before.typeName, before.typeSource)
		}

		resp := post(t, same(map[string]any{"sensitivity": "credentials"}))
		if resp["action"] != "updated" || !slices.Equal(changedOf(resp), []string{"sensitivity"}) {
			t.Errorf("explicit sensitivity upgrade: action=%v changed=%v, want updated [sensitivity] — the upgrade was dropped behind a success answer", resp["action"], changedOf(resp))
		}
		if got := read(t, id); got.sens != "credentials" || got.sensSource != "manual" {
			t.Errorf("stored sensitivity = %s/%s, want credentials/manual", got.sens, got.sensSource)
		}
		if resp := post(t, same(map[string]any{"sensitivity": "credentials"})); resp["action"] != "noop" {
			t.Errorf("repeated sensitivity assertion answered %v, want noop", resp["action"])
		}
		// A downgrade is not applied on the write path (upgrade-only), so it
		// changes nothing: noop, and the block keeps its level.
		if resp := post(t, same(map[string]any{"sensitivity": "public"})); resp["action"] != "noop" {
			t.Errorf("downgrade request on identical content answered %v changed=%v, want noop — the upsert would not apply it", resp["action"], changedOf(resp))
		}
		if got := read(t, id); got.sens != "credentials" || got.sensSource != "manual" {
			t.Errorf("downgrade request changed the block: %s/%s, want credentials/manual", got.sens, got.sensSource)
		}

		resp = post(t, same(map[string]any{"type": before.typeName}))
		if resp["action"] != "updated" || !slices.Equal(changedOf(resp), []string{"type"}) {
			t.Errorf("explicit type: action=%v changed=%v, want updated [type]", resp["action"], changedOf(resp))
		}
		if got := read(t, id); got.typeName != before.typeName || got.typeSource != "manual" {
			t.Errorf("stored type = %s/%s, want %s/manual", got.typeName, got.typeSource, before.typeName)
		}
		if resp := post(t, same(map[string]any{"type": before.typeName})); resp["action"] != "noop" {
			t.Errorf("repeated type assertion answered %v, want noop", resp["action"])
		}
	})

	t.Run("f_unchecked_block_client_marker", func(t *testing.T) {
		req := map[string]any{
			"category": category, "title": "i64-unchecked", "content": "a block the guard never saw",
			"metadata": map[string]any{"x": "1", "guard_checked_at": forged},
		}
		first := post(t, req)
		b, _ := first["block"].(map[string]any)
		uid, _ := b["id"].(string)
		if resp := post(t, req); resp["action"] != "noop" {
			t.Errorf("second identical save with a client guard_checked_at answered %v, want noop — the comparison must strip the marker exactly like the write does", resp["action"])
		}
		if got := read(t, uid); got.meta["guard_checked_at"] != nil {
			t.Errorf("client guard_checked_at stored: %v", got.meta)
		}
	})

	t.Run("d_content_change_resets_the_guard", func(t *testing.T) {
		resp := post(t, map[string]any{
			"category": category, "title": title, "content": content + " — edited",
			"tags": []string{"/root"},
		})
		if got := keysOf(resp); got != "block,success" {
			t.Errorf("content-changing upsert answer keys = %s, want block,success (shape unchanged)", got)
		}
		got := read(t, id)
		if _, ok := got.meta["guard_checked_at"]; ok {
			t.Errorf("content change kept guard_checked_at — the verdict about the old vector survived a new content: %v", got.meta)
		}
		if got.hasEmbed {
			t.Errorf("content change kept the stale embedding")
		}
	})
}
