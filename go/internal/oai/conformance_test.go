package oai_test

// Conformance harness, Go half (design/02 §7 "Konformitäts-Harness", wave
// W02-02): the official OpenAI Go SDK (github.com/openai/openai-go/v3) talks
// to an httptest server whose routes answer with a given writer set, and
// every answer is checked the way a client sees it — the SDK's typed error
// (status, type, code), the retry count the fake observes, the headers, and
// the bytes on the wire against the vendored ErrorResponse schema
// (conformance/oai/testdata, openai-openapi @ c2aecc2f).
//
// The harness grows per wave: every later /v1 handler adds its cases here and
// in conformance/oai/test_openai_sdk.py (the Python half, build tag
// pyconformance, see pyconformance_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// schemaExcerpt is the vendored openai-openapi excerpt (ErrorResponse and its
// transitive $ref closure). The Python harness reads the same file.
const schemaExcerpt = "../../conformance/oai/testdata/openapi-c2aecc2f-error-response.json"

// listModelsExcerpt is the excerpt of the model list (ListModelsResponse and
// Model, same commit; W02-05). The Python harness reads the same file.
const listModelsExcerpt = "../../conformance/oai/testdata/openapi-c2aecc2f-list-models-response.json"

// sdkCase is one error situation a client can meet. The fake serves it under
// /c/<name>/, so the SDK's GET <base>/models lands on /c/<name>/models.
type sdkCase struct {
	name        string
	status      int
	errType     string
	code        string // the parsed error.code the SDK must expose
	shouldRetry string // the explicit x-should-retry value on the wire
	maxRetries  int    // SDK retry budget for the call
	wantHits    int    // requests the fake must see (1 = no retry)
	headers     map[string]string
}

// sdkCases is the case table both SDK halves run. x-should-retry:false must
// stop the SDK's own retry (0 retries although maxRetries is 2); the one
// retryable answer (budget exhausted) must be retried exactly as often as
// the budget allows, honouring Retry-After.
var sdkCases = []sdkCase{
	{name: "not_found", status: http.StatusNotFound, errType: "invalid_request_error", code: "not_found",
		shouldRetry: "false", maxRetries: 2, wantHits: 1},
	{name: "method_not_allowed", status: http.StatusMethodNotAllowed, errType: "invalid_request_error", code: "method_not_allowed",
		shouldRetry: "false", maxRetries: 2, wantHits: 1, headers: map[string]string{"Allow": "POST"}},
	{name: "invalid_api_key", status: http.StatusUnauthorized, errType: "invalid_request_error", code: "invalid_api_key",
		shouldRetry: "false", maxRetries: 2, wantHits: 1, headers: map[string]string{"WWW-Authenticate": `Bearer realm="ctx"`}},
	{name: "request_too_large", status: http.StatusRequestEntityTooLarge, errType: "invalid_request_error", code: "request_too_large",
		shouldRetry: "false", maxRetries: 2, wantHits: 1},
	{name: "rate_limit_no_retry", status: http.StatusTooManyRequests, errType: "rate_limit_error", code: "rate_limit_exceeded",
		shouldRetry: "false", maxRetries: 2, wantHits: 1},
	{name: "internal_error", status: http.StatusServiceUnavailable, errType: "service_unavailable_error", code: "internal_error",
		shouldRetry: "false", maxRetries: 2, wantHits: 1},
	{name: "server_overloaded", status: http.StatusServiceUnavailable, errType: "service_unavailable_error", code: "server_overloaded",
		shouldRetry: "true", maxRetries: 1, wantHits: 2, headers: map[string]string{"Retry-After": "1"}},
	// The admission writer of /v1 (A03-W8, handler.V1Admission): the
	// dispatcher's saturation (429 server_busy, both Retry-After headers from
	// the rejection's own estimate, retried after it) and a model no target
	// offers (503 model_unavailable, Retry-After = the largest step).
	{name: "server_busy", status: http.StatusTooManyRequests, errType: "rate_limit_error", code: "server_busy",
		shouldRetry: "true", maxRetries: 1, wantHits: 2, headers: map[string]string{"Retry-After": "1", "retry-after-ms": "1000"}},
	{name: "model_unavailable", status: http.StatusServiceUnavailable, errType: "service_unavailable_error", code: "model_unavailable",
		shouldRetry: "true", maxRetries: 0, wantHits: 1, headers: map[string]string{"Retry-After": "2"}},
}

// confDispatch is the dispatcher behind the admission writer cases: steps
// {1 s, 2 s}, nothing offered (the writer reads only the steps).
type confDispatch struct{}

