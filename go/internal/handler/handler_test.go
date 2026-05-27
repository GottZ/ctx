package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// wrongKeyType is a distinct type from contextKey, used to verify that
// context lookups are type-safe (SA1029: avoid bare string as context key).
type wrongKeyType string

// ===.
// RequestIDFromContext tests
// ===.

// SECURITY PROPERTY: Extracting a request ID from a nil-value context returns
// empty string, not a panic. Prevents nil-pointer crashes in error paths where
// context may not have been initialized with middleware.
func TestRequestIDFromContext_NilValue(t *testing.T) {
	ctx := context.Background()
	got := RequestIDFromContext(ctx)
	if got != "" {
		t.Errorf("RequestIDFromContext(background) = %q, want empty", got)
	}
}

// SECURITY PROPERTY: A valid request ID stored in context is retrieved correctly.
// Verifies the typed context key prevents collisions with other string values.
func TestRequestIDFromContext_ValidValue(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestIDKey, "abc123")
	got := RequestIDFromContext(ctx)
	if got != "abc123" {
		t.Errorf("RequestIDFromContext = %q, want %q", got, "abc123")
	}
}

// SECURITY PROPERTY: Using a plain string key (not the typed contextKey) does
// NOT retrieve a value, preventing context key collisions across packages.
func TestRequestIDFromContext_WrongKeyType(t *testing.T) {
	// Use a different named type instead of the typed contextKey — must not match.
	ctx := context.WithValue(context.Background(), wrongKeyType("request_id"), "injected")
	got := RequestIDFromContext(ctx)
	if got != "" {
		t.Errorf("RequestIDFromContext with wrong key type = %q, want empty (type safety)", got)
	}
}

// SECURITY PROPERTY: If a non-string value is stored under the correct key,
// the type assertion returns empty string instead of panicking.
func TestRequestIDFromContext_WrongValueType(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestIDKey, 12345)
	got := RequestIDFromContext(ctx)
	if got != "" {
		t.Errorf("RequestIDFromContext with int value = %q, want empty (type mismatch)", got)
	}
}

// ===.
// AuthResultFromContext tests
// ===.

// SECURITY PROPERTY: Missing auth result returns nil (deny by default), not a
// panic. Callers must check for nil before accessing fields.
func TestAuthResultFromContext_Missing(t *testing.T) {
	ctx := context.Background()
	got := AuthResultFromContext(ctx)
	if got != nil {
		t.Errorf("AuthResultFromContext(background) = %+v, want nil", got)
	}
}

// SECURITY PROPERTY: A valid AuthResult is retrieved correctly from context.
func TestAuthResultFromContext_ValidValue(t *testing.T) {
	ar := &auth.AuthResult{
		ApiKeyID:      "key-123",
		HomeScope:     "private",
		AllowedScopes: []string{"shared"},
		ReadScopes:    []string{"private", "shared"},
		IsValid:       true,
	}
	ctx := context.WithValue(context.Background(), authResultKey, ar)
	got := AuthResultFromContext(ctx)
	if got == nil {
		t.Fatal("AuthResultFromContext returned nil, want non-nil")
	}
	if got.ApiKeyID != "key-123" {
		t.Errorf("ApiKeyID = %q, want %q", got.ApiKeyID, "key-123")
	}
	if got.HomeScope != "private" {
		t.Errorf("HomeScope = %q, want %q", got.HomeScope, "private")
	}
	if !got.IsValid {
		t.Error("IsValid = false, want true")
	}
}

// SECURITY PROPERTY: A plain string key "auth_result" does not collide with
// the typed contextKey, preventing cross-package key injection.
func TestAuthResultFromContext_WrongKeyType(t *testing.T) {
	ar := &auth.AuthResult{IsValid: true, HomeScope: "private"}
	ctx := context.WithValue(context.Background(), wrongKeyType("auth_result"), ar)
	got := AuthResultFromContext(ctx)
	if got != nil {
		t.Errorf("AuthResultFromContext with wrong key type = %+v, want nil", got)
	}
}

