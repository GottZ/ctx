// Wave C6 (Cluster-Topic-Map, design/03 §4.8/§5.7 + §7 "C6") — the two facet
// gates that need NO database, and prove it by running against a nil pool: if
// either arm reached the store, this test would panic instead of failing.
//
//	(ii) FORM GATE: a malformed handle is a 400 BEFORE any DB roundtrip. Without
//	     it the value would travel into `$n::uuid` and come back as SQLSTATE
//	     22P02 → a 500 — which is itself a signal ("this string was not a
//	     handle") and, worse, a distinguishable one;
//	(vi) DARK STATE: with cluster.facet_enabled off (the default) the field is
//	     ignored ENTIRELY — no filter, no 400, no echo. The request behaves
//	     exactly as it did before C6, when `cluster` was an unknown JSON field
//	     the decoder dropped.
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
)

func c6Auth() *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, ApiKeyID: "019d0000-0000-7000-9000-0000000000ff",
		HomeScope: "private", ReadScopes: []string{"private"}}
}

// c6Post drives HandleSearch with a nil pool. Only paths that return BEFORE the
// store call survive that; anything else panics, which is the point.
func c6Post(t *testing.T, facetEnabled bool, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewSearchHandler(nil, ovStubConfig{&config.Config{
		ClusterOps: config.ClusterOpsConfig{FacetEnabled: facetEnabled},
	}}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, c6Auth()))
	rec := httptest.NewRecorder()
	h.HandleSearch(rec, req)
	return rec
}

// Gate (ii): malformed handle ⇒ 400, no DB roundtrip.
//
// ROT-PROBE: drop the fullUUIDRe check (pattern handler/graph.go) ⇒ the request
// reaches the nil pool ⇒ panic instead of 400 ⇒ red.
func TestSearchClusterFacet_MalformedHandleIs400WithoutDB(t *testing.T) {
	for _, bad := range []string{`"not-a-uuid"`, `"019c9629-0000-7000-9000"`, `"019c9629-0000-7000-9000-00000000a001x"`, `"'; DROP TABLE"`} {
		rec := c6Post(t, true, `{"cluster":`+bad+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("cluster=%s: status = %d, want 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"success":false`) {
			t.Errorf("cluster=%s: body should be an error envelope: %s", bad, rec.Body.String())
		}
	}
}

// Gate (vi): with the facet OFF the same malformed value is simply ignored —
// the request continues as an ordinary search. Against the nil pool that shows
// up as a panic, which is exactly what "it went on to the store" means; the
// recover below turns it into the assertion.
//
// ROT-PROBE: gate the form check on nothing (validate before consulting
// facet_enabled) ⇒ the disabled path answers 400 ⇒ a behaviour change in the
// dark state ⇒ red.
func TestSearchClusterFacet_DisabledIgnoresTheField(t *testing.T) {
	status, panicked := func() (status int, panicked bool) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true // nil pool dereference == the search ran on unfiltered
			}
		}()
		rec := c6Post(t, false, `{"cluster":"not-a-uuid"}`)
		return rec.Code, false
	}()
	if !panicked && status != http.StatusInternalServerError {
		t.Errorf("with cluster.facet_enabled off the field must be ignored and the search must proceed; got status %d", status)
	}
	if status == http.StatusBadRequest {
		t.Error("the dark state rejected a value it must ignore — pre-C6 behaviour is not preserved")
	}
	t.Logf("disabled facet: reached the store (panicked=%v, status=%d)", panicked, status)
}
