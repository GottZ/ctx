//go:build integration

// Gap-C0-a (RC-1 W-B1), handler side: POST /api/blob/store collapsed EVERY
// UpsertBlob failure into a bare 500 "Internal server error". A constraint
// violation is a property of the REQUEST, not of the server, so class-23
// SQLSTATEs must surface as 409 (uniqueness) / 422 (every other integrity
// violation) with the offending constraint named; everything else stays 500.
//
// store.UpsertBlob takes the *pgxpool.Pool directly — there is no injectable
// store seam — so the violations are induced in the throwaway container by
// adding a real constraint the request then trips. The non-class-23 probe
// needs no such fixture: a write scope longer than context_blobs.scope
// (VARCHAR(50), migration 058) raises 22001 straight through the normal path.
//
//	go test -tags=integration ./internal/handler/ -run TestBlobStoreHandler -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// blobAR is a plain, valid private-scope key.
func blobAR(homeScope string) *auth.AuthResult {
	return &auth.AuthResult{
		ApiKeyID:      "11111111-2222-7333-8444-555555555555",
		IsValid:       true,
		HomeScope:     homeScope,
		AllowedScopes: []string{},
		ReadScopes:    []string{homeScope},
	}
}

// postBlobStore posts a blob-store payload with the given AuthResult injected
// and returns the HTTP status + decoded body.
func postBlobStore(t *testing.T, h *BlobHandler, ar *auth.AuthResult, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/blob/store", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleBlobStore(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, resp
}

// blobPayload builds a minimally valid blob-store request body.
func blobPayload(category, title, filename, mimeType string, data []byte) map[string]any {
	return map[string]any{
		"file":      base64.StdEncoding.EncodeToString(data),
		"filename":  filename,
		"category":  category,
		"title":     title,
		"mime_type": mimeType,
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("fixture %q: %v", sql, err)
	}
}

func TestBlobStoreHandler_ConstraintViolationsAreDifferentiated(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewBlobHandler(pool, staticConfigStore{cfg: &config.Config{}})
	ar := blobAR("private")

	t.Run("valid write succeeds", func(t *testing.T) {
		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b1-handler-ok", "ok.bin", "application/octet-stream", []byte("blob-b1-ok")))
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", code, resp)
		}
		blob, ok := resp["blob"].(map[string]any)
		if !ok {
			t.Fatalf("response has no blob object: %v", resp)
		}
		if blob["checksum"] == "" || blob["checksum"] == nil {
			t.Errorf("blob.checksum missing: %v", blob)
		}
	})

	t.Run("unique violation is 409 with the constraint named", func(t *testing.T) {
		mustExec(t, pool, `CREATE UNIQUE INDEX uq_blob_probe_filename ON context_blobs (filename)`)
		t.Cleanup(func() { mustExec(t, pool, `DROP INDEX uq_blob_probe_filename`) })

		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b1-dup-a", "dup.bin", "application/octet-stream", []byte("first")))
		if code != http.StatusOK {
			t.Fatalf("seed status = %d, want 200 (body %v)", code, resp)
		}

		// Different (category, title) → no ON CONFLICT match → the INSERT
		// trips the probe index instead.
		code, resp = postBlobStore(t, h, ar,
			blobPayload("reference", "b1-dup-b", "dup.bin", "application/octet-stream", []byte("second")))
		if code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %v)", code, resp)
		}
		if got := resp["constraint"]; got != "uq_blob_probe_filename" {
			t.Errorf("constraint = %v, want uq_blob_probe_filename", got)
		}
		if got := resp["sqlstate"]; got != "23505" {
			t.Errorf("sqlstate = %v, want 23505", got)
		}
		errMsg, _ := resp["error"].(string)
		if !strings.Contains(errMsg, "uq_blob_probe_filename") {
			t.Errorf("error = %q, want the constraint name in the reason", errMsg)
		}
	})

	t.Run("check violation is 422 with the constraint named", func(t *testing.T) {
		mustExec(t, pool, `ALTER TABLE context_blobs ADD CONSTRAINT chk_blob_probe_mime
			CHECK (mime_type <> 'application/x-b1-probe')`)
		t.Cleanup(func() {
			mustExec(t, pool, `ALTER TABLE context_blobs DROP CONSTRAINT chk_blob_probe_mime`)
		})

		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b1-check", "check.bin", "application/x-b1-probe", []byte("nope")))
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %v)", code, resp)
		}
		if got := resp["constraint"]; got != "chk_blob_probe_mime" {
			t.Errorf("constraint = %v, want chk_blob_probe_mime", got)
		}
		if got := resp["sqlstate"]; got != "23514" {
			t.Errorf("sqlstate = %v, want 23514", got)
		}
		errMsg, _ := resp["error"].(string)
		if !strings.Contains(errMsg, "chk_blob_probe_mime") {
			t.Errorf("error = %q, want the constraint name in the reason", errMsg)
		}
	})

	t.Run("non-constraint failure stays 500", func(t *testing.T) {
		// 60-char home scope → 22001 on context_blobs.scope VARCHAR(50).
		longScope := strings.Repeat("s", 60)
		code, resp := postBlobStore(t, h, blobAR(longScope),
			blobPayload("reference", "b1-oversize-scope", "oversize.bin", "application/octet-stream", []byte("x")))
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %v)", code, resp)
		}
		if got, _ := resp["error"].(string); got != "Internal server error" {
			t.Errorf("error = %q, want the opaque 500 message", got)
		}
		if _, leaked := resp["sqlstate"]; leaked {
			t.Errorf("non-constraint failure leaked sqlstate: %v", resp)
		}
	})
}