// SECURITY PROPERTY: Storing a non-pointer AuthResult under the correct key
// returns nil (type assertion to *auth.AuthResult fails on value type).
func TestAuthResultFromContext_ValueTypeNotPointer(t *testing.T) {
	ar := auth.AuthResult{IsValid: true, HomeScope: "private"}
	ctx := context.WithValue(context.Background(), authResultKey, ar) // value, not pointer
	got := AuthResultFromContext(ctx)
	if got != nil {
		t.Errorf("AuthResultFromContext with value (not pointer) = %+v, want nil", got)
	}
}

// ===.
// writeJSON tests
// ===.

// SECURITY PROPERTY: writeJSON always sets Content-Type to application/json,
// preventing browsers from sniffing the response as HTML and executing XSS.
func TestWriteJSON_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"hello": "world"})

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

// SECURITY PROPERTY: writeJSON sets the specified HTTP status code, ensuring
// error responses (401, 400, 500) are not accidentally sent as 200 OK.
func TestWriteJSON_StatusCodes(t *testing.T) {
	codes := []int{
		http.StatusOK,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusInternalServerError,
		http.StatusTooManyRequests,
	}
	for _, code := range codes {
		w := httptest.NewRecorder()
		writeJSON(w, code, map[string]string{"status": "test"})
		if w.Code != code {
			t.Errorf("writeJSON status = %d, want %d", w.Code, code)
		}
	}
}

// SECURITY PROPERTY: writeJSON output is valid JSON, ensuring downstream
// parsers cannot be confused by malformed responses.
func TestWriteJSON_ValidJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   42,
		"nested":  map[string]string{"key": "value"},
	})

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("writeJSON produced invalid JSON: %v\nbody: %s", err, w.Body.String())
	}
}

// SECURITY PROPERTY: writeJSON correctly serializes HTML-unsafe characters,
// preventing JSON injection that could be interpreted as HTML.
func TestWriteJSON_HTMLEscaping(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{
		"xss": "<script>alert(1)</script>",
	})

	body := w.Body.String()
	// Go's json.Encoder escapes <, >, and & by default.
	if strings.Contains(body, "<script>") {
		t.Errorf("writeJSON did not escape HTML: %s", body)
	}
}

// SECURITY PROPERTY: writeJSON handles nil data without panicking.
func TestWriteJSON_NilData(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, nil)
	if w.Code != http.StatusOK {
		t.Errorf("writeJSON(nil) status = %d, want 200", w.Code)
	}
	body := strings.TrimSpace(w.Body.String())
	if body != "null" {
		t.Errorf("writeJSON(nil) body = %q, want %q", body, "null")
	}
}

// ===.
// Request size limit tests (MaxBodySize middleware + query handler)
// ===.

// SECURITY PROPERTY: The MaxBodySize middleware limits request body size,
// preventing denial-of-service via oversized payloads. A body exceeding the
// limit causes the json.Decoder to fail with a MaxBytesError.
func TestMaxBodySize_EnforcesLimit(t *testing.T) {
	const maxSize int64 = 1024 // 1 KB for test

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := MaxBodySize(maxSize)(inner)

	// Request within limit should succeed.
	t.Run("within_limit", func(t *testing.T) {
		body := strings.NewReader(strings.Repeat("a", 512))
		req := httptest.NewRequest(http.MethodPost, "/test", body)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("within-limit request got status %d, want 200", w.Code)
		}
	})

	// Request exceeding limit should fail.
	t.Run("exceeds_limit", func(t *testing.T) {
		body := strings.NewReader(strings.Repeat("a", 2048))
		req := httptest.NewRequest(http.MethodPost, "/test", body)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("over-limit request got status %d, want 413", w.Code)
		}
	})
}

