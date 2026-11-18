//go:build integration

// Issue #253: the embed backfill inside POST /api/query applied the
// sensitivity floor of the tenant whose query triggered it, also to a block
// of another tenant it picked. pool.scope_sensitivity_floor is
// tenant-overridable, so a floor only the owning tenant set was not applied
// on this path, and the block could reach an embed backend below it. The
// background arm resolves the floor per owner; the request path now does as
// well, and keeps the caller's floor next to it (backfillFloor).
//
// Every probe drives the real request path over the fixture of issue #199
// (bfsSetup: the caller is a key of the default tenant, the foreign tenant
// owns the scope bfsforeign). The floors are real settings rows read through
// the production tenant overlay (settings.TenantOverlay), the embed backend
// takes everything up to personal (no-credentials), and a floor of
// credentials therefore keeps a block away from it. What the backend
// received is read from the wire, next to which block has a vector.
//
// RED before the fix: owner_floor_arm_off and owner_floor_granted_scope (the
// foreign block reaches the backend). The others pass before and after: the
// control that the fixture reaches the foreign block at all, that the
// caller's own floor still applies, and that a floor counts only for a scope
// its tenant owns.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryBackfillOwnerFloor -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
)

// ownfConfigStore is the production config.Store with the production tenant
// overlay (SnapshotForTenant resolves a tenant's own settings rows), whose
// request snapshot is the caller's generation: the caller is a key of the
// default tenant, so without a request-scope hook that is the base.
type ownfConfigStore struct {
	*config.Store
	request *config.Config
}

func (s ownfConfigStore) SnapshotForRequest(context.Context) *config.Config { return s.request }

// ownfRecorder records every embed input the backend receives.
type ownfRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	inputs []string
}

func ownfNewRecorder(t *testing.T) *ownfRecorder {
	t.Helper()
	r := &ownfRecorder{}
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path != "/api/embed" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message":    map[string]any{"role": "assistant", "content": "database status"},
				"eval_count": 1, "prompt_eval_count": 1,
			})
			return
		}
		var body struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		in := ""
		switch v := body.Input.(type) {
		case string:
			in = v
		case []any:
			if len(v) > 0 {
				in, _ = v[0].(string)
			}
		}
		r.mu.Lock()
		r.inputs = append(r.inputs, in)
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *ownfRecorder) received(title string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, in := range r.inputs {
		if strings.Contains(in, title) {
			return true
		}
	}
	return false
}

