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
	body := `{"urls":["https://e.com/a.png","/relative.png","data:image/png;base64,AAAA","mailto:x@y","https://e.com/b.jpg"]}`
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