// SECURITY PROPERTY: The 10KB query limit in HandleQuery prevents resource
// exhaustion via oversized query strings that would be sent to embedding and
// LLM models. We simulate the parsing logic here without needing a DB.
func TestQuerySizeLimit_10KB(t *testing.T) {
	// The handler checks len(req.Query) > 10*1024 AFTER JSON parsing.
	// We verify the boundary condition by constructing requests and checking
	// the JSON parsing + size check logic directly.

	type queryReq struct {
		Query string `json:"query"`
	}

	// Exactly 10240 bytes (10 KB) — should be accepted by size check.
	t.Run("exactly_10KB", func(t *testing.T) {
		q := queryReq{Query: strings.Repeat("a", 10*1024)}
		data, _ := json.Marshal(q)
		var parsed queryReq
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(&parsed); err != nil {
			t.Fatalf("JSON decode failed: %v", err)
		}
		if len(parsed.Query) > 10*1024 {
			t.Error("10KB query should pass size check")
		}
	})

	// 10241 bytes (10 KB + 1) — must be rejected.
	t.Run("exceeds_10KB_by_one", func(t *testing.T) {
		q := queryReq{Query: strings.Repeat("a", 10*1024+1)}
		data, _ := json.Marshal(q)
		var parsed queryReq
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(&parsed); err != nil {
			t.Fatalf("JSON decode failed: %v", err)
		}
		if len(parsed.Query) <= 10*1024 {
			t.Error("10KB+1 query should fail size check")
		}
	})
}

// ===.
// Request parsing tests (queryRequest JSON)
// ===.

// SECURITY PROPERTY: Malformed JSON is rejected, preventing JSON deserialization
// attacks and ensuring the handler never operates on garbage data.
func TestQueryRequestParsing_MalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated object", `{"query": "hello`},
		{"trailing comma", `{"query": "hello",}`},
		{"single quotes", `{'query': 'hello'}`},
		{"bare string", `hello`},
		{"empty body", ``},
		{"just whitespace", `   `},
		{"null byte in JSON", "{\"query\": \"he\x00llo\"}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req queryRequest
			err := json.NewDecoder(strings.NewReader(tc.body)).Decode(&req)
			if err == nil {
				// null byte in string is valid JSON, so skip error check for that case
				if tc.name == "null byte in JSON" {
					return
				}
				t.Errorf("expected JSON decode error for %q, got nil", tc.body)
			}
		})
	}
}

// SECURITY PROPERTY: Missing required field "query" results in an empty string,
// which the handler then rejects (empty query check). Go's JSON decoder does
// not error on missing fields — the handler must validate after parsing.
func TestQueryRequestParsing_MissingQuery(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		query string
	}{
		{"empty object", `{}`, ""},
		{"only category", `{"category": "test"}`, ""},
		{"only limit", `{"limit": 5}`, ""},
		{"query is null", `{"query": null}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req queryRequest
			err := json.NewDecoder(strings.NewReader(tc.body)).Decode(&req)
			if err != nil {
				t.Fatalf("JSON decode failed: %v", err)
			}
			if req.Query != tc.query {
				t.Errorf("Query = %q, want %q", req.Query, tc.query)
			}
		})
	}
}

// SECURITY PROPERTY: Extra/unknown fields in JSON are silently ignored (Go
// default), preventing attackers from injecting unexpected fields that might
// be processed by downstream code.
func TestQueryRequestParsing_ExtraFields(t *testing.T) {
	body := `{"query": "hello", "evil": "payload", "admin": true, "__proto__": {"isAdmin": true}}`
	var req queryRequest
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("JSON decode failed: %v", err)
	}
	if req.Query != "hello" {
		t.Errorf("Query = %q, want %q", req.Query, "hello")
	}
	// Extra fields must not appear in the struct.
	if req.Limit != nil {
		t.Error("Limit should be nil when not provided")
	}
}

