//go:build integration

// Integration probes for 02-W4a: the DCR /register happy paths that need a
// real context_oauth_clients row (201 responses, label truncation at the
// column, secret minting per auth method, admin-mode key gate, metadata
// persistence). The DB-less rejections live in oauth_register_test.go.
//
//	go test -tags=integration ./internal/handler/ -run TestDCRRegister_Integration -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDCRRegister_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := NewOAuthHandler(pool)

	post := func(t *testing.T, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.Register(rec, req)
		var doc map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		return rec, doc
	}

	t.Run("OpenValidPublicClient201_NoSecret", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "open")
		rec, doc := post(t, `{"redirect_uris":["https://client.example/cb"],"client_name":"probe"}`, nil)
		if rec.Code != 201 {
			t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
		clientID, _ := doc["client_id"].(string)
		if !strings.HasPrefix(clientID, "ctx_") {
			t.Errorf("client_id = %q, want ctx_ prefix", clientID)
		}
		if _, ok := doc["client_id_issued_at"]; !ok {
			t.Error("client_id_issued_at missing")
		}
		if _, ok := doc["client_secret"]; ok {
			t.Error("public client (none) must NOT receive a client_secret")
		}
		// Fail-closed persistence probe: the row is a dcr registration with
		// an EMPTY secret hash (never a valid secret) and no creator.
		var source, secretHash string
		var createdBy *string
		if err := pool.QueryRow(ctx,
			`SELECT registration_source, client_secret_hash, created_by::text
			 FROM context_oauth_clients WHERE client_id = $1`, clientID,
		).Scan(&source, &secretHash, &createdBy); err != nil {
			t.Fatalf("row lookup: %v", err)
		}
		if source != "dcr" || secretHash != "" || createdBy != nil {
			t.Errorf("row = (source %q, hash %q, created_by %v), want (dcr, empty, nil)", source, secretHash, createdBy)
		}
	})

	t.Run("LoopbackHTTPRedirect201", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "open")
		rec, _ := post(t, `{"redirect_uris":["http://127.0.0.1:7777/cb"]}`, nil)
		if rec.Code != 201 {
			t.Errorf("loopback http redirect: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("ClientName250Truncated200", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "open")
		long := strings.Repeat("x", 250)
		rec, doc := post(t, `{"redirect_uris":["https://client.example/cb"],"client_name":"`+long+`"}`, nil)
		if rec.Code != 201 {
			t.Fatalf("250-char client_name: status = %d, want 201 (no 22001/500; body %s)", rec.Code, rec.Body.String())
		}
		name, _ := doc["client_name"].(string)
		if len([]rune(name)) != 200 {
			t.Errorf("client_name len = %d, want 200 (truncated)", len([]rune(name)))
		}
		var label string
		if err := pool.QueryRow(ctx,
			`SELECT label FROM context_oauth_clients WHERE client_id = $1`, doc["client_id"],
		).Scan(&label); err != nil {
			t.Fatalf("label lookup: %v", err)
		}
		if len([]rune(label)) != 200 {
			t.Errorf("stored label len = %d, want 200", len([]rune(label)))
		}
	})

	t.Run("ConfidentialClient201_SecretOnceNeverExpires", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "open")
		rec, doc := post(t, `{"redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, nil)
		if rec.Code != 201 {
			t.Fatalf("client_secret_basic: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
		secret, _ := doc["client_secret"].(string)
		if len(secret) != 64 { // hex(32)
			t.Errorf("client_secret len = %d, want 64", len(secret))
		}
		if exp, ok := doc["client_secret_expires_at"].(float64); !ok || exp != 0 {
			t.Errorf("client_secret_expires_at = %v, want 0", doc["client_secret_expires_at"])
		}
	})

	t.Run("MetadataAndScopesPersisted", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "open")
		rec, doc := post(t, `{"redirect_uris":["https://client.example/cb"],"client_uri":"https://client.example","software_id":"probe-sw","scope":"read write"}`, nil)
		if rec.Code != 201 {
			t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
		var clientURI, softwareID string
		var scopes []string
		if err := pool.QueryRow(ctx,
			`SELECT metadata->>'client_uri', metadata->>'software_id', scopes
			 FROM context_oauth_clients WHERE client_id = $1`, doc["client_id"],
		).Scan(&clientURI, &softwareID, &scopes); err != nil {
			t.Fatalf("metadata lookup: %v", err)
		}
		if clientURI != "https://client.example" || softwareID != "probe-sw" {
			t.Errorf("metadata = (%q, %q), want the registered values", clientURI, softwareID)
		}
		if len(scopes) != 2 || scopes[0] != "read" || scopes[1] != "write" {
			t.Errorf("scopes = %v, want [read write] (requestable ceiling, INV-B data only)", scopes)
		}
	})

	t.Run("AdminMode_NonAdminKey403_AdminKey201", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "admin")

		_, memberPlain, err := store.CreateApiKey(ctx, pool, "dcr-member", "private", nil, "")
		if err != nil {
			t.Fatalf("create member key: %v", err)
		}
		rec, _ := post(t, `{"redirect_uris":["https://client.example/cb"]}`,
			map[string]string{"Authorization": "Bearer " + memberPlain})
		if rec.Code != 403 {
			t.Errorf("admin mode with member key: status = %d, want 403", rec.Code)
		}

		adminKey, adminPlain, err := store.CreateApiKey(ctx, pool, "dcr-admin", "private", nil, "")
		if err != nil {
			t.Fatalf("create admin key: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, adminKey.ID); err != nil {
			t.Fatalf("promote admin: %v", err)
		}
		rec, doc := post(t, `{"redirect_uris":["https://client.example/cb"]}`,
			map[string]string{"Authorization": "Bearer " + adminPlain})
		if rec.Code != 201 {
			t.Fatalf("admin mode with admin key: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
		// Forensics: admin-mode registrations carry the acting key.
		var createdBy *string
		if err := pool.QueryRow(ctx,
			`SELECT created_by::text FROM context_oauth_clients WHERE client_id = $1`, doc["client_id"],
		).Scan(&createdBy); err != nil {
			t.Fatalf("created_by lookup: %v", err)
		}
		if createdBy == nil || *createdBy != adminKey.ID {
			t.Errorf("created_by = %v, want acting admin key %s", createdBy, adminKey.ID)
		}
	})
}
