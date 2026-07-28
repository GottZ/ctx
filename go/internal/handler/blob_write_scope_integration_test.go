//go:build integration

// Wave B3 (Gap-C0-c): /api/blob/store must resolve its write scope through the
// SAME formula as the block write path.
//
// Ist-Stand before B3 (blob.go, "Scope validation") carried a SECOND, narrower
// formula of its own:
//
//	writeScope := home_scope
//	req.Scope != "" ⇒ home_scope | 'shared'-if-in-allowed_scopes | 403
//
// ar.WriteScopes never entered it. A key minted with write_scopes=[b] could
// create, update and delete BLOCKS in b — writableBlockScopes is the single
// eval point of the block write gate (078/E4b) — while every blob write to
// that same scope came back 403: one principal, two authorisation answers for
// one scope, and the narrower one silently attached to the blob surface.
//
// B3 deletes the second formula. The blob write scope is writableBlockScopes(ar),
// verbatim, so the two surfaces cannot drift apart again. The probes carry
// their brief letters:
//
//	a  WriteScopeIsHonoured — write_scopes=[b] ⇒ scope=b stores in b
//	                          (RED pre-B3: 403, the formula ignored WriteScopes)
//	b  ForeignScopeRefused  — scope=c ⇒ 403 AND write_scopes=[] ⇒ scope=b ⇒ 403
//	                          (the second half is RED against an allowed_scopes-only fix)
//	c  Golden               — omitted / home_scope / shared behave exactly as before
//	d  GateBeforeBudget     — a refused scope books no blob-write row (B2 ordering)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestBlobWriteScope -count=1 -v
package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBlobWriteScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// b3Key mints a REAL context_api_keys row (context_access_log.api_key_id is
	// FK-bound and B2 books the metering row synchronously) and returns an
	// AuthResult whose scope sets are the PROBE's, not the row's: home/allowed/
	// write are what the handler reads, and only the key id has to exist.
	b3Key := func(t *testing.T, label, home string, allowed, write []string) (string, *auth.AuthResult) {
		t.Helper()
		row, _, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create api key %q: %v", label, err)
		}
		return row.ID, &auth.AuthResult{
			ApiKeyID:      row.ID,
			IsValid:       true,
			HomeScope:     home,
			AllowedScopes: allowed,
			ReadScopes:    append([]string{home}, allowed...),
			WriteScopes:   write,
		}
	}

	// b3Handler wires a handler with the blob budget DISABLED (0), so a probe
	// that expects 403 can never be answered by a 429 instead.
	b3Handler := func() *BlobHandler {
		return NewBlobHandler(pool, staticConfigStore{cfg: &config.Config{}})
	}

	// storedScope reads the scope the row actually landed in — the response
	// echoes the handler's own variable, the table echoes what was written.
	storedScope := func(t *testing.T, category, title string) string {
		t.Helper()
		var scope string
		if err := pool.QueryRow(ctx,
			`SELECT scope FROM context_blobs WHERE category = $1 AND title = $2`,
			category, title).Scan(&scope); err != nil {
			t.Fatalf("read stored scope of (%s,%s): %v", category, title, err)
		}
		return scope
	}

	// scopedPayload is blobPayload plus an explicit scope field.
	scopedPayload := func(category, title, filename, scope string, data []byte) map[string]any {
		p := blobPayload(category, title, filename, "application/octet-stream", data)
		p["scope"] = scope
		return p
	}

	t.Run("a_WriteScopeIsHonoured", func(t *testing.T) {
		// home=a, allowed=[a,b], write=[b] — writableBlockScopes yields [a,b],
		// so this key writes BLOCKS in b. Pre-B3 the blob formula knew only
		// home and 'shared' ⇒ 403.
		_, ar := b3Key(t, "b3-write-scope", "b3a", []string{"b3a", "b3b"}, []string{"b3b"})
		h := b3Handler()

		code, resp := postBlobStore(t, h, ar,
			scopedPayload("reference", "b3-write-scope", "ws.bin", "b3b", []byte("write-scope")))
		if code != http.StatusOK {
			t.Fatalf("blob-store with scope=b3b (write_scopes=[b3b]) status = %d, want 200 (body %v) — "+
				"the blob write scope must be writableBlockScopes(ar), the same set the block write gate uses", code, resp)
		}
		if got := storedScope(t, "reference", "b3-write-scope"); got != "b3b" {
			t.Errorf("blob stored in scope %q, want b3b — a fix that accepts the request but writes home_scope "+
				"would pass the status check and land the payload in the wrong scope", got)
		}
	})

	t.Run("b_ForeignScopeRefused", func(t *testing.T) {
		h := b3Handler()

		// b1: a scope the key holds NEITHER a read nor a write right for.
		_, ar := b3Key(t, "b3-foreign", "b3a", []string{"b3a", "b3b"}, []string{"b3b"})
		code, resp := postBlobStore(t, h, ar,
			scopedPayload("reference", "b3-foreign", "foreign.bin", "b3c", []byte("nope")))
		if code != http.StatusForbidden {
			t.Fatalf("blob-store with an unheld scope status = %d, want 403 (body %v)", code, resp)
		}

		// b2: READ right without a WRITE right. This is the discriminator
		// against the lazy fix "just check AllowedScopes": that variant would
		// answer 200 here and hand every read-only grant a write channel.
		_, roAR := b3Key(t, "b3-readonly", "b3a", []string{"b3a", "b3b"}, nil)
		code, resp = postBlobStore(t, h, roAR,
			scopedPayload("reference", "b3-readonly", "readonly.bin", "b3b", []byte("nope")))
		if code != http.StatusForbidden {
			t.Fatalf("blob-store with a read-only scope status = %d, want 403 (body %v) — "+
				"allowed_scopes alone is a READ right; only write_scopes ∩ (allowed ∪ home) may write", code, resp)
		}

		// b3: a STALE write_scope, dropped from allowed_scopes by a later
		// shrink, must fall out of the intersection — enforcement path (b) of
		// the double invariant, inherited verbatim from writableBlockScopes.
		_, staleAR := b3Key(t, "b3-stale", "b3a", []string{"b3a"}, []string{"b3b"})
		code, resp = postBlobStore(t, h, staleAR,
			scopedPayload("reference", "b3-stale", "stale.bin", "b3b", []byte("nope")))
		if code != http.StatusForbidden {
			t.Fatalf("blob-store with a stale write_scope status = %d, want 403 (body %v) — "+
				"a write_scope outside allowed_scopes ∪ home must fail closed", code, resp)
		}

		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE scope IN ('b3c','b3b') AND title LIKE 'b3-%'
			   AND title <> 'b3-write-scope'`).Scan(&rows); err != nil {
			t.Fatalf("count refused writes: %v", err)
		}
		if rows != 0 {
			t.Errorf("%d refused blob write(s) reached the table, want 0", rows)
		}
	})

	t.Run("c_Golden", func(t *testing.T) {
		h := b3Handler()

		// An omitted scope still defaults to home_scope.
		_, ar := b3Key(t, "b3-golden-home", "b3home", []string{"shared"}, nil)
		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b3-golden-default", "default.bin", "application/octet-stream", []byte("d")))
		if code != http.StatusOK {
			t.Fatalf("blob-store without a scope status = %d, want 200 (body %v)", code, resp)
		}
		if got := storedScope(t, "reference", "b3-golden-default"); got != "b3home" {
			t.Errorf("omitted scope stored in %q, want the home scope b3home", got)
		}

		// An explicit home_scope stays writable even though it is in NEITHER
		// allowed_scopes nor write_scopes — it is element [0] of the formula.
		code, resp = postBlobStore(t, h, ar,
			scopedPayload("reference", "b3-golden-explicit", "explicit.bin", "b3home", []byte("e")))
		if code != http.StatusOK {
			t.Fatalf("blob-store with the explicit home scope status = %d, want 200 (body %v)", code, resp)
		}
		if got := storedScope(t, "reference", "b3-golden-explicit"); got != "b3home" {
			t.Errorf("explicit home scope stored in %q, want b3home", got)
		}

		// 'shared' stays writable exactly while it is in allowed_scopes.
		code, resp = postBlobStore(t, h, ar,
			scopedPayload("reference", "b3-golden-shared", "shared.bin", "shared", []byte("s")))
		if code != http.StatusOK {
			t.Fatalf("blob-store with scope=shared (shared in allowed) status = %d, want 200 (body %v)", code, resp)
		}
		if got := storedScope(t, "reference", "b3-golden-shared"); got != "shared" {
			t.Errorf("shared write stored in %q, want shared", got)
		}

		_, noShared := b3Key(t, "b3-golden-noshared", "b3home2", nil, nil)
		code, resp = postBlobStore(t, h, noShared,
			scopedPayload("reference", "b3-golden-noshared", "noshared.bin", "shared", []byte("s")))
		if code != http.StatusForbidden {
			t.Fatalf("blob-store with scope=shared (shared NOT in allowed) status = %d, want 403 (body %v)", code, resp)
		}
	})

	t.Run("d_GateBeforeBudget", func(t *testing.T) {
		// B2 books the blob-write intent SYNCHRONOUSLY before the upsert. A
		// scope the key may not write must be refused BEFORE that booking: a
		// rejected request must not cost budget, or an attacker spraying
		// foreign scopes drains a legitimate key's quota.
		keyID, ar := b3Key(t, "b3-order", "b3a", []string{"b3a"}, nil)
		h := NewBlobHandler(pool, staticConfigStore{cfg: &config.Config{
			Pool: config.PoolConfig{BlobRateLimitWrite: 5},
		}})

		code, resp := postBlobStore(t, h, ar,
			scopedPayload("reference", "b3-order", "order.bin", "b3z", []byte("nope")))
		if code != http.StatusForbidden {
			t.Fatalf("blob-store with an unheld scope status = %d, want 403 (body %v)", code, resp)
		}

		var booked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = 'blob-write'`, keyID).Scan(&booked); err != nil {
			t.Fatalf("count blob-write rows: %v", err)
		}
		if booked != 0 {
			t.Fatalf("a refused scope booked %d blob-write row(s), want 0 — the scope gate must run "+
				"BEFORE meterBlobWrite", booked)
		}
	})
}