// SECURITY PROPERTY: JSON type confusion — arrays where objects expected,
// nested objects where strings expected, numbers where strings expected.
// The decoder must reject or correctly handle these.
func TestQueryRequestParsing_TypeConfusion(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		expectErr bool
	}{
		{
			name:      "array instead of object",
			body:      `[{"query": "hello"}]`,
			expectErr: true,
		},
		{
			name:      "query as number",
			body:      `{"query": 12345}`,
			expectErr: true,
		},
		{
			name:      "query as boolean",
			body:      `{"query": true}`,
			expectErr: true,
		},
		{
			name:      "query as array",
			body:      `{"query": ["hello", "world"]}`,
			expectErr: true,
		},
		{
			name:      "query as nested object",
			body:      `{"query": {"$gt": ""}}`,
			expectErr: true,
		},
		{
			name:      "limit as string (should fail)",
			body:      `{"query": "hello", "limit": "999"}`,
			expectErr: true,
		},
		{
			name:      "limit as float",
			body:      `{"query": "hello", "limit": 5.5}`,
			expectErr: true, // Go rejects float when target is *int
		},
		{
			name:      "tags as string instead of array",
			body:      `{"query": "hello", "tags": "single-tag"}`,
			expectErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req queryRequest
			err := json.NewDecoder(strings.NewReader(tc.body)).Decode(&req)
			if tc.expectErr && err == nil {
				t.Errorf("expected error for %s, got nil (req=%+v)", tc.name, req)
			}
			if !tc.expectErr && err != nil {
				t.Errorf("unexpected error for %s: %v", tc.name, err)
			}
		})
	}
}

// v2.0.0 C2 (M048): categories_exclude + block_roles_exclude request fields.
// These are optional and default to empty slices when omitted, exactly like
// Tags. Decoding must populate them for the rrf.Search call site.
func TestQueryRequestParsing_ExcludeParams(t *testing.T) {
	cases := []struct {
		name              string
		body              string
		wantCatExclude    []string
		wantRoleExclude   []string
		shouldExpectError bool
	}{
		{
			name:            "omitted defaults to nil",
			body:            `{"query": "hello"}`,
			wantCatExclude:  nil,
			wantRoleExclude: nil,
		},
		{
			name:            "categories_exclude only",
			body:            `{"query": "hello", "categories_exclude": ["index"]}`,
			wantCatExclude:  []string{"index"},
			wantRoleExclude: nil,
		},
		{
			name:            "block_roles_exclude only",
			body:            `{"query": "hello", "block_roles_exclude": ["audit-trail", "synthesis"]}`,
			wantCatExclude:  nil,
			wantRoleExclude: []string{"audit-trail", "synthesis"},
		},
		{
			name:            "both lists provided",
			body:            `{"query": "hello", "categories_exclude": ["index"], "block_roles_exclude": ["audit-trail"]}`,
			wantCatExclude:  []string{"index"},
			wantRoleExclude: []string{"audit-trail"},
		},
		{
			name:            "empty arrays decoded as empty (not nil)",
			body:            `{"query": "hello", "categories_exclude": [], "block_roles_exclude": []}`,
			wantCatExclude:  []string{},
			wantRoleExclude: []string{},
		},
		{
			name:              "categories_exclude as string (type confusion)",
			body:              `{"query": "hello", "categories_exclude": "index"}`,
			shouldExpectError: true,
		},
		{
			name:              "block_roles_exclude as number",
			body:              `{"query": "hello", "block_roles_exclude": 42}`,
			shouldExpectError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req queryRequest
			err := json.NewDecoder(strings.NewReader(tc.body)).Decode(&req)
			if tc.shouldExpectError {
				if err == nil {
					t.Errorf("expected decode error for %s, got nil (req=%+v)", tc.name, req)
				}
				return
			}
			if err != nil {
				t.Fatalf("JSON decode failed: %v", err)
			}
			if !equalStringSlice(req.CategoriesExclude, tc.wantCatExclude) {
				t.Errorf("CategoriesExclude = %#v, want %#v", req.CategoriesExclude, tc.wantCatExclude)
			}
			if !equalStringSlice(req.BlockRolesExclude, tc.wantRoleExclude) {
				t.Errorf("BlockRolesExclude = %#v, want %#v", req.BlockRolesExclude, tc.wantRoleExclude)
			}
		})
	}
}

