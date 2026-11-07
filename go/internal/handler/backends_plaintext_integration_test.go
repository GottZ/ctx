//go:build integration

// B24 / K-04-6 (design/01 §5, F-W5): a backend write whose base_url is plain
// http:// and whose api_key_ref resolves to a ctx model key (ctxm_) needs
// confirm_plaintext_credential:true — the key would travel unencrypted on
// every call. Both provider classes, both tiers, create and update.
//
//	go test -tags=integration -p 1 -count=1 -run TestBackendPlaintextModelKeyConfirm -v ./internal/handler/
package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

func TestBackendPlaintextModelKeyConfirm(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ph := setupProfileHarness(t)
	keyHex := freshKeyHexHandler(t)
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")
	box, err := sealbox.New(keyHex, "")
	if err != nil {
		t.Fatalf("sealbox: %v", err)
	}
	seal := func(name, scope, plain string) {
		t.Helper()
		nonce, ct, err := box.Seal(name, scope, []byte(plain))
		if err != nil {
			t.Fatalf("seal %s: %v", name, err)
		}
		tx, err := ph.pool.Begin(ph.ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ph.ctx) }()
		if _, err := store.PutSecret(ph.ctx, tx, name, scope, nonce, ct, 1, nil); err != nil {
			t.Fatalf("put %s/%s: %v", name, scope, err)
		}
		if err := tx.Commit(ph.ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	newKey := func() modelkey.Key {
		t.Helper()
		k, err := modelkey.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		return k
	}
	peerKey, otherKey, tenantKey, bomKey := newKey(), newKey(), newKey(), newKey()
	seal("peer-key", store.GlobalScope, peerKey.Plaintext())
	seal("other-peer-key", store.GlobalScope, otherKey.Plaintext())
	seal("provider-key", store.GlobalScope, "sk-or-v1-not-a-model-key")
	seal("bom-key", store.GlobalScope, "\ufeff "+bomKey.Plaintext())
	seal("tenant-peer-key", "tenant-a", tenantKey.Plaintext())

	// Every log line of the test lands here; the plaintext checks read it.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// forbidden: the value, the secret name, the value after the prefix and
	// the hint of every model key in play.
	type secret struct {
		name string
		key  modelkey.Key
	}
	secrets := []secret{{"peer-key", peerKey}, {"other-peer-key", otherKey}, {"tenant-peer-key", tenantKey}, {"bom-key", bomKey}}
	plaintextFree := func(t *testing.T, where, text string, withName bool) {
		t.Helper()
		for _, s := range secrets {
			p := s.key.Plaintext()
			for _, bad := range []string{p, strings.TrimPrefix(p, modelkey.Prefix), s.key.Hint()} {
				if strings.Contains(text, bad) {
					t.Errorf("%s carries secret material %q of %s: %s", where, bad, s.name, text)
				}
			}
			if withName && strings.Contains(text, s.name) {
				t.Errorf("%s names the secret %q: %s", where, s.name, text)
			}
		}
	}

	spec := func(name, baseURL, ref string, extra map[string]any) map[string]any {
		d := map[string]any{
			"name": name, "base_url": baseURL, "roles": []string{"chat"},
			"model_map": map[string]any{"default": "peer-model"}, "api_key_ref": ref,
		}
		for k, v := range extra {
			d[k] = v
		}
		return map[string]any{"action": "backend-create", "data": d}
	}
	rowCount := func(name string) int {
		var n int
		if err := ph.pool.QueryRow(ph.ctx, `SELECT count(*) FROM context_backends WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		return n
	}
	rowID := func(name string) string {
		var id string
		if err := ph.pool.QueryRow(ph.ctx, `SELECT id FROM context_backends WHERE name = $1`, name).Scan(&id); err != nil {
			t.Fatalf("id %s: %v", name, err)
		}
		return id
	}
	wantConfirm := func(t *testing.T, what string, code int, body string) {
		t.Helper()
		t.Logf("%s: %d %s", what, code, body)
		if code != http.StatusBadRequest || !strings.Contains(body, "confirm_plaintext_credential") {
			t.Fatalf("%s = %d %s, want 400 naming confirm_plaintext_credential", what, code, body)
		}
		plaintextFree(t, what+" body", body, true)
	}
	wantOK := func(t *testing.T, what string, code int, body string, wantWarn bool) {
		t.Helper()
		t.Logf("%s: %d %s", what, code, body)
		if code != http.StatusOK {
			t.Fatalf("%s = %d %s, want 200", what, code, body)
		}
		var resp struct {
			Warnings []string `json:"warnings"`
		}
		_ = json.Unmarshal([]byte(body), &resp)
		got := false
		for _, w := range resp.Warnings {
			if strings.Contains(w, "plain http://") {
				got = true
				plaintextFree(t, what+" warning", w, true)
			}
		}
		if got != wantWarn {
			t.Errorf("%s: plaintext warning present = %v, want %v (warnings %v)", what, got, wantWarn, resp.Warnings)
		}
		plaintextFree(t, what+" body", body, false)
	}

	t.Run("create_generic_http_needs_confirm", func(t *testing.T) {
		rec := ph.doRaw(ph.admin, spec("peer-generic", "http://10.0.0.5:8077", "peer-key", nil))
		wantConfirm(t, "create without confirm", rec.Code, rec.Body.String())
		if n := rowCount("peer-generic"); n != 0 {
			t.Fatalf("row written despite 400: %d", n)
		}
		rec = ph.doRaw(ph.admin, spec("peer-generic", "http://10.0.0.5:8077", "peer-key", map[string]any{"confirm_plaintext_credential": true}))
		wantOK(t, "create with confirm", rec.Code, rec.Body.String(), true)
	})
	t.Run("create_ctx_http_needs_confirm", func(t *testing.T) {
		ctxRow := map[string]any{"provider_class": "ctx", "protocol": "openai"}
		rec := ph.doRaw(ph.admin, spec("peer-ctx", "http://10.0.0.7:8077", "peer-key", ctxRow))
		wantConfirm(t, "ctx create without confirm", rec.Code, rec.Body.String())
		ctxRow["confirm_plaintext_credential"] = true
		rec = ph.doRaw(ph.admin, spec("peer-ctx", "http://10.0.0.7:8077", "peer-key", ctxRow))
		wantOK(t, "ctx create with confirm", rec.Code, rec.Body.String(), true)
	})
	t.Run("loopback_http_needs_confirm", func(t *testing.T) {
		rec := ph.doRaw(ph.admin, spec("peer-loop", "http://127.0.0.1:8077", "peer-key", nil))
		wantConfirm(t, "loopback create without confirm", rec.Code, rec.Body.String())
		rec = ph.doRaw(ph.admin, spec("peer-localhost", "http://localhost:8077", "peer-key", nil))
		wantConfirm(t, "localhost create without confirm", rec.Code, rec.Body.String())
	})
	t.Run("invisible_prefix_still_a_model_key", func(t *testing.T) {
		rec := ph.doRaw(ph.admin, spec("peer-bom", "http://10.0.0.8:8077", "bom-key", nil))
		wantConfirm(t, "BOM-prefixed key without confirm", rec.Code, rec.Body.String())
	})
	t.Run("https_model_key_needs_no_confirm", func(t *testing.T) {
		rec := ph.doRaw(ph.admin, spec("peer-tls", "https://10.0.0.6:8077", "peer-key", nil))
		wantOK(t, "https create", rec.Code, rec.Body.String(), false)
	})
	t.Run("http_provider_key_needs_no_confirm", func(t *testing.T) {
		rec := ph.doRaw(ph.admin, spec("provider-http", "http://10.0.0.9:8077", "provider-key", nil))
		wantOK(t, "http + non-model-key create", rec.Code, rec.Body.String(), false)
	})
	t.Run("update_https_to_http_needs_confirm", func(t *testing.T) {
		id := rowID("peer-tls")
		rec := ph.doID(ph.admin, "backend-update", id, map[string]any{"base_url": "http://10.0.0.6:8077"})
		wantConfirm(t, "update to http without confirm", rec.Code, rec.Body.String())
		var url string
		if err := ph.pool.QueryRow(ph.ctx, `SELECT base_url FROM context_backends WHERE id = $1`, id).Scan(&url); err != nil || url != "https://10.0.0.6:8077" {
			t.Fatalf("base_url after the 400 = %q (err %v), want https untouched", url, err)
		}
		rec = ph.doID(ph.admin, "backend-update", id, map[string]any{"base_url": "http://10.0.0.6:8077", "confirm_plaintext_credential": true})
		wantOK(t, "update to http with confirm", rec.Code, rec.Body.String(), true)
	})
	t.Run("update_of_a_confirmed_row", func(t *testing.T) {
		id := rowID("peer-generic")
		// Neither target nor key changes: no new exposure, no confirm (the
		// trust/data-collection doctrine: confirm when the edge arms).
		rec := ph.doID(ph.admin, "backend-update", id, map[string]any{"priority": 42})
		wantOK(t, "re-save without confirm", rec.Code, rec.Body.String(), false)
		// The brake is never gated.
		rec = ph.doID(ph.admin, "backend-update", id, map[string]any{"enabled": false})
		wantOK(t, "brake", rec.Code, rec.Body.String(), false)
		// Moving the key to another plaintext host, or another model key onto
		// this host, is a new exposure.
		rec = ph.doID(ph.admin, "backend-update", id, map[string]any{"base_url": "http://10.0.0.15:8077"})
		wantConfirm(t, "re-host without confirm", rec.Code, rec.Body.String())
		rec = ph.doID(ph.admin, "backend-update", id, map[string]any{"api_key_ref": "other-peer-key"})
		wantConfirm(t, "other model key without confirm", rec.Code, rec.Body.String())
		rec = ph.doID(ph.admin, "backend-update", id, map[string]any{"api_key_ref": "other-peer-key", "confirm_plaintext_credential": true})
		wantOK(t, "other model key with confirm", rec.Code, rec.Body.String(), true)
	})
	t.Run("tenant_admin_same_rule", func(t *testing.T) {
		rec := ph.doRaw(ph.ten, spec("tenant-peer", "http://peer.example.com", "tenant-peer-key", nil))
		wantConfirm(t, "tenant create without confirm", rec.Code, rec.Body.String())
		rec = ph.doRaw(ph.ten, spec("tenant-peer", "http://peer.example.com", "tenant-peer-key", map[string]any{"confirm_plaintext_credential": true}))
		wantOK(t, "tenant create with confirm", rec.Code, rec.Body.String(), true)
	})
	t.Run("log_is_plaintext_free", func(t *testing.T) {
		logs := logBuf.String()
		if !strings.Contains(logs, "confirmed plaintext model-key transport") {
			t.Errorf("no WARN for a confirmed plaintext write in the log:\n%s", logs)
		}
		// Value, remainder and hint nowhere in the log; the secret name not in
		// a line of this check (the pool's own keyless WARN names its ref —
		// unchanged behaviour, outside B24).
		plaintextFree(t, "log", logs, false)
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "plaintext") {
				plaintextFree(t, "log line", line, true)
			}
		}
	})
}