func (confDispatch) Acquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, dispatch.ErrExternalNotOffered
}

func (confDispatch) TryAcquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, dispatch.ErrExternalNotOffered
}

func (confDispatch) ExternalAdmissible(string) bool { return false }
func (confDispatch) ExternalWeightCap(string) int   { return 0 }
func (confDispatch) ExternalRetryAfterSteps() []time.Duration {
	return []time.Duration{time.Second, 2 * time.Second}
}

// admissionWriter answers err through the /v1 admission writer, jitter 0
// (the headers are pinned per case).
func admissionWriter(err error) http.HandlerFunc {
	adm := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: confDispatch{}, Settings: func() oai.Settings { return oai.Settings{} }})
	return func(w http.ResponseWriter, r *http.Request) { adm.WriteAdmissionError(r.Context(), w, err) }
}

// writerSet maps a case name to the handler that answers it.
type writerSet map[string]http.HandlerFunc

// oaiWriters answers every case with the W02-02 writers. The two situations
// without a fixed writer (a 429 the server marks as not retryable, which the
// limiter of design/06 sends for quota) go through the general oai.Write.
func oaiWriters() writerSet {
	return writerSet{
		"not_found": oai.NotFound,
		"method_not_allowed": func(w http.ResponseWriter, r *http.Request) {
			// Stand-in for the mounting wave's Allow computation (W01-6):
			// chi sets no Allow for a custom 405 handler.
			w.Header().Set("Allow", "POST")
			oai.MethodNotAllowed(w, r)
		},
		"invalid_api_key":   func(w http.ResponseWriter, _ *http.Request) { oai.WriteInvalidAPIKey(w) },
		"request_too_large": func(w http.ResponseWriter, _ *http.Request) { oai.WriteRequestTooLarge(w, 8<<20) },
		"rate_limit_no_retry": func(w http.ResponseWriter, _ *http.Request) {
			oai.Write(w, oai.Error{Status: http.StatusTooManyRequests, Type: oai.TypeRateLimit, Code: oai.CodeRateLimitExceeded,
				Message: "Rate limit reached."})
		},
		"internal_error":    func(w http.ResponseWriter, _ *http.Request) { oai.WriteInternal(w) },
		"server_overloaded": func(w http.ResponseWriter, _ *http.Request) { oai.WriteOverloaded(w) },
		"server_busy":       admissionWriter(&dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: time.Second}),
		"model_unavailable": admissionWriter(dispatch.ErrExternalNotOffered),
	}
}

// houseWriters is the Ist state before W02-02: the answers the tree could
// give for each situation, byte-for-byte the house forms — chi/web 404
// (http.NotFound, web/web.go), chi's bodyless 405, Auth's 401
// (handler/middleware.go), the strict body cap's 413, the dispatcher
// rejection body (handler/dispatch_reject.go rejectionBody, here also as the
// nearest form of "overloaded", which had no writer at all) and Recovery's
// plain-text 500 for an internal fault.
func houseWriters() writerSet {
	houseJSON := func(status int, body any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
	}
	return writerSet{
		"not_found":          http.NotFound,
		"method_not_allowed": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) },
		"invalid_api_key": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://`+r.Host+`/.well-known/oauth-protected-resource/mcp", scope="mcp"`)
			houseJSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})(w, r)
		},
		"request_too_large":   houseJSON(http.StatusRequestEntityTooLarge, map[string]any{"success": false, "error": "request body exceeds 1 MB cap"}),
		"rate_limit_no_retry": houseJSON(http.StatusTooManyRequests, map[string]any{"success": false, "error": "server busy — retry shortly"}),
		"internal_error": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		},
		"server_overloaded": houseJSON(http.StatusServiceUnavailable, map[string]any{"success": false, "error": "server busy — retry shortly"}),
		// Before A03-W8 a /v1 saturation had only the house 429 body of the
		// in-house rejection mapping, and a not-offered model no writer at all.
		"server_busy":       houseJSON(http.StatusTooManyRequests, map[string]any{"success": false, "error": "server busy — retry shortly"}),
		"model_unavailable": houseJSON(http.StatusServiceUnavailable, map[string]any{"success": false, "error": "server busy — retry shortly"}),
	}
}

// fakeServer is the httptest side: it counts requests per case and keeps the
// last body it wrote per case, so the schema check runs on the wire bytes.
type fakeServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	bodies map[string][]byte
}

type captureWriter struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.buf.Write(p)
	return c.ResponseWriter.Write(p)
}

