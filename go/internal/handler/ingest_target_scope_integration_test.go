//go:build integration

// `ctx ingest --scope` was sent by the CLI and never read by the server: every
// chunk landed in the key's home scope and the run reported success (#322).
// POST /api/ingest now takes `target_scope`, resolved by resolveWriteScope, the
// gate of POST /api/store: absent or empty means the key's home scope, a scope
// the key may not write is refused whole (403 scope_denied) before a chunk is
// written. The field `scope`, which every earlier CLI sends as "private"
// whether the user chose it or not, stays ignored. The answer names the scope
// the request wrote to, and the CLI prints it.
//
// TestIngestCLITargetScope drives the FULL cobra tree against the production
// HandleIngest on a real PG18 with real minted keys; TestIngestTargetScopeWire
// sends the request bodies of old and new clients to the handler directly and
// checks every scope-dependent step of the write at the resolved scope.
//
//	go test -tags=integration ./internal/handler/ -run 'TestIngestCLITargetScope|TestIngestTargetScopeWire' -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// tsKey mints a key through the self-service mint (store.MintKeyWithQuota) in
// tenantID and authenticates it, so the AuthResult carries the rights the auth
// path derives from the row. allowed nil in the default tenant means
// {shared}, as for every key minted without --allowed.
func tsKey(t *testing.T, pool *pgxpool.Pool, label, home string, allowed, write []string, tenantID string) *auth.AuthResult {
	t.Helper()
	ctx := context.Background()
	_, plain, err := store.MintKeyWithQuota(ctx, pool, label, home, allowed, write, tenantID, "member", nil, store.AuditStamp{})
	if err != nil {
		t.Fatalf("mint key %s: %v", label, err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate %s: %v", label, err)
	}
	return ar
}

// tsServer mounts the production ingest handler behind an AuthResult injector
// carrying ar.
func tsServer(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
		})
	})
	r.Post("/api/ingest", NewIngestHandler(pool, nil, nil).HandleIngest)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// tsNote is a one-chunk note carrying marker.
func tsNote(marker string) string {
	return "# Note\n\n" + marker + " " + strings.Repeat("Import the folder and compare the scope with the request. ", 6) + "\n"
}

