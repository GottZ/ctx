//go:build integration

// Integration probes for the F2-W6 secrets API and the §3.6 master-key
// re-encrypt sweep, against a real PG18 testcontainer:
//
//   - GUARD PIN (04-W5 §5.7): DELETE on a secret referenced ONLY by a backend
//     pool row → 409 carrying the BACKEND remediation, not the settings one;
//     clearing the reference frees the delete, a second delete is 404
//   - remediation unit pin: the three-way branch of secretRefs.remediation,
//     including the mixed arm no live reference set can produce any more
//   - ROTATION PIN (04-W6 §5.8): create→rotate roundtrip and rotation metadata;
//     a rotation reaches the BACKEND POOL over the real NOTIFY funnel — API
//     rotation AND psql-shape rotation both leave the new plaintext in the pool
//     snapshot's in-memory APIKey
//   - response scan: no submitted value in ANY secrets response
//   - ReencryptSweep: prev-key rows re-seal (key_version bump, readable
//     with current alone), foreign-key rows stay untouched, no-PREV = no-op
//
// The settings-side half of all of this (secret_ref rows, the config snapshot
// carrying a resolved plaintext) died with the chat tuple in β8 — see the
// comment above BackendReferencedDelete409.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSecretsAPI -count=1 -v
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertBackendSecretRef seeds one pool row that references an F2 secret by
// name — the fixture shape of the 04-W5 delete guard. Raw SQL like
// insertBackendRoles: the guard reads ROWS, so the row only has to exist and
// carry api_key_ref; DDL defaults cover the rest.
func insertBackendSecretRef(t *testing.T, pool *pgxpool.Pool, name, scope, apiKeyRef string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_backends (name, base_url, scope, api_key_ref, roles, model_map)
		 VALUES ($1,$2,$3,$4,'{chat}','{"default":"stub-model"}') RETURNING id`,
		name, "https://"+name+".example.com/v1", scope, apiKeyRef).Scan(&id); err != nil {
		t.Fatalf("insert backend %s: %v", name, err)
	}
	return id
}

func freshKeyHexHandler(t *testing.T) string {
	t.Helper()
	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

func TestSecretsAPI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	keyHex := freshKeyHexHandler(t)
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")

	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}
	cfgStore := config.NewStore(envCfg)

	ar := &auth.AuthResult{ApiKeyID: "", HomeScope: "private",
		ReadScopes: []string{"private"}, IsValid: true, IsAdmin: true}
	newRouter := func(h *SecretsHandler, sh *SettingsHandler) *chi.Mux {
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
			})
		})
		MountSecrets(r, h)
		if sh != nil {
			MountSettings(r, sh)
		}
		return r
	}
	router := newRouter(NewSecretsHandler(pool, cfgStore), NewSettingsHandler(pool, cfgStore))
	do := func(router *chi.Mux, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	const valueA = "PROVIDER-KEY-A-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const valueB = "PROVIDER-KEY-B-" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanClean := func(rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		if strings.Contains(rec.Body.String(), valueA) || strings.Contains(rec.Body.String(), valueB) {
			t.Errorf("%s echoes a submitted secret value: %s", label, rec.Body.String())
		}
	}

	// CreateReferenceRotate_Propagation and DeleteReferenced409_ThenUnsetAndDelete
	// died with the chat tuple in β8. Both were built on the SETTINGS side of the
	// secret strecke: a config key holding a secret_ref, and the config snapshot
	// carrying the resolved plaintext (cfgStore.Snapshot().Chat.APIKey). After
	// the cut no registry key is sensitive — chat.api_key was the last secret:"fp"
	// carrier — and server.db_password, the only sensitive key left, is
	// mut:"restart", so admitOverride drops any row on it before a snapshot could
	// resolve anything. There is no key left that a settings-side secret_ref can
	// live on, hence no vehicle for either subtest.
	//
	// Where each statement lives now:
	//
	//   - create→reference→rotate→propagate: RotationPropagatesToBackendPool
	//     below. It is the STRONGER form of the same statement — the reference
	//     is a pool row's api_key_ref (where F3 actually put provider keys) and
	//     the propagation runs over the REAL NOTIFY funnel instead of the
	//     handler's own reload call. The rotation metadata assertion
	//     ("rotated_at" appears only after a rotation) moved there with it.
	//   - the "action":"create" / "action":"rotate" envelope and the
	//     no-material response scan: RotationPropagatesToBackendPool.
	//   - 409 on a referenced secret, unset-then-delete, second delete = 404:
	//     BackendReferencedDelete409 below, on the pool half.
	//
	// What is genuinely GONE is the severed-reload NEGATIVE PROBE: it asserted
	// that SecretsHandler.reload (settings.Reload) is load-bearing, by showing
	// the config snapshot keeping the OLD plaintext when the link is cut. That
	// link is still live code (sealbox.go h.reload), but it can no longer have
	// an observable effect — no config key resolves a secret, so severed and
	// production handler now produce identical snapshots. Keeping the probe
	// would have meant keeping a test that is green either way. The incident
	// path it guarded (a rotation that does not reach the serving chain) is
	// guarded pool-side instead, with its own negative probe documented on
	// RotationPropagatesToBackendPool.

	// GUARD PIN (04-W5 §5.7): a secret whose ONLY reference is a backend pool
	// row must not be deletable. Before the referencedBy union this DELETE
	// answered 200 — deleteSealed + reload dropped the plaintext and the
	// backend went keyless at the next resolver pass (chat/embed calls failing
	// or, worse, leaving without auth). The 409 text must carry the BACKEND
	// remediation: "unset the setting" is wrong for a pool row and names an
	// endpoint that retires with the registry cut.
	//
	// Negative probe (2026-08-21): with the pool half of referencedBy removed
	// (the store.BackendSecretRefsAll block in sealbox.go), this subtest fails
	// red at the first assertion — DELETE = 200 and the backend row keeps a
	// dangling api_key_ref. Restored → green.
	t.Run("BackendReferencedDelete409", func(t *testing.T) {
		const secretName = "prov-pool"
		const backendName = "pool-guard-be"

		rec := do(router, http.MethodPut, "/api/secrets/"+secretName, `{"value":"`+valueA+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		insertBackendSecretRef(t, pool, backendName, store.GlobalScope, secretName)

		// The list surfaces the pool reference under the backend: prefix.
		rec = do(router, http.MethodGet, "/api/secrets", "")
		if !strings.Contains(rec.Body.String(), `"backend:`+backendName+`"`) {
			t.Errorf("list lacks the pool reference: %s", rec.Body.String())
		}
		scanClean(rec, "GET list with pool ref")

		rec = do(router, http.MethodDelete, "/api/secrets/"+secretName, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("backend-referenced delete = %d, want 409 (fail-open §5.7)", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"backend:`+backendName+`"`) {
			t.Errorf("409 lacks the referencing pool row: %s", body)
		}
		if !strings.Contains(body, "api_key_ref") {
			t.Errorf("409 lacks the backend remediation: %s", body)
		}
		if strings.Contains(body, "/api/settings/") {
			t.Errorf("409 carries the SETTINGS remediation for a pool-only reference: %s", body)
		}
		// The guard did not just answer 409 — the row is still there.
		if exists, err := store.SecretExists(ctx, pool, secretName, store.GlobalScope); err != nil || !exists {
			t.Fatalf("secret gone despite 409 (exists=%v err=%v)", exists, err)
		}

		// The MIXED reference set (a settings row AND a pool row on the same
		// secret, both remediations in one 409) died with the chat tuple in β8.
		// referencedBy is still the UNION of both halves — untouched — but its
		// settings half is now structurally unreachable: no registry key is
		// sensitive any more, so no settings row can BE a secret_ref, so
		// secretRefs.settings is always empty in a live scan. That leaves
		// secretRefs.remediation's three-way branch reachable only in its
		// backends-only form over HTTP. The two other arms are pinned without a
		// DB in TestSecretRefsRemediationBranches below, so the branch stays
		// covered for the day a settings-side secret carrier returns.

		// Clear the reference → the delete goes through. This is the green half
		// of the guard: it blocks references, not deletes as such.
		if _, err := pool.Exec(ctx,
			`UPDATE context_backends SET api_key_ref=NULL WHERE name=$1`, backendName); err != nil {
			t.Fatalf("clear api_key_ref: %v", err)
		}
		rec = do(router, http.MethodDelete, "/api/secrets/"+secretName, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("unreferenced delete = %d %s", rec.Code, rec.Body.String())
		}
		scanClean(rec, "DELETE ok")
		// Migrated from DeleteReferenced409_ThenUnsetAndDelete: the row is
		// really gone, so a second delete is an ordinary 404.
		rec = do(router, http.MethodDelete, "/api/secrets/"+secretName, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("second delete: %d, want 404", rec.Code)
		}
	})

	// ROTATION PIN (04-W6 §3.4/§4.3/§5.8): a rotation must reach the BACKEND
	// POOL, not only the config snapshot. Backends carry no key of their own —
	// they name an F2 secret via api_key_ref, and the pool resolves it into an
	// in-memory APIKey at reload time. Before this wave nothing reloaded the
	// pool on a secret write: an operator rotating a leaked provider key saw
	// 200, and the chain kept authenticating with the OLD key until the next
	// restart or an unrelated backend-row write — the incident path was dead.
	//
	// The pin runs over the REAL funnel: a dedicated LISTEN connection takes
	// the notification the 051 trg_secrets_notify actually emitted and hands
	// exactly that to the production listener handler. The test NEVER calls
	// bp.Reload itself — a manual reload would assert the resolver, which was
	// never in doubt, and would be green even with the incident path dead.
	//
	// Negative probe (2026-08-21): with the context_secrets branch removed
	// from events/listener.go, both rotation assertions fail red (the pool
	// keeps value A) while the baseline assertion stays green. Restored → green.
	t.Run("RotationPropagatesToBackendPool", func(t *testing.T) {
		const secretName = "prov-rotate"
		const backendName = "pool-rotate-be"
		const valueC = "PROVIDER-KEY-C-" + "ccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

		// LISTEN before the first write, so no notification of this subtest can
		// be missed between the write and the wait.
		lc, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire LISTEN conn: %v", err)
		}
		defer lc.Release()
		if _, err := lc.Exec(ctx, "LISTEN ctx_settings_write"); err != nil {
			t.Fatalf("LISTEN: %v", err)
		}
		// waitEntity returns the next REAL notification carrying that entity.
		// Notifications for other entities are discarded here exactly as the
		// listener discards what it has no branch for.
		waitEntity := func(entity string) *pgconn.Notification {
			t.Helper()
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			for {
				n, err := lc.Conn().WaitForNotification(wctx)
				if err != nil {
					t.Fatalf("WaitForNotification(%s): %v", entity, err)
				}
				var p struct{ Entity string }
				if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
					t.Fatalf("notify payload %q: %v", n.Payload, err)
				}
				if p.Entity == entity {
					return n
				}
			}
		}

		// The production wiring: pool with the real secret resolver, listener
		// handler on top of it (cmd/ctxd/main.go shape, dispatcher-less).
		bp := backends.NewPool(pool, settings.BackendSecretResolver(pool))
		lh := events.NewSettingsWriteHandler(pool, cfgStore, bp, nil)
		deliver := func(entity string) {
			t.Helper()
			if err := lh.HandleNotification(ctx, waitEntity(entity), nil); err != nil {
				t.Fatalf("listener HandleNotification(%s): %v", entity, err)
			}
		}
		poolKey := func() string {
			t.Helper()
			for _, b := range bp.Snapshot() {
				if b.Name == backendName {
					return b.APIKey
				}
			}
			t.Fatalf("backend %q missing from the pool snapshot", backendName)
			return ""
		}

		rec := do(router, http.MethodPut, "/api/secrets/"+secretName, `{"value":"`+valueA+`"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"create"`) {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		scanClean(rec, "PUT create (rotation pin)")

		// Migrated from CreateReferenceRotate_Propagation: the list surfaces
		// rotation METADATA and never material. rotated_at is omitempty, so its
		// absence before the first rotation is what makes the presence check
		// after it a statement rather than a field-name grep.
		listRotatedAt := func(label string) *time.Time {
			t.Helper()
			rec := do(router, http.MethodGet, "/api/secrets", "")
			scanClean(rec, label)
			var resp struct {
				Secrets []secretView `json:"secrets"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal list: %v", err)
			}
			for _, s := range resp.Secrets {
				if s.Name == secretName {
					return s.RotatedAt
				}
			}
			t.Fatalf("secret %q missing from the list", secretName)
			return nil
		}
		if got := listRotatedAt("GET list (pre-rotation)"); got != nil {
			t.Errorf("rotated_at = %v before any rotation, want absent", got)
		}

		// Baseline over the funnel too: the row write's own notification is
		// what publishes the first snapshot. A failure here is fixture or
		// resolver, not the wave.
		insertBackendSecretRef(t, pool, backendName, store.GlobalScope, secretName)
		deliver("context_backends")
		if got := poolKey(); got != valueA {
			t.Fatalf("baseline pool APIKey = %q, want value A (resolver/fixture broken — the pin below would be meaningless)", got)
		}

		// (1) Rotation through the API handler.
		rec = do(router, http.MethodPut, "/api/secrets/"+secretName, `{"value":"`+valueB+`"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"rotate"`) {
			t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
		}
		scanClean(rec, "PUT rotate (rotation pin)")
		if got := listRotatedAt("GET list (post-rotation)"); got == nil {
			t.Errorf("list lacks rotation metadata after a rotation")
		}
		deliver("context_secrets")
		if got := poolKey(); got != valueB {
			t.Errorf("pool APIKey after API rotation = %q, want value B — the rotation never reached the serving chain", got)
		}

		// (2) psql-shape rotation: sealed row written straight to
		// context_secrets, no handler and no settings.Reload in between. Same
		// trigger, same branch, same assertion.
		{
			box, err := sealbox.New(keyHex, "")
			if err != nil {
				t.Fatalf("sealbox: %v", err)
			}
			nonce, ciphertext, err := box.Seal(secretName, store.GlobalScope, []byte(valueC))
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := store.PutSecret(ctx, tx, secretName, store.GlobalScope, nonce, ciphertext, 1, nil); err != nil {
				t.Fatalf("put secret: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		deliver("context_secrets")
		if got := poolKey(); got != valueC {
			t.Errorf("pool APIKey after psql rotation = %q, want value C — the funnel misses direct DB writes", got)
		}

		// Leave no dangling reference behind for the sweep subtest.
		if _, err := pool.Exec(ctx,
			`UPDATE context_backends SET api_key_ref=NULL WHERE name=$1`, backendName); err != nil {
			t.Fatalf("clear api_key_ref: %v", err)
		}
	})

	t.Run("ReencryptSweep", func(t *testing.T) {
		// Row sealed under key A (the future PREV), row sealed under a LOST
		// key C, and a row sealed under the future current key B.
		keyA, keyB, keyC := keyHex, freshKeyHexHandler(t), freshKeyHexHandler(t)
		boxA, _ := sealbox.New(keyA, "")
		boxB, _ := sealbox.New(keyB, "")
		boxC, _ := sealbox.New(keyC, "")
		put := func(box *sealbox.Box, name, plain string) {
			t.Helper()
			nonce, ct, err := box.Seal(name, store.GlobalScope, []byte(plain))
			if err != nil {
				t.Fatalf("seal %s: %v", name, err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := store.PutSecret(ctx, tx, name, store.GlobalScope, nonce, ct, 1, nil); err != nil {
				t.Fatalf("put %s: %v", name, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		put(boxA, "sweep-prev", "plain-prev")
		put(boxC, "sweep-lost", "plain-lost")
		put(boxB, "sweep-current", "plain-current")

		// No PREV set → no-op (key_version stays 1 everywhere).
		t.Setenv(sealbox.EnvKey, keyB)
		t.Setenv(sealbox.EnvKeyPrev, "")
		settings.ReencryptSweep(ctx, pool)
		var v int
		if err := pool.QueryRow(ctx,
			`SELECT key_version FROM context_secrets WHERE name='sweep-prev'`).Scan(&v); err != nil {
			t.Fatalf("query: %v", err)
		}
		if v != 1 {
			t.Fatalf("sweep ran without PREV set (key_version=%d)", v)
		}

		// PREV=A: sweep-prev re-seals (version 2, current-only readable),
		// sweep-lost stays untouched, sweep-current stays version 1.
		t.Setenv(sealbox.EnvKeyPrev, keyA)
		settings.ReencryptSweep(ctx, pool)

		rows, err := store.LoadSealedSecrets(ctx, pool)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		byName := map[string]store.SealedSecret{}
		for _, r := range rows {
			byName[r.Name] = r
		}
		if byName["sweep-prev"].KeyVersion != 2 {
			t.Errorf("sweep-prev key_version = %d, want 2", byName["sweep-prev"].KeyVersion)
		}
		if byName["sweep-current"].KeyVersion != 1 {
			t.Errorf("sweep-current key_version = %d, want 1 (already current)", byName["sweep-current"].KeyVersion)
		}
		if byName["sweep-lost"].KeyVersion != 1 {
			t.Errorf("sweep-lost key_version = %d, want 1 (untouched)", byName["sweep-lost"].KeyVersion)
		}
		// Readable with current ALONE (no prev loaded) and value preserved.
		boxBOnly, _ := sealbox.New(keyB, "")
		got := byName["sweep-prev"]
		plain, usedPrev, err := boxBOnly.Open(got.Name, got.Scope, got.Nonce, got.Ciphertext)
		if err != nil || usedPrev || string(plain) != "plain-prev" {
			t.Errorf("re-sealed row: plain=%q usedPrev=%v err=%v, want plain-prev/false/nil", plain, usedPrev, err)
		}
		// The lost row is bytes-identical readable by its own key (no data loss).
		lost := byName["sweep-lost"]
		plain, _, err = boxC.Open(lost.Name, lost.Scope, lost.Nonce, lost.Ciphertext)
		if err != nil || string(plain) != "plain-lost" {
			t.Errorf("lost row mutated: plain=%q err=%v, want plain-lost/nil", plain, err)
		}
		// Idempotence: a second sweep changes nothing further.
		settings.ReencryptSweep(ctx, pool)
		if err := pool.QueryRow(ctx,
			`SELECT key_version FROM context_secrets WHERE name='sweep-prev'`).Scan(&v); err != nil {
			t.Fatalf("query: %v", err)
		}
		if v != 2 {
			t.Errorf("second sweep bumped key_version to %d — not idempotent", v)
		}
	})
}
