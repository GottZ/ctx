//go:build integration

// W02-08 negative probe of the stream form (briefing W02-08 "Negativ-Probe
// EnabledGate / Wissens-Credential") over the PRODUCTION router: the stream
// changes nothing in front of the route — with gateway.enabled=false a
// stream body to POST /v1/chat/completions gets the byte-identical 404 of
// the gate without one statement on the pool, and a knowledge credential
// gets the door's 401 invalid_api_key without one knowledge-auth statement.
// The patterns are TestV1EnabledGateNoDBHit and
// TestV1RejectsKnowledgeCredentials (v1_mount_integration_test.go).
//
//	go test -tags=integration -p 1 -count=1 -run 'V1Stream|V1EnabledGate|V1RejectsKnowledge|V1DoorCensus' -v ./cmd/ctxd/
package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
)

// v1StreamBodies are stream requests of the chat route: the briefing's
// shape on an alias that exists only by name, the granted alias, and one
// with stream_options.
var v1StreamBodies = map[string]string{
	"chat, stream":                `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	"granted, stream":             `{"model":"granted","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	"granted, stream, with usage": `{"model":"granted","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`,
}

// postStreamBody sends body as a POST over TLS from the fixture's client
// address and returns the answer without X-Request-ID (f.do with a body).
func (f *v1Fixture) postStreamBody(path, body string, set func(*http.Request)) v1Answer {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if set != nil {
		set(req)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	h := rec.Header().Clone()
	h.Del("X-Request-ID")
	return v1Answer{status: rec.Code, header: h, body: rec.Body.Bytes()}
}

// TestV1StreamEnabledGateNoDBHit: with gateway.enabled=false every stream
// body to POST /v1/chat/completions — anonymous, a knowledge key, a valid
// model key, a malformed one — gets the same bytes as the gate's 404 on GET
// /v1/models and costs the pool not one statement.
func TestV1StreamEnabledGateNoDBHit(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, false)
	ref := f.do(http.MethodGet, "/v1/models", nil)
	if ref.status != http.StatusNotFound || ref.code() != oai.CodeNotFound {
		t.Fatalf("reference GET /v1/models with the flag off: %d %s, want 404 not_found", ref.status, ref.body)
	}
	creds := map[string]func(*http.Request){
		"anonymous":           nil,
		"knowledge key":       withBearer(v1AdminKey),
		"valid model key":     withBearer(f.modelKey),
		"malformed model key": withBearer(modelkey.Prefix + "not-a-key"),
	}
	before := f.tracer.all.Load()
	n := 0
	for bname, body := range v1StreamBodies {
		for cname, set := range creds {
			a := f.postStreamBody("/v1/chat/completions", body, set)
			n++
			if !sameAnswer(a, ref) {
				t.Errorf("%s, %s: %d %v %s — want the gate's 404 byte for byte (%d %v %s)", bname, cname, a.status, a.header, a.body, ref.status, ref.header, ref.body)
			}
		}
	}
	stmts := f.tracer.all.Load() - before
	t.Logf("flag off: %d stream requests, %d statements on the router's pool (ctx_model_auth %d, knowledge auth %d); 404 body %s",
		n, stmts, f.tracer.modelAuth.Load(), f.tracer.knowledge.Count(), ref.body)
	if stmts != 0 {
		t.Errorf("flag off: %d statements on the pool, want 0 (the gate runs before the door)", stmts)
	}
}

// TestV1StreamRejectsKnowledgeCredentials: a 64-hex knowledge key as Bearer
// and as X-Context-Key, a live ctxt_ token with the /mcp audience and a live
// session cookie each get 401 invalid_api_key with WWW-Authenticate: Bearer
// realm="ctx" for a stream body — without a knowledge-auth statement, a
// ctx_model_auth call or a grant read. Positive control: a valid model key
// with the same stream body passes the door (one ctx_model_auth call).
func TestV1StreamRejectsKnowledgeCredentials(t *testing.T) {
	f := newV1Fixture(t, map[string]string{handler.EnvCanonicalIssuer: "https://ctx.example"}, nil)
	f.setEnabled(t, true)
	creds := v1KnowledgeCredentials(t, f)
	for name, set := range creds {
		if a := f.do(http.MethodGet, "/api/whoami", set); a.status != http.StatusOK {
			t.Fatalf("positive control %s on /api/whoami: %d %s — the credential is not live", name, a.status, a.body)
		}
	}
	knowledge, grants, door := f.tracer.knowledge.Count(), f.tracer.grantSQL.Load(), f.tracer.modelAuth.Load()
	for bname, body := range v1StreamBodies {
		for cname, set := range creds {
			a := f.postStreamBody("/v1/chat/completions", body, set)
			if a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey || a.header.Get("WWW-Authenticate") != `Bearer realm="ctx"` ||
				strings.HasPrefix(a.header.Get("Content-Type"), "text/event-stream") {
				t.Errorf("%s, %s: %d WWW-Authenticate=%q Content-Type=%q %s — want the door's JSON 401 invalid_api_key",
					bname, cname, a.status, a.header.Get("WWW-Authenticate"), a.header.Get("Content-Type"), a.body)
			}
		}
	}
	k, g, d := f.tracer.knowledge.Count()-knowledge, f.tracer.grantSQL.Load()-grants, f.tracer.modelAuth.Load()-door
	t.Logf("knowledge credentials × stream bodies: %d requests; knowledge-auth statements %d, grant reads %d, ctx_model_auth %d",
		len(creds)*len(v1StreamBodies), k, g, d)
	if k != 0 || g != 0 || d != 0 {
		t.Errorf("knowledge credentials under /v1: %d knowledge-auth statements, %d grant reads, %d ctx_model_auth calls — want 0 each", k, g, d)
	}

	// Positive control: the same body with a valid model key passes the door
	// (whatever the route answers behind it, it is neither the gate's 404
	// nor the door's 401).
	a := f.postStreamBody("/v1/chat/completions", v1StreamBodies["granted, stream"], withBearer(f.modelKey))
	t.Logf("positive control, valid model key: %d %q %.200s; ctx_model_auth %d", a.status, a.header.Get("Content-Type"), a.body, f.tracer.modelAuth.Load()-door)
	if a.status == http.StatusUnauthorized || a.status == http.StatusNotFound || f.tracer.modelAuth.Load()-door != 1 {
		t.Errorf("valid model key with a stream body: %d, %d ctx_model_auth calls — want past the door with one check", a.status, f.tracer.modelAuth.Load()-door)
	}
}

// v1KnowledgeCredentials are the four knowledge credentials of
// TestV1RejectsKnowledgeCredentials: the bootstrap key as Bearer and as
// X-Context-Key, a live ctxt_ access token with the /mcp audience and a live
// session cookie.
func v1KnowledgeCredentials(t *testing.T, f *v1Fixture) map[string]func(*http.Request) {
	t.Helper()
	ctx := context.Background()
	var principal string
	if err := f.seed.QueryRow(ctx, `INSERT INTO context_principals (display_name) VALUES ('w02-08-person') RETURNING id::text`).Scan(&principal); err != nil {
		t.Fatalf("principal: %v", err)
	}
	if _, err := f.seed.Exec(ctx, `UPDATE context_api_keys SET principal_id = $1::uuid WHERE id = $2::uuid`, principal, f.adminID); err != nil {
		t.Fatalf("bind principal: %v", err)
	}
	pair, err := store.MintTokenPair(ctx, f.seed, store.OAuthToken{APIKeyID: f.adminID, PrincipalID: principal,
		Audiences: []string{"https://ctx.example/mcp", "https://ctx.example/web"}, IssuedVia: "login"}, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	var tokenRow, family, session string
	if err := f.seed.QueryRow(ctx, `SELECT id::text, refresh_family::text FROM context_access_tokens WHERE token_hash = $1`,
		store.TokenHash(pair.AccessToken)).Scan(&tokenRow, &family); err != nil {
		t.Fatalf("token row: %v", err)
	}
	if err := f.seed.QueryRow(ctx, `INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'w02-08-csrf') RETURNING id::text`, principal, tokenRow, family).Scan(&session); err != nil {
		t.Fatalf("session: %v", err)
	}
	return map[string]func(*http.Request){
		"64-hex key as Bearer":        withBearer(v1AdminKey),
		"64-hex key as X-Context-Key": func(r *http.Request) { r.Header.Set("X-Context-Key", v1AdminKey) },
		"ctxt_ token (/mcp audience)": withBearer(pair.AccessToken),
		"session cookie":              func(r *http.Request) { r.Header.Set("Cookie", "ctx_session="+session) },
	}
}
