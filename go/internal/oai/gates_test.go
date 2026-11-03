package oai_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestEnabledGateOffIsOneAnswer: with the flag off every request — any
// method, path, credential or body — gets the same 404 bytes and headers,
// and nothing behind the gate runs (masterplan K4: no oracle, no DB work).
func TestEnabledGateOffIsOneAnswer(t *testing.T) {
	var behind atomic.Int64
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		behind.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := oai.EnabledGate(func() oai.Settings { return oai.Settings{Enabled: false} })(next)

	reqs := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`)),
		httptest.NewRequest(http.MethodDelete, "/v2/rerank", nil),
		httptest.NewRequest(http.MethodGet, "/v1/does/not/exist", nil),
	}
	reqs[1].Header.Set("Authorization", "Bearer ctxm_0123456789abcdef")
	reqs[2].Header.Set("X-Context-Key", strings.Repeat("a", 64))
	reqs[3].AddCookie(&http.Cookie{Name: "ctx_session", Value: "s"})

	var first *httptest.ResponseRecorder
	for i, req := range reqs {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("request %d: status %d, want 404", i, rec.Code)
		}
		if first == nil {
			first = rec
			continue
		}
		if !bytes.Equal(rec.Body.Bytes(), first.Body.Bytes()) {
			t.Errorf("request %d: body %q differs from %q", i, rec.Body.String(), first.Body.String())
		}
		for k, v := range first.Header() {
			if got := rec.Header().Values(k); strings.Join(got, ",") != strings.Join(v, ",") {
				t.Errorf("request %d: header %s %v differs from %v", i, k, got, v)
			}
		}
	}
	if n := behind.Load(); n != 0 {
		t.Errorf("%d request(s) passed the closed gate", n)
	}

	// The closed gate answers exactly like the router's own 404 (the
	// catch-all W01-6 mounts): one shape whether the gateway is off or the
	// path is unknown.
	nf := httptest.NewRecorder()
	oai.NotFound(nf, nil)
	if !bytes.Equal(nf.Body.Bytes(), first.Body.Bytes()) {
		t.Errorf("gate 404 %q differs from oai.NotFound %q", first.Body.String(), nf.Body.String())
	}
}

// TestEnabledGateIsHot: the flag is read per request, so turning it on (or
// off) takes effect with the next request, no restart.
func TestEnabledGateIsHot(t *testing.T) {
	var on atomic.Bool
	h := oai.EnabledGate(func() oai.Settings { return oai.Settings{Enabled: on.Load()} })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	probe := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		return rec.Code
	}
	if got := probe(); got != http.StatusNotFound {
		t.Errorf("flag off: status %d, want 404", got)
	}
	on.Store(true)
	if got := probe(); got != http.StatusTeapot {
		t.Errorf("flag on: status %d, want the next handler's 418", got)
	}
	on.Store(false)
	if got := probe(); got != http.StatusNotFound {
		t.Errorf("flag off again: status %d, want 404", got)
	}
}

// TestEnabledGateNilGetterPanics: a gate without a settings source is a
// wiring bug and fails at construction, not open at request time.
func TestEnabledGateNilGetterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("EnabledGate(nil) did not panic")
		}
	}()
	oai.EnabledGate(nil)
}