func newFakeServer(t testing.TB, ws writerSet) *fakeServer {
	t.Helper()
	f := &fakeServer{hits: map[string]int{}, bodies: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/c/{case}/", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("case")
		h, ok := ws[name]
		if !ok {
			http.Error(w, "unknown case "+name, http.StatusTeapot)
			return
		}
		f.mu.Lock()
		f.hits[name]++
		f.mu.Unlock()
		cw := &captureWriter{ResponseWriter: w}
		h(cw, r)
		f.mu.Lock()
		f.bodies[name] = cw.buf.Bytes()
		f.mu.Unlock()
	})
	// The Python half reads and resets the counters over HTTP.
	mux.HandleFunc("GET /_hits/{case}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := f.hits[r.PathValue("case")]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"hits": n})
	})
	mux.HandleFunc("POST /_reset", func(w http.ResponseWriter, _ *http.Request) {
		f.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = map[string]int{}
	f.bodies = map[string][]byte{}
}

func (f *fakeServer) observed(name string) (int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name], f.bodies[name]
}

// loadErrorSchema resolves the vendored ErrorResponse schema with
// github.com/google/jsonschema-go. The excerpt keeps the spec's own layout
// (components/schemas); jsonschema-go resolves JSON pointers only through
// schema keywords, so the loader moves the definitions under $defs and
// rewrites the refs — mechanical, the schema values stay the vendored ones.
func loadErrorSchema(t testing.TB) *jsonschema.Resolved {
	t.Helper()
	return loadSchema(t, schemaExcerpt, "ErrorResponse")
}

// loadSchema resolves the vendored schema name of the excerpt at path (same
// mechanical $defs move as above).
func loadSchema(t testing.TB, path, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema excerpt: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse schema excerpt: %v", err)
	}
	defs := strings.ReplaceAll(string(doc.Components.Schemas), `"#/components/schemas/`, `"#/$defs/`)
	root := `{"$ref":"#/$defs/` + name + `","$defs":` + defs + `}`
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(root), &s); err != nil {
		t.Fatalf("schema excerpt is not a JSON Schema: %v", err)
	}
	res, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return res
}

// goSDKViolations runs every case through the Go SDK against a fake serving
// ws and returns what a client would trip over — empty means conformant.
func goSDKViolations(t *testing.T, ws writerSet) []string {
	t.Helper()
	srv := newFakeServer(t, ws)
	schema := loadErrorSchema(t)
	var out []string
	for _, c := range sdkCases {
		bad := func(format string, args ...any) {
			out = append(out, c.name+": "+fmt.Sprintf(format, args...))
		}
		client := openai.NewClient(
			option.WithBaseURL(srv.URL+"/c/"+c.name+"/"),
			option.WithAPIKey("sk-conformance"),
			option.WithMaxRetries(c.maxRetries),
		)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		_, err := client.Models.List(ctx)
		elapsed := time.Since(start)
		cancel()
		hits, body := srv.observed(c.name)
		if hits != c.wantHits {
			bad("fake saw %d request(s), want %d (x-should-retry %s, max retries %d)", hits, c.wantHits, c.shouldRetry, c.maxRetries)
		}
		// A retried case must have waited Retry-After (1 s) before the retry.
		if c.wantHits > 1 && c.headers["Retry-After"] == "1" && elapsed < 900*time.Millisecond {
			bad("retry after %v, want >= 1 s (Retry-After: 1 not honoured)", elapsed)
		}
		var apiErr *openai.Error
		if !errors.As(err, &apiErr) {
			bad("SDK returned %v, want *openai.Error", err)
			continue
		}
		if apiErr.StatusCode != c.status {
			bad("status %d, want %d", apiErr.StatusCode, c.status)
		}
		if apiErr.Code != c.code {
			bad("SDK parsed code %q, want %q", apiErr.Code, c.code)
		}
		if apiErr.Type != c.errType {
			bad("SDK parsed type %q, want %q", apiErr.Type, c.errType)
		}
		if apiErr.Message == "" {
			bad("SDK parsed an empty message")
		}
		h := apiErr.Response.Header
		if got := h.Get("x-should-retry"); got != c.shouldRetry {
			bad("x-should-retry %q, want %q", got, c.shouldRetry)
		}
		if got := h.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			bad("Content-Type %q, want application/json", got)
		}
		for k, want := range c.headers {
			if got := h.Get(k); got != want {
				bad("%s %q, want %q", k, got, want)
			}
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			bad("wire body is not JSON: %v (%q)", err, body)
			continue
		}
		if err := schema.Validate(v); err != nil {
			bad("wire body fails ErrorResponse: %v", err)
		}
	}
	return out
}

