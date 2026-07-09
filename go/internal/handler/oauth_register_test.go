package handler

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// dcrTestIPSeq hands every helper request a unique source IP: the W4b rate
// limiter is process-wide, so tests sharing httptest's default RemoteAddr
// would throttle EACH OTHER once the guard exists. Rate-limit tests pin
// their own RemoteAddr explicitly instead.
var dcrTestIPSeq atomic.Int64

func dcrTestAddr() string {
	n := dcrTestIPSeq.Add(1)
	return fmt.Sprintf("10.99.%d.%d:4242", n/250, n%250+1)
}

// These probes pin the 02-W4a fail-closed surface that returns BEFORE any DB
// access — mode gate, body cap, metadata validation. Every rejection is its
// own case (negative-probed gate); the 201 happy paths need a real client
// row and live in oauth_register_integration_test.go.

func dcrPost(t *testing.T, h *OAuthHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = dcrTestAddr()
	rec := httptest.NewRecorder()
	h.Register(rec, req)
	return rec
}

func dcrErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body decode: %v (body %q)", err, rec.Body.String())
	}
	return e["error"]
}

func TestDCRRegister_ModeOff404(t *testing.T) {
	t.Setenv(EnvDCRMode, "off")
	h := &OAuthHandler{}
	if rec := dcrPost(t, h, `{"redirect_uris":["https://client.example/cb"]}`); rec.Code != 404 {
		t.Errorf("off mode: status = %d, want 404", rec.Code)
	}
}

func TestDCRRegister_ModeUnsetFailsClosed404(t *testing.T) {
	t.Setenv(EnvDCRMode, "")
	h := &OAuthHandler{}
	if rec := dcrPost(t, h, `{"redirect_uris":["https://client.example/cb"]}`); rec.Code != 404 {
		t.Errorf("unset mode: status = %d, want 404 (fail-closed off)", rec.Code)
	}
}

func TestDCRRegister_AdminModeWithoutKey401(t *testing.T) {
	t.Setenv(EnvDCRMode, "admin")
	h := &OAuthHandler{} // nil pool: Authenticate("") early-returns invalid without DB
	rec := dcrPost(t, h, `{"redirect_uris":["https://client.example/cb"]}`)
	if rec.Code != 401 {
		t.Errorf("admin mode without key: status = %d, want 401", rec.Code)
	}
}

func TestDCRRegister_EmptyRedirectURIs400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"redirect_uris":[]}`)
	if rec.Code != 400 {
		t.Fatalf("empty redirect_uris: status = %d, want 400", rec.Code)
	}
	if code := dcrErrorCode(t, rec); code != "invalid_redirect_uri" {
		t.Errorf("error = %q, want invalid_redirect_uri", code)
	}
}

func TestDCRRegister_MissingRedirectURIs400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"client_name":"no uris"}`)
	if rec.Code != 400 {
		t.Fatalf("missing redirect_uris: status = %d, want 400", rec.Code)
	}
	if code := dcrErrorCode(t, rec); code != "invalid_redirect_uri" {
		t.Errorf("error = %q, want invalid_redirect_uri", code)
	}
}

// The §4b core fix: plain http on a NON-loopback host is a permanent
// plaintext code/key exfiltration channel once 03 matches this list —
// registration must refuse it.
func TestDCRRegister_HTTPForeignHost400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"redirect_uris":["http://attacker.example/cb"]}`)
	if rec.Code != 400 {
		t.Fatalf("http foreign host: status = %d, want 400", rec.Code)
	}
	if code := dcrErrorCode(t, rec); code != "invalid_redirect_uri" {
		t.Errorf("error = %q, want invalid_redirect_uri", code)
	}
}

func TestDCRRegister_MixedValidAndInvalidRedirect400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"redirect_uris":["https://ok.example/cb","http://attacker.example/cb"]}`)
	if rec.Code != 400 {
		t.Errorf("one bad uri poisons the set: status = %d, want 400", rec.Code)
	}
}

func TestDCRRegister_ResponseTypeToken400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"redirect_uris":["https://client.example/cb"],"response_types":["token"]}`)
	if rec.Code != 400 {
		t.Fatalf("response_types [token]: status = %d, want 400", rec.Code)
	}
	if code := dcrErrorCode(t, rec); code != "invalid_client_metadata" {
		t.Errorf("error = %q, want invalid_client_metadata", code)
	}
}

func TestDCRRegister_GrantTypeOutsideSuperset400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"redirect_uris":["https://client.example/cb"],"grant_types":["client_credentials"]}`)
	if rec.Code != 400 {
		t.Fatalf("grant_types [client_credentials]: status = %d, want 400", rec.Code)
	}
	if code := dcrErrorCode(t, rec); code != "invalid_client_metadata" {
		t.Errorf("error = %q, want invalid_client_metadata", code)
	}
}

func TestDCRRegister_PrivateKeyJWT400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	rec := dcrPost(t, h, `{"redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`)
	if rec.Code != 400 {
		t.Fatalf("private_key_jwt: status = %d, want 400", rec.Code)
	}
	if code := dcrErrorCode(t, rec); code != "invalid_client_metadata" {
		t.Errorf("error = %q, want invalid_client_metadata", code)
	}
}

func TestDCRRegister_MalformedJSON400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	if rec := dcrPost(t, h, `{not json`); rec.Code != 400 {
		t.Errorf("malformed JSON: status = %d, want 400", rec.Code)
	}
}

func TestDCRRegister_BodyOverCap400(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	h := &OAuthHandler{}
	// > 8192 bytes: pad a syntactically valid document past the cap so the
	// rejection can only come from MaxBytesReader, not the JSON syntax.
	body := `{"client_name":"` + strings.Repeat("a", 9000) + `","redirect_uris":["https://client.example/cb"]}`
	rec := dcrPost(t, h, body)
	if rec.Code != 400 && rec.Code != 413 {
		t.Errorf("oversized body: status = %d, want 400 or 413", rec.Code)
	}
}