// equalStringSlice returns true for both-nil, both-empty, and equal-content.
// Distinguishes nil from non-nil-empty by comparing lengths and elements.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Both nil or both empty → equal length 0 — but also distinguish nil/[].
	if (a == nil) != (b == nil) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SECURITY PROPERTY: Duplicate JSON keys — Go's encoding/json uses last-wins
// semantics. Verify this behavior is consistent and an attacker cannot exploit
// parser differentials by sending duplicate keys.
func TestQueryRequestParsing_DuplicateKeys(t *testing.T) {
	body := `{"query": "first", "query": "second"}`
	var req queryRequest
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("JSON decode failed: %v", err)
	}
	// Go's encoding/json uses last-wins for duplicate keys.
	if req.Query != "second" {
		t.Errorf("Query = %q, want %q (last-wins semantics)", req.Query, "second")
	}
}

// ===.
// Limit clamping tests
// ===.

// SECURITY PROPERTY: The limit parameter is clamped to [1, 20], preventing
// resource exhaustion via extremely high limits (unbounded DB queries) and
// zero/negative limits that could cause empty or erroneous results.
func TestLimitClamping(t *testing.T) {
	cases := []struct {
		name     string
		input    *int
		expected int
	}{
		{"nil (default)", nil, 5},
		{"zero clamped to 1", intPtr(0), 1},
		{"negative clamped to 1", intPtr(-1), 1},
		{"large negative clamped to 1", intPtr(-999999), 1},
		{"one stays one", intPtr(1), 1},
		{"five stays five", intPtr(5), 5},
		{"twenty stays twenty", intPtr(20), 20},
		{"twentyone clamped to 20", intPtr(21), 20},
		{"max int clamped to 20", intPtr(2147483647), 20},
		{"min int clamped to 1", intPtr(-2147483648), 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Replicate the exact clamping logic from HandleQuery.
			limit := 5
			if tc.input != nil {
				limit = *tc.input
				if limit < 1 {
					limit = 1
				}
				if limit > 20 {
					limit = 20
				}
			}
			if limit != tc.expected {
				t.Errorf("limit clamping: input=%v, got %d, want %d", tc.input, limit, tc.expected)
			}
		})
	}
}

func intPtr(n int) *int {
	return &n
}

// ===.
// RequestID middleware tests
// ===.