// TestConformanceGoSDK is the masterplan gate "Harness rot auf leerem
// Server": every case parses in the Go SDK with its code, x-should-retry
// stops the SDK's retries, and every body is a valid ErrorResponse.
func TestConformanceGoSDK(t *testing.T) {
	for _, v := range goSDKViolations(t, oaiWriters()) {
		t.Error(v)
	}
}

// TestConformanceGoSDKRejectsHouseFormat keeps the gate's red side
// reproducible: the same harness against the house forms the tree answered
// with before W02-02 must find violations in every case — otherwise the
// harness has lost its teeth. The violations are logged (go test -v).
func TestConformanceGoSDKRejectsHouseFormat(t *testing.T) {
	violations := goSDKViolations(t, houseWriters())
	for _, v := range violations {
		t.Log(v)
	}
	for _, c := range sdkCases {
		found := false
		for _, v := range violations {
			if strings.HasPrefix(v, c.name+": ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: the house form passes the harness — it no longer tells the envelope from the house format", c.name)
		}
	}
}

// conformanceModels is the model fixture both SDK halves list: an app
// consumer of acme holding grants on the tenant alias "chat" and on the
// _global "qwen/x" (slash id); "embed", "rank", the foreign, disabled and
// broken aliases are not released to it.
func conformanceModels(t *testing.T) http.Handler {
	t.Helper()
	f := &fakeDeps{ca: appConsumer(), reg: fixtureRegistry(t, fixtureRows()), grants: map[string]bool{idTenantChat: true, idSlash: true}}
	return modelsRouter(f.deps())
}

// conformanceModelIDs are the ids the fixture releases, in list order.
var conformanceModelIDs = []string{"chat", "qwen/x"}

// TestConformanceGoSDKModels (W02-05, G1): the Go SDK lists exactly the
// released fixture aliases and retrieves the slash id; the list and every
// element are valid against the vendored ListModelsResponse / Model, and
// the ctx extension reaches the SDK as an extra field instead of failing it.
func TestConformanceGoSDKModels(t *testing.T) {
	h := conformanceModels(t)
	var mu sync.Mutex
	bodies := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &captureWriter{ResponseWriter: w}
		h.ServeHTTP(cw, r)
		mu.Lock()
		bodies[r.URL.EscapedPath()] = cw.buf.Bytes()
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	client := openai.NewClient(option.WithBaseURL(srv.URL+"/v1/"), option.WithAPIKey("ctxm_conformance"), option.WithMaxRetries(0))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	page, err := client.Models.List(ctx)
	if err != nil {
		t.Fatalf("Models.List: %v", err)
	}
	var got []string
	for _, m := range page.Data {
		got = append(got, m.ID)
		if m.Object != "model" || m.OwnedBy != "ctx" || m.Created != aliasCreated.Unix() {
			t.Errorf("%s: object %q owned_by %q created %d", m.ID, m.Object, m.OwnedBy, m.Created)
		}
		if _, ok := m.JSON.ExtraFields["ctx"]; !ok {
			t.Errorf("%s: the ctx extension did not reach the SDK as an extra field", m.ID)
		}
	}
	if strings.Join(got, ",") != strings.Join(conformanceModelIDs, ",") {
		t.Errorf("Models.List = %v, want %v", got, conformanceModelIDs)
	}
	one, err := client.Models.Get(ctx, "qwen/x")
	if err != nil {
		t.Fatalf(`Models.Get("qwen/x"): %v`, err)
	}
	if one.ID != "qwen/x" {
		t.Errorf(`Models.Get("qwen/x") = %q`, one.ID)
	}
	_, err = client.Models.Get(ctx, "embed")
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != oai.CodeModelNotFound {
		t.Errorf(`Models.Get("embed") (not granted) = %v, want *openai.Error 404 model_not_found`, err)
	}

	list := loadSchema(t, listModelsExcerpt, "ListModelsResponse")
	model := loadSchema(t, listModelsExcerpt, "Model")
	mu.Lock()
	defer mu.Unlock()
	for path, schema := range map[string]*jsonschema.Resolved{"/v1/models": list, "/v1/models/qwen%2Fx": model} {
		var v any
		if err := json.Unmarshal(bodies[path], &v); err != nil {
			t.Errorf("%s: wire body %q is not JSON: %v", path, bodies[path], err)
			continue
		}
		if err := schema.Validate(v); err != nil {
			t.Errorf("%s: wire body fails the vendored schema: %v", path, err)
		}
	}
}
