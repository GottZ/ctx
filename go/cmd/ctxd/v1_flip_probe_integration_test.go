//go:build integration

// W06-20: the flip probe over the PRODUCTION router (design/06 §7 W06-20,
// masterplan W06-20 gate "404-Probe ohne DB-Hit"). Before the flip every
// model route — the catalog and the four inference routes — answers the
// gateway's one 404 in the OpenAI envelope without one statement on the
// pool, anonymous and with a valid model key alike; after the flip an
// anonymous request is the door's 401, still without a statement (stage 3
// refuses before the database); the way back (gateway.enabled=false) is the
// 404 again on the next request. The live half of the probe is
// conformance/oai/live/smoke.py flip-probe (curl-equivalent, run in S4).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestV1FlipProbe' -v ./cmd/ctxd/
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/GottZ/ctx/internal/oai"
)

// flipProbeRoutes are the model routes the probe walks: every route of
// v1Routes/v2Routes with a concrete path.
var flipProbeRoutes = [][2]string{
	{http.MethodGet, "/v1/models"},
	{http.MethodGet, "/v1/models/granted"},
	{http.MethodPost, "/v1/chat/completions"},
	{http.MethodPost, "/v1/embeddings"},
	{http.MethodPost, "/v1/rerank"},
	{http.MethodPost, "/v2/rerank"},
}

// flipErrorSchema resolves the vendored ErrorResponse (the same excerpt the
// conformance harness of internal/oai reads).
func flipErrorSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile("../../conformance/oai/testdata/openapi-c2aecc2f-error-response.json")
	if err != nil {
		t.Fatalf("schema excerpt: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema excerpt: %v", err)
	}
	defs := strings.ReplaceAll(string(doc.Components.Schemas), `"#/components/schemas/`, `"#/$defs/`)
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(`{"$ref":"#/$defs/ErrorResponse","$defs":`+defs+`}`), &s); err != nil {
		t.Fatalf("schema excerpt: %v", err)
	}
	res, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve ErrorResponse: %v", err)
	}
	return res
}

// TestV1FlipProbe is W06-20's gate: flag off ⇒ 404 not_found in the OpenAI
// envelope, byte-identical to oai.NotFound, with 0 statements — for an
// anonymous POST /v1/chat/completions above all (the live curl of the
// flip); flag on ⇒ anonymous 401 invalid_api_key with WWW-Authenticate and
// 0 statements; flag off again ⇒ 404 at once. Red against the order "door
// before gate" (mutant m1 under .gotmp/rcd-c4-flip/mut/).
func TestV1FlipProbe(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	schema := flipErrorSchema(t)
	ref := httptest.NewRecorder()
	oai.NotFound(ref, httptest.NewRequest(http.MethodGet, "/", nil))

	off := func(t *testing.T, phase string) {
		t.Helper()
		before := f.tracer.all.Load()
		creds := map[string]func(*http.Request){"anonymous": nil, "valid model key": withBearer(f.modelKey)}
		for _, rt := range flipProbeRoutes {
			for name, set := range creds {
				a := f.do(rt[0], rt[1], set)
				if a.status != http.StatusNotFound || a.code() != oai.CodeNotFound {
					t.Errorf("%s: %s %s, %s: %d %.120s, want 404 not_found (gate before door)", phase, rt[0], rt[1], name, a.status, a.body)
					continue
				}
				if string(a.body) != ref.Body.String() || a.header.Get("x-should-retry") != "false" || !strings.HasPrefix(a.header.Get("Content-Type"), "application/json") {
					t.Errorf("%s: %s %s, %s: %q %v, want the bytes of oai.NotFound with x-should-retry false", phase, rt[0], rt[1], name, a.body, a.header)
				}
				var v any
				if err := json.Unmarshal(a.body, &v); err != nil || schema.Validate(v) != nil {
					t.Errorf("%s: %s %s, %s: body is no valid ErrorResponse (%v)", phase, rt[0], rt[1], name, err)
				}
			}
		}
		if n := f.tracer.all.Load() - before; n != 0 {
			t.Errorf("%s: %d statements on the pool for %d requests, want 0 (ctx_model_auth %d) — the door ran before the gate",
				phase, n, len(flipProbeRoutes)*len(creds), f.tracer.modelAuth.Load())
		}
	}

	f.setEnabled(t, false)
	off(t, "before the flip")

	f.setEnabled(t, true)
	before := f.tracer.all.Load()
	a := f.do(http.MethodPost, "/v1/chat/completions", nil)
	t.Logf("after the flip, anonymous POST /v1/chat/completions: %d %s WWW-Authenticate=%q", a.status, strings.TrimSpace(string(a.body)), a.header.Get("WWW-Authenticate"))
	if a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey || a.header.Get("WWW-Authenticate") != `Bearer realm="ctx"` {
		t.Errorf("after the flip, anonymous: %d %q, want 401 invalid_api_key with WWW-Authenticate", a.status, a.body)
	}
	if n := f.tracer.all.Load() - before; n != 0 {
		t.Errorf("after the flip, anonymous: %d statements, want 0 (the door refuses a missing credential before the database)", n)
	}

	f.setEnabled(t, false)
	off(t, "after the way back")
}