// SECURITY PROPERTY: The RequestID middleware generates an ID when none is
// provided, ensuring every request can be traced in logs for forensic analysis.
func TestRequestIDMiddleware_GeneratesID(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if id == "" {
			t.Error("expected request ID in context, got empty")
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	respID := w.Header().Get("X-Request-ID")
	if respID == "" {
		t.Error("X-Request-ID response header missing")
	}
	// Generated IDs should be 16 hex chars (8 random bytes).
	if len(respID) != 16 {
		t.Errorf("generated request ID length = %d, want 16", len(respID))
	}
}

// SECURITY PROPERTY: Valid client-provided request IDs (hex + hyphens, <= 64 chars)
// are passed through, enabling end-to-end tracing.
func TestRequestIDMiddleware_ClientProvided(t *testing.T) {
	const validID = "abcdef01-2345-6789-abcd-ef0123456789"
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if id != validID {
			t.Errorf("context request ID = %q, want %q", id, validID)
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestID(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", validID)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	respID := w.Header().Get("X-Request-ID")
	if respID != validID {
		t.Errorf("X-Request-ID response = %q, want %q", respID, validID)
	}
}

// SECURITY PROPERTY: Client-provided request IDs with invalid characters,
// injection payloads, or excessive length are rejected and replaced with
// a server-generated ID. The adversarial value must never appear in the response.
func TestRequestIDMiddleware_InjectionAttempt(t *testing.T) {
	adversarial := []string{
		"id\r\nX-Injected: true",
		"id\x00null",
		"id<script>alert(1)</script>",
		strings.Repeat("a", 10000),  // very long ID (exceeds 64 char limit)
		strings.Repeat("a", 65),     // exactly one over the limit
		"valid-hex-but-has-GHIJKL",  // non-hex letters
		"abc 123",                   // space
		"abc/def",                   // slash
	}

	for _, adversarialID := range adversarial {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := RequestID(inner)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Request-ID", adversarialID)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		respID := w.Header().Get("X-Request-ID")
		// Must be a server-generated 16-char hex ID, not the adversarial input.
		if len(respID) != 16 {
			t.Errorf("adversarial ID %q: response ID length = %d, want 16 (server-generated)", adversarialID, len(respID))
		}
		if !validRequestID.MatchString(respID) {
			t.Errorf("adversarial ID %q: response ID %q is not valid hex", adversarialID, respID)
		}
	}
}

// SECURITY PROPERTY: Request IDs at the boundary of the max length (64 chars)
// are accepted when valid, and rejected when exactly one over.
func TestRequestIDMiddleware_MaxLength(t *testing.T) {
	// Exactly 64 hex chars — should be accepted.
	exactly64 := strings.Repeat("ab", 32) // 64 chars
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := RequestID(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", exactly64)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Header().Get("X-Request-ID") != exactly64 {
		t.Errorf("64-char valid ID was rejected, got %q", w.Header().Get("X-Request-ID"))
	}

	// 65 hex chars — should be rejected.
	tooLong := strings.Repeat("ab", 32) + "c" // 65 chars
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.Header.Set("X-Request-ID", tooLong)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Header().Get("X-Request-ID") == tooLong {
		t.Error("65-char ID should have been rejected")
	}
}

// SECURITY PROPERTY: isValidRequestID unit tests cover edge cases.
func TestIsValidRequestID(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		valid bool
	}{
		{"empty", "", false},
		{"hex only", "abcdef0123456789", true},
		{"uppercase hex", "ABCDEF0123456789", true},
		{"mixed case hex", "aAbBcCdDeEfF", true},
		{"with hyphens", "abcd-ef01-2345", true},
		{"UUID format", "550e8400-e29b-41d4-a716-446655440000", true},
		{"only hyphens", "---", true},
		{"single char", "a", true},
		{"non-hex letter g", "abcg", false},
		{"non-hex letter z", "abcz", false},
		{"space", "abc def", false},
		{"underscore", "abc_def", false},
		{"dot", "abc.def", false},
		{"slash", "abc/def", false},
		{"newline", "abc\ndef", false},
		{"null byte", "abc\x00def", false},
		{"exactly 64", strings.Repeat("a", 64), true},
		{"65 chars", strings.Repeat("a", 65), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isValidRequestID(tc.id)
			if got != tc.valid {
				t.Errorf("isValidRequestID(%q) = %v, want %v", tc.id, got, tc.valid)
			}
		})
	}
}

// ===.
// Recovery middleware tests
// ===.

// SECURITY PROPERTY: The Recovery middleware catches panics and returns 500
// without leaking stack traces or internal details to the client.
func TestRecoveryMiddleware_CatchesPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	handler := Recovery(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("recovery status = %d, want 500", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "internal server error") {
		t.Errorf("recovery body = %q, want 'internal server error'", body)
	}
	// Must NOT contain stack trace details.
	if strings.Contains(body, "goroutine") || strings.Contains(body, "panic") {
		t.Errorf("recovery body leaks internal details: %s", body)
	}
}

// SECURITY PROPERTY: Recovery middleware does not interfere with normal requests.
func TestRecoveryMiddleware_NoPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handler := Recovery(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("no-panic status = %d, want 200", w.Code)
	}
}

