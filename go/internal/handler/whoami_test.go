package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// whoamiReqAs runs GET /api/whoami directly against the handler with the given
// AuthResult injected (nil = request that somehow bypassed the Auth group) and
// a stubbed label lookup — no database in -short runs.
func whoamiReqAs(t *testing.T, ar *auth.AuthResult, label string, lookupErr error) *httptest.ResponseRecorder {
	t.Helper()
	h := NewWhoamiHandler(nil)
	h.labelByKeyID = func(context.Context, string) (string, error) { return label, lookupErr }

	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	if ar != nil {
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	}
	rec := httptest.NewRecorder()
	h.HandleWhoami(rec, req)
	return rec
}

func TestWhoami_ValidKey(t *testing.T) {
	rec := whoamiReqAs(t, adminAR(), "example-admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var got whoamiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Success || got.Label != "example-admin" || got.HomeScope != "private" || !got.Admin {
		t.Fatalf("unexpected response: %+v", got)
	}
	if len(got.ReadScopes) != 2 || got.ReadScopes[0] != "private" || got.ReadScopes[1] != "shared" {
		t.Fatalf("read_scopes = %v, want [private shared]", got.ReadScopes)
	}
}

func TestWhoami_NonAdminKey(t *testing.T) {
	rec := whoamiReqAs(t, nonAdminAR(), "example-reader", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got whoamiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Admin {
		t.Fatal("admin = true for a non-admin key")
	}
}

func TestWhoami_NoAuthContext401(t *testing.T) {
	rec := whoamiReqAs(t, nil, "unused", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestWhoami_NoKeyThroughAuthMiddleware exercises the real group semantics:
// a request without any credentials is rejected by the Auth middleware before
// the handler runs (empty key short-circuits, the nil pool is never touched).
func TestWhoami_NoKeyThroughAuthMiddleware(t *testing.T) {
	h := NewWhoamiHandler(nil)
	h.labelByKeyID = func(context.Context, string) (string, error) {
		t.Fatal("handler reached without auth — middleware did not gate")
		return "", nil
	}
	chain := Auth(nil)(http.HandlerFunc(h.HandleWhoami))

	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from Auth middleware", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("body = %q, want unauthorized envelope", rec.Body.String())
	}
}

func TestWhoami_LabelLookupError500(t *testing.T) {
	rec := whoamiReqAs(t, adminAR(), "", errors.New("connection refused"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"success":false`) {
		t.Fatalf("body = %q, want success:false envelope", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatal("internal error detail leaked into the response body")
	}
}

// TestWhoamiGoldenShape pins the wire shape (design 04-§3.2, §5.3): the
// hand-maintained TS WhoamiResponse type mirrors this — changing it means a
// deliberate golden update on both sides.
func TestWhoamiGoldenShape(t *testing.T) {
	got, err := json.Marshal(whoamiResponse{
		Success:    true,
		Label:      "example-key",
		HomeScope:  "private",
		ReadScopes: []string{"private", "shared"},
		Admin:      true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"success":true,"label":"example-key","home_scope":"private","read_scopes":["private","shared"],"admin":true}`
	if string(got) != want {
		t.Fatalf("golden shape drift:\n got: %s\nwant: %s", got, want)
	}
}
