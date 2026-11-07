package handler

// The gateway-limits-* actions without a wired registry (wave W06-15): each
// answers 503 before it reads a request field or the database — a handler
// built without SetGatewayLimits (a test, a broken boot) never dereferences
// the missing registry.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

func TestGatewayLimitsActionsNotWired(t *testing.T) {
	h := &ManageHandler{}
	ar := &auth.AuthResult{IsValid: true, IsAdmin: true}
	for name, fn := range map[string]func(http.ResponseWriter, *http.Request, *auth.AuthResult, manageRequest){
		"get": h.handleGatewayLimitsGet, "set": h.handleGatewayLimitsSet, "delete": h.handleGatewayLimitsDelete,
	} {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), ar,
			manageRequest{Data: []byte(`{"subject_kind":"instance","op":"chat","rpm":1}`)})
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not available") {
			t.Errorf("%s without a registry: %d %s, want 503 not available", name, rec.Code, rec.Body.String())
		}
	}
}