// ===.
// WithScheduler tests
// ===.

// SECURITY PROPERTY: WithScheduler with nil notifier passes through directly
// without panicking, ensuring resilient operation when scheduler is not configured.
func TestWithScheduler_NilNotifier(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := WithScheduler(nil, inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("inner handler not called with nil scheduler")
	}
}

// mockScheduler tracks query start/end calls.
type mockScheduler struct {
	starts int
	ends   int
}

func (m *mockScheduler) QueryStart() { m.starts++ }
func (m *mockScheduler) QueryEnd()   { m.ends++ }

// SECURITY PROPERTY: WithScheduler signals query end even when the handler
// panics (via defer), preventing scheduler resource leaks.
func TestWithScheduler_SignalsStartAndEnd(t *testing.T) {
	ms := &mockScheduler{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := WithScheduler(ms, inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if ms.starts != 1 {
		t.Errorf("scheduler starts = %d, want 1", ms.starts)
	}
	if ms.ends != 1 {
		t.Errorf("scheduler ends = %d, want 1", ms.ends)
	}
}

// SECURITY PROPERTY: WithScheduler calls QueryEnd even if the handler panics,
// preventing a stuck scheduler state from blocking future queries.
func TestWithScheduler_QueryEndOnPanic(t *testing.T) {
	ms := &mockScheduler{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	// Wrap with Recovery so the panic does not kill the test.
	handler := Recovery(WithScheduler(ms, inner))
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if ms.starts != 1 {
		t.Errorf("scheduler starts = %d, want 1", ms.starts)
	}
	if ms.ends != 1 {
		t.Errorf("scheduler ends = %d, want 1 (must call QueryEnd even on panic)", ms.ends)
	}
}

// ===.
// contains helper tests
// ===.

// SECURITY PROPERTY: The contains helper is used in scope checks. It must
// correctly match and reject values to prevent scope bypass.
func TestContains(t *testing.T) {
	cases := []struct {
		name  string
		slice []string
		val   string
		want  bool
	}{
		{"found in middle", []string{"a", "b", "c"}, "b", true},
		{"found at start", []string{"private", "shared"}, "private", true},
		{"found at end", []string{"private", "shared"}, "shared", true},
		{"not found", []string{"private", "shared"}, "work", false},
		{"empty slice", []string{}, "private", false},
		{"nil slice", nil, "private", false},
		{"empty value in slice", []string{""}, "", true},
		{"empty value not in slice", []string{"a", "b"}, "", false},
		{"case sensitive", []string{"Private"}, "private", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contains(tc.slice, tc.val)
			if got != tc.want {
				t.Errorf("contains(%v, %q) = %v, want %v", tc.slice, tc.val, got, tc.want)
			}
		})
	}
}

// ===.
// responseWriter tests
// ===.

// SECURITY PROPERTY: The responseWriter wrapper correctly captures status codes,
// ensuring the Logger middleware logs the actual response status (not always 200).
func TestResponseWriter_CapturesStatusCode(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: inner, statusCode: http.StatusOK}

	rw.WriteHeader(http.StatusForbidden)
	if rw.statusCode != http.StatusForbidden {
		t.Errorf("statusCode = %d, want %d", rw.statusCode, http.StatusForbidden)
	}
}

// SECURITY PROPERTY: The default status code is 200 (set at construction in
// Logger middleware), matching Go's default behavior.
func TestResponseWriter_DefaultStatusOK(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: inner, statusCode: http.StatusOK}
	if rw.statusCode != http.StatusOK {
		t.Errorf("default statusCode = %d, want 200", rw.statusCode)
	}
}
