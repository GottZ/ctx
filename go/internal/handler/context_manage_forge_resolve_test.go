package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/forge"
)

// TestWriteForgeResolveError pins the status each refusal of the resolver
// maps onto (issue #509), wrapped or not, and that the fields a caller acts
// on travel with it: retry_after_s on a push in flight, the current flag on
// a stale expect, the way out on a truncated body kept local. Every other
// error is the generic 500 without its cause.
func TestWriteForgeResolveError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		res      forge.ResolveResult
		code     int
		field    string
		want     any
		contains string
	}{
		{"no mapping", forge.ErrConflictNotFound, forge.ResolveResult{}, http.StatusNotFound, "error", forgeNoMappingError, ""},
		{"direction", forge.ErrResolveDirection, forge.ResolveResult{}, http.StatusBadRequest, "", nil, "local or remote"},
		{"push in flight", forge.ErrPushInFlight, forge.ResolveResult{}, http.StatusConflict, "retry_after_s", float64(forgePushInFlightRetryAfterS), "retry"},
		{"flag changed", forge.ErrFlagChanged, forge.ResolveResult{Flag: "abcdef0123456789"}, http.StatusConflict, "flag", "abcdef0123456789", "list the conflicts again"},
		{"truncated body kept local", forge.ErrLocalTruncated, forge.ResolveResult{}, http.StatusConflict, "", nil, "resolve with direction remote"},
		{"wrapped", fmt.Errorf("resolve: %w", forge.ErrPushInFlight), forge.ResolveResult{}, http.StatusConflict, "retry_after_s", float64(forgePushInFlightRetryAfterS), ""},
		{"other", errors.New("pgx: connection reset by peer at 10.0.0.1"), forge.ResolveResult{}, http.StatusInternalServerError, "error", "Internal server error", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeForgeResolveError(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), "p1", c.res, c.err)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if rec.Code != c.code || body["success"] != false {
				t.Fatalf("status %d body %s, want %d with success:false", rec.Code, rec.Body.String(), c.code)
			}
			if c.field != "" && body[c.field] != c.want {
				t.Errorf("%s = %v, want %v (body %s)", c.field, body[c.field], c.want, rec.Body.String())
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, c.contains) {
				t.Errorf("error %q, want it to name %q", msg, c.contains)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.1") {
				t.Errorf("the cause reached the wire: %s", rec.Body.String())
			}
		})
	}
}