// tsScopesOf counts the live blocks whose content carries marker, per scope.
func tsScopesOf(t *testing.T, pool *pgxpool.Pool, marker string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT scope, count(*) FROM context_blocks
		  WHERE NOT is_archived AND content LIKE '%' || $1 || '%' GROUP BY scope`, marker)
	if err != nil {
		t.Fatalf("count blocks of %s: %v", marker, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var scope string
		var n int
		if err := rows.Scan(&scope, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[scope] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func sameCounts(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestIngestCLITargetScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	const foreignScope = "tb322:main"
	tenantB := bgTenant(t, pool, "tb322")
	bgMapScope(t, pool, foreignScope, tenantB)

	type keySpec struct {
		home           string
		allowed, write []string
		tenant         string
	}
	cases := []struct {
		name    string
		key     keySpec
		args    []string
		want    map[string]int // scope -> blocks of the note; empty = none anywhere
		wantErr bool
		wantOut string // a line the run prints on stderr
	}{
		// The key may write 'shared' (default tenant, allowed defaults to {shared}).
		{"named_writable_scope", keySpec{"private", nil, nil, store.DefaultTenantID},
			[]string{"--scope", "shared"}, map[string]int{"shared": 1}, false, "Scope:    shared"},
		// A scope the key may write through write_scopes.
		{"named_write_scope", keySpec{"private", []string{"team322"}, []string{"team322"}, store.DefaultTenantID},
			[]string{"--scope", "team322"}, map[string]int{"team322": 1}, false, "Scope:    team322"},
		// No flag: the key's home scope, also when it is not 'private' (the old
		// CLI default) — the flag's default must not move or refuse these runs.
		{"no_flag_is_home_scope", keySpec{"work322", []string{}, nil, store.DefaultTenantID},
			nil, map[string]int{"work322": 1}, false, "Scope:    work322"},
		// 'shared' is not in this key's rights.
		{"named_unwritable_scope", keySpec{"private", []string{}, nil, store.DefaultTenantID},
			[]string{"--scope", "shared"}, map[string]int{}, true, em4ScopeDeniedMsg},
		// Another tenant's scope, from the default tenant ...
		{"other_tenants_scope", keySpec{"private", nil, nil, store.DefaultTenantID},
			[]string{"--scope", foreignScope}, map[string]int{}, true, em4ScopeDeniedMsg},
		// ... and the default tenant's scope from another tenant's key.
		{"default_tenant_scope_from_other_tenant", keySpec{foreignScope, nil, nil, tenantB},
			[]string{"--scope", "private"}, map[string]int{}, true, em4ScopeDeniedMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar := tsKey(t, pool, "ts322-"+tc.name, tc.key.home, tc.key.allowed, tc.key.write, tc.key.tenant)
			srv := tsServer(t, pool, ar)
			vault := t.TempDir()
			marker := "MARKER-ts322-" + tc.name
			writeVaultFile(t, vault, "note.md", tsNote(marker))

			out, err := runIngestCLIStderr(t, srv.URL, append([]string{"ingest", vault, "--category", "ts322"}, tc.args...)...)

			if got := tsScopesOf(t, pool, marker); !sameCounts(got, tc.want) {
				t.Errorf("blocks per scope = %v, want %v", got, tc.want)
			}
			if tc.wantErr && err == nil {
				t.Errorf("ctx ingest returned nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ctx ingest: %v", err)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("stderr lacks %q:\n%s", tc.wantOut, out)
			}
		})
	}
}

func TestIngestTargetScopeWire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := NewIngestHandler(pool, staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitWrite: 100}}}, nil)
	post := func(ar *auth.AuthResult, body map[string]any) (int, map[string]any) {
		t.Helper()
		return postIngest(t, h, context.WithValue(ctx, authResultKey, ar), body)
	}
	chunk := func(title, content string) map[string]any {
		return map[string]any{"category": "ts322", "title": title, "content": content}
	}
	withScope := func(key string, scope any, chunks ...map[string]any) map[string]any {
		body := map[string]any{"source": "obsidian", "chunks": chunks}
		if key != "" {
			body[key] = scope
		}
		return body
	}

	// Every earlier CLI sends scope "private" on every request, chosen or
	// not. Honouring it would refuse this key (it may not write 'private') or,
	// for a key that may, move its chunks out of the home scope they always
	// went to. The field stays ignored, also when it names a writable scope.
	t.Run("legacy_scope_field_is_ignored", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-legacy-work", "work322l", []string{}, nil, store.DefaultTenantID)
		status, resp := post(ar, withScope("scope", "private", chunk("ts322 legacy default", "MARKER-ts322-legacy-default")))
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("old CLI default: status %d body %v, want 200 success", status, resp)
		}
		if got := tsScopesOf(t, pool, "MARKER-ts322-legacy-default"); !sameCounts(got, map[string]int{"work322l": 1}) {
			t.Errorf("old CLI default: blocks per scope = %v, want the home scope", got)
		}
		if resp["scope"] != "work322l" {
			t.Errorf("old CLI default: answer scope = %v, want work322l", resp["scope"])
		}

		ar = tsKey(t, pool, "ts322-legacy-shared", "private", nil, nil, store.DefaultTenantID)
		status, resp = post(ar, withScope("scope", "shared", chunk("ts322 legacy shared", "MARKER-ts322-legacy-shared")))
		if status != http.StatusOK {
			t.Fatalf("old CLI --scope shared: status %d body %v, want 200", status, resp)
		}
		if got := tsScopesOf(t, pool, "MARKER-ts322-legacy-shared"); !sameCounts(got, map[string]int{"private": 1}) {
			t.Errorf("old CLI --scope shared: blocks per scope = %v, want the home scope as before", got)
		}
		if resp["scope"] != "private" {
			t.Errorf("old CLI --scope shared: answer scope = %v, want private", resp["scope"])
		}
	})

	t.Run("absent_or_empty_target_scope_is_home", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-empty", "work322e", []string{}, nil, store.DefaultTenantID)
		for _, body := range []map[string]any{
			withScope("", nil, chunk("ts322 absent", "MARKER-ts322-absent")),
			withScope("target_scope", "", chunk("ts322 empty", "MARKER-ts322-empty")),
		} {
			status, resp := post(ar, body)
			if status != http.StatusOK || resp["scope"] != "work322e" {
				t.Errorf("status %d answer scope %v, want 200 and work322e (body %v)", status, resp["scope"], resp)
			}
		}
		for _, m := range []string{"MARKER-ts322-absent", "MARKER-ts322-empty"} {
			if got := tsScopesOf(t, pool, m); !sameCounts(got, map[string]int{"work322e": 1}) {
				t.Errorf("%s: blocks per scope = %v, want the home scope", m, got)
			}
		}
	})

	// The whole request is refused before anything is written, and before the
	// write budget, so a refused scope costs none.
	t.Run("denied_target_scope_refuses_the_batch", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-denied", "private", []string{}, nil, store.DefaultTenantID)
		status, resp := post(ar, withScope("target_scope", "shared",
			chunk("ts322 denied a", "MARKER-ts322-denied"), chunk("ts322 denied b", "MARKER-ts322-denied")))
		if status != http.StatusForbidden || resp["code"] != "scope_denied" || resp["error"] != em4ScopeDeniedMsg {
			t.Fatalf("status %d body %v, want 403 scope_denied %q", status, resp, em4ScopeDeniedMsg)
		}
		if got := tsScopesOf(t, pool, "MARKER-ts322-denied"); len(got) != 0 {
			t.Errorf("refused request wrote blocks: %v", got)
		}
		n, err := store.CheckRateLimitByAction(ctx, pool, ar.ApiKeyID, ingestBucket)
		if err != nil || n != 0 {
			t.Errorf("refused request booked the ingest bucket: %d (err %v), want 0", n, err)
		}
	})

	// The identical-content check asks the scope the chunk is written to: the
	// same chunk already in the home scope does not make a write to 'shared' a
	// no-op.
	t.Run("identical_content_is_checked_in_the_target_scope", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-identical", "private", nil, nil, store.DefaultTenantID)
		c := chunk("ts322 identical", "MARKER-ts322-identical")
		_, resp := post(ar, withScope("", nil, c))
		home := firstResult(t, resp)
		_, resp = post(ar, withScope("target_scope", "shared", c))
		r := firstResult(t, resp)
		if r["status"] == "skipped" || r["status"] == "failed" || r["id"] == home["id"] {
			t.Fatalf("write to shared = %v, want a block of its own (home block %v)", r, home)
		}
		if got := tsScopesOf(t, pool, "MARKER-ts322-identical"); !sameCounts(got, map[string]int{"private": 1, "shared": 1}) {
			t.Errorf("blocks per scope = %v, want one in private and one in shared", got)
		}
		_, resp = post(ar, withScope("target_scope", "shared", c))
		if r := firstResult(t, resp); r["status"] != "skipped" {
			t.Errorf("repeat write to shared = %v, want skipped", r)
		}
	})

	// The I7 batch gate asks the target scope for a derived block of the same
	// (category, title); the home scope holds none.
	t.Run("provenance_claim_is_checked_in_the_target_scope", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-claim", "private", nil, nil, store.DefaultTenantID)
		const title = "ts322 derived in shared"
		seeded, err := store.UpsertBlock(ctx, pool, "ts322", title, "derived body MARKER-ts322-claim-seed", nil,
			map[string]any{derived.MetadataKey: map[string]any{
				"v": derived.ContractVersion, "stratum": int(derived.StratumDerived),
			}}, "shared", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed derived block: %v", err)
		}
		status, resp := post(ar, withScope("target_scope", "shared", chunk(title, "client text MARKER-ts322-claim")))
		if status != http.StatusForbidden || resp["code"] != "provenance_protected" {
			t.Fatalf("status %d body %v, want 403 provenance_protected", status, resp)
		}
		if got := readChunk(t, pool, seeded.ID); !strings.Contains(got.content, "derived body") {
			t.Errorf("derived block changed: %+v", got)
		}
		status, resp = post(ar, withScope("", nil, chunk(title, "client text MARKER-ts322-claim")))
		if status != http.StatusOK || resp["failed"] != float64(0) {
			t.Errorf("home scope write: status %d body %v, want 200 without failures", status, resp)
		}
	})

	// The scope gate answers before the provenance probe: a scope the key may
	// not write gets scope_denied whether or not it holds a derived block of
	// that (category, title), so the answer says nothing about its contents.
	t.Run("denied_scope_is_refused_before_the_provenance_probe", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-probe-order", "private", nil, nil, store.DefaultTenantID)
		const title = "ts322 derived in a foreign scope"
		if _, err := store.UpsertBlock(ctx, pool, "ts322", title, "derived body", nil,
			map[string]any{derived.MetadataKey: map[string]any{
				"v": derived.ContractVersion, "stratum": int(derived.StratumDerived),
			}}, "foreign322", false, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed derived block: %v", err)
		}
		for _, ti := range []string{title, "ts322 nothing of that title"} {
			status, resp := post(ar, withScope("target_scope", "foreign322", chunk(ti, "MARKER-ts322-probe-order")))
			if status != http.StatusForbidden || resp["code"] != "scope_denied" {
				t.Errorf("title %q: status %d body %v, want 403 scope_denied", ti, status, resp)
			}
		}
	})

	// Chunk mode binds the source to the resolved scope (InsertChunk).
	t.Run("chunk_mode_writes_the_target_scope", func(t *testing.T) {
		ar := tsKey(t, pool, "ts322-chunk", "private", nil, nil, store.DefaultTenantID)
		src := seedSource(t, pool, "/ts322/shared.md", "shared")
		c := chunk("ts322 chunk", "MARKER-ts322-chunk")
		c["source_id"], c["chunk_index"] = src, 0
		_, resp := post(ar, withScope("target_scope", "shared", c))
		r := firstResult(t, resp)
		id, _ := r["id"].(string)
		if r["status"] != "inserted" || id == "" {
			t.Fatalf("chunk to shared = %v, want inserted", r)
		}
		if got := readChunk(t, pool, id); got.scope != "shared" {
			t.Errorf("chunk scope = %q, want shared", got.scope)
		}
		c2 := chunk("ts322 chunk home", "MARKER-ts322-chunk-home")
		c2["source_id"], c2["chunk_index"] = src, 1
		_, resp = post(ar, withScope("", nil, c2))
		if r := firstResult(t, resp); r["status"] != "failed" || r["error"] != "source not found" {
			t.Errorf("chunk to the home scope from a source in shared = %v, want source not found", r)
		}
	})
}