// ownfSetFloor writes a pool.scope_sensitivity_floor row at configScope
// through the settings store's write function.
func ownfSetFloor(t *testing.T, f *bfsFixture, configScope string, floor map[string]string) {
	t.Helper()
	// A scope_floor value is stored as a JSON string holding the map, the
	// form the settings API writes.
	inner, _ := json.Marshal(floor)
	raw, _ := json.Marshal(string(inner))
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, scopeFloorSettingKey, configScope, raw, nil); err != nil {
		t.Fatalf("write floor at %s: %v", configScope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// ownfQuery runs one POST /api/query under keyCtx with the request config
// request (its floor is the caller's), against an embed backend of trust
// no-credentials, with sync_cap 4 and the background arm on or off.
func ownfQuery(t *testing.T, f *bfsFixture, rec *ownfRecorder, keyCtx context.Context, callerFloor config.ScopeFloor, armOn bool) {
	t.Helper()
	base := bw2Config()
	base.EmbedBackfill.SyncCap = 4
	if armOn {
		base.EmbedBackfill.Interval = 15 * time.Second
	}
	request := *base
	request.Pool.ScopeSensitivityFloor = callerFloor
	cs := config.NewStore(base)
	cs.SetOverlay(settings.TenantOverlay(f.pool))

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "of-embed", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustNoCredentials, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
		{ID: "t", Name: "of-chat", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-chat", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleTranslate}},
	})
	h := NewQueryHandler(f.pool, ownfConfigStore{Store: cs, request: &request}, bpool, nil, f.reg, snapshotTestAdmitter(t))
	// The question itself is public: the embed backend takes no credentials,
	// and an unset query sensitivity counts as credentials.
	raw, _ := json.Marshal(map[string]any{"query": bw2GoldenQuery, "synthesize": false, "sensitivity": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(string(raw))).WithContext(keyCtx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleQuery(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("query: status %d, body %s", w.Code, w.Body.String())
	}
}

// ownfGrantedCaller grants the caller's tenant (the default one) read on the
// foreign scope and returns a key of it authenticated after the grant, so
// its read scopes carry the foreign scope.
func ownfGrantedCaller(t *testing.T, f *bfsFixture) context.Context {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateTenantGrant(ctx, f.pool, store.DefaultTenantID, bfsForeignScope, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	_, plain, err := store.CreateApiKey(ctx, f.pool, "of-granted", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := auth.Authenticate(ctx, f.pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	if !contains(ar.ReadScopes, bfsForeignScope) {
		t.Fatalf("premise: the granted key reads %v, want %s among them", ar.ReadScopes, bfsForeignScope)
	}
	return context.WithValue(ctx, authResultKey, ar)
}

func TestQueryBackfillOwnerFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	credentials := string(backends.SensCredentials)
	// The tenant overlay builds a tenant's generation from the environment
	// and its rows, and the environment has to carry what the build requires.
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")

	// The fixture reaches the foreign block: without any floor the old end
	// (arm off) embeds it. Every probe below is about a floor, not the pick.
	t.Run("control_no_floor", func(t *testing.T) {
		f, rec := bfsSetup(t), ownfNewRecorder(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		ownfQuery(t, f, rec, f.callerCtx, nil, false)
		if !f.embedded(t, bfsForeignTitle) || !rec.received(bfsForeignTitle) {
			t.Fatal("premise: without a floor the old end embeds the foreign block")
		}
	})

	// The owner floors its scope; the caller has no floor. With the arm off
	// the old end is not bound to the caller and picks the foreign block.
	t.Run("owner_floor_arm_off", func(t *testing.T) {
		f, rec := bfsSetup(t), ownfNewRecorder(t)
		ownfSetFloor(t, f, bfsForeignScope, map[string]string{bfsForeignScope: credentials})
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)
		ownfQuery(t, f, rec, f.callerCtx, nil, false)
		if rec.received(bfsForeignTitle) || f.embedded(t, bfsForeignTitle) {
			t.Error("the backend received a block its owner floored to credentials, on the floor of the caller")
		}
		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the caller's own block is still pending: the backfill did not run")
		}
	})

	// The same through a read scope another tenant granted, with the arm on:
	// the pick stays in the caller's read scopes, and the foreign scope is
	// one of them.
	t.Run("owner_floor_granted_scope", func(t *testing.T) {
		f, rec := bfsSetup(t), ownfNewRecorder(t)
		ownfSetFloor(t, f, bfsForeignScope, map[string]string{bfsForeignScope: credentials})
		granted := ownfGrantedCaller(t, f)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)
		ownfQuery(t, f, rec, granted, nil, true)
		if rec.received(bfsForeignTitle) || f.embedded(t, bfsForeignTitle) {
			t.Error("the backend received a block of a granted scope its owner floored to credentials")
		}
		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the caller's own block is still pending: the backfill did not run")
		}
	})

	// The caller's own floor on the foreign scope still applies next to the
	// owner's lower one (the fold takes the higher): the request does not
	// send what it did not send before.
	t.Run("caller_floor_kept", func(t *testing.T) {
		f, rec := bfsSetup(t), ownfNewRecorder(t)
		ownfSetFloor(t, f, bfsForeignScope, map[string]string{bfsForeignScope: string(backends.SensInternal)})
		f.write(t, f.foreignCtx, bfsForeignTitle)
		ownfQuery(t, f, rec, f.callerCtx, config.ScopeFloor{bfsForeignScope: backends.SensCredentials}, false)
		if rec.received(bfsForeignTitle) || f.embedded(t, bfsForeignTitle) {
			t.Error("the caller's floor on the scope no longer applies")
		}
	})

	// A floor counts only for a scope its tenant owns: the foreign tenant
	// flooring `private` (the caller's scope, owned by the default tenant)
	// does not hold the caller's block back. Both arm modes: with the arm on
	// only the owners of the caller's read scopes are read, with the arm off
	// every configured floor is, the foreign one among them.
	for _, armOn := range []bool{true, false} {
		name := "floor_of_a_scope_the_tenant_does_not_own_arm_off"
		if armOn {
			name = "floor_of_a_scope_the_tenant_does_not_own_arm_on"
		}
		t.Run(name, func(t *testing.T) {
			f, rec := bfsSetup(t), ownfNewRecorder(t)
			ownfSetFloor(t, f, bfsForeignScope, map[string]string{"private": credentials})
			f.write(t, f.callerCtx, bfsCallerTitle)
			ownfQuery(t, f, rec, f.callerCtx, nil, armOn)
			if !f.embedded(t, bfsCallerTitle) {
				t.Error("a tenant's floor on a scope it does not own held back the owner's block")
			}
		})
	}
}
