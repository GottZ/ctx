package handler

// F-W8 (design/04 §4.6, §7.2 no. 1): "/v1 only with its ingress protection"
// is code — a settings write that would turn gateway.enabled on is a 409
// while the router's federation ingress is not registered. Gate G4, unit
// half: the refusal happens before any database work (nil pool), so no
// override row can have been written. The DB half (row probe, 200 with a
// registered ingress, two routers in one process) is
// settings_ingress_integration_test.go.
//
//	go test -count=1 -run 'TestGatewayEnableRequiresIngress' -v ./internal/handler/

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
)

// ingressSettingsServe mounts the production settings chain over a nil pool
// with the given ingress registration and fires one admin request. env is
// CTX_GATEWAY_ENABLED at request time (set after testCfgStore emptied the
// registry env — the value a DELETE falls back to). reached reports that the
// request got past the ingress check into the database layer (the nil pool
// panics there) or the reload.
func ingressSettingsServe(t *testing.T, in *fedhop.Ingress, env, method, path, body string) (rec *httptest.ResponseRecorder, reached bool) {
	t.Helper()
	cfg := testCfgStore(t)
	t.Setenv("CTX_GATEWAY_ENABLED", env)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, adminAR())))
		})
	})
	h := &SettingsHandler{pool: nil, cfg: cfg, reload: func(*http.Request) error { reached = true; return nil }}
	MountSettings(r, h.WithIngress(in))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				reached = true
			}
		}()
		r.ServeHTTP(rec, req)
	}()
	return rec, reached
}

func TestGatewayEnableRequiresIngress(t *testing.T) {
	const wantText = "the model API cannot be enabled before its federation ingress is mounted"
	marked := &fedhop.Ingress{}
	marked.MarkLoopGuard()
	marked.MarkPeerIngress()
	// guardOnly is a router whose model group carries the LoopGuard but not
	// the PeerIngress (F-W9 G8): not registered, the write is refused.
	guardOnly := &fedhop.Ingress{}
	guardOnly.MarkLoopGuard()
	refused := func(t *testing.T, rec *httptest.ResponseRecorder, reached bool) {
		t.Helper()
		if reached {
			t.Fatalf("the write reached the database layer (status %d) — want a 409 before any row", rec.Code)
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body.Bytes())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.Bytes(), err)
		}
		if resp["success"] != false || !strings.Contains(resp["error"].(string), wantText) {
			t.Errorf("body %s, want the house envelope with %q", rec.Body.Bytes(), wantText)
		}
	}

	for name, in := range map[string]*fedhop.Ingress{"nil ingress": nil, "unmarked ingress": {}, "LoopGuard without PeerIngress": guardOnly} {
		t.Run("PUT true/"+name, func(t *testing.T) {
			rec, reached := ingressSettingsServe(t, in, "", http.MethodPut, "/api/settings/gateway.enabled", `{"value":true}`)
			refused(t, rec, reached)
		})
		t.Run("PUT \"true\"/"+name, func(t *testing.T) {
			rec, reached := ingressSettingsServe(t, in, "", http.MethodPut, "/api/settings/gateway.enabled", `{"value":"true"}`)
			refused(t, rec, reached)
		})
	}
	t.Run("PUT false passes the check", func(t *testing.T) {
		if rec, reached := ingressSettingsServe(t, &fedhop.Ingress{}, "", http.MethodPut, "/api/settings/gateway.enabled", `{"value":false}`); !reached {
			t.Fatalf("turning the API off was refused (%d %s) — only turning it on needs the ingress", rec.Code, rec.Body.Bytes())
		}
	})
	t.Run("PUT true with a registered ingress passes the check", func(t *testing.T) {
		if rec, reached := ingressSettingsServe(t, marked, "", http.MethodPut, "/api/settings/gateway.enabled", `{"value":true}`); !reached {
			t.Fatalf("refused with a registered ingress: %d %s", rec.Code, rec.Body.Bytes())
		}
	})
	t.Run("other keys are untouched", func(t *testing.T) {
		if rec, reached := ingressSettingsServe(t, nil, "", http.MethodPut, "/api/settings/gateway.relay_upstream_4xx_messages", `{"value":true}`); !reached {
			t.Fatalf("an unguarded bool was refused: %d %s", rec.Code, rec.Body.Bytes())
		}
	})

	// DELETE of the _global override falls back to the env value: with
	// CTX_GATEWAY_ENABLED=true that turns the API on, so it is refused the
	// same way; with the env off it is an ordinary delete.
	t.Run("DELETE that would fall back to env true", func(t *testing.T) {
		rec, reached := ingressSettingsServe(t, nil, "true", http.MethodDelete, "/api/settings/gateway.enabled", "")
		refused(t, rec, reached)
	})
	t.Run("DELETE that would fall back to env true, LoopGuard without PeerIngress", func(t *testing.T) {
		rec, reached := ingressSettingsServe(t, guardOnly, "true", http.MethodDelete, "/api/settings/gateway.enabled", "")
		refused(t, rec, reached)
	})
	t.Run("DELETE that falls back to env false passes the check", func(t *testing.T) {
		if rec, reached := ingressSettingsServe(t, nil, "false", http.MethodDelete, "/api/settings/gateway.enabled", ""); !reached {
			t.Fatalf("refused although the env value is off: %d %s", rec.Code, rec.Body.Bytes())
		}
	})
	t.Run("DELETE with a registered ingress passes the check", func(t *testing.T) {
		if rec, reached := ingressSettingsServe(t, marked, "true", http.MethodDelete, "/api/settings/gateway.enabled", ""); !reached {
			t.Fatalf("refused with a registered ingress: %d %s", rec.Code, rec.Body.Bytes())
		}
	})
}

// TestGatewayEnabledCarriesTheIngressGuard pins the registry half: the guard
// is a property of the key, not a name match in the handler.
func TestGatewayEnabledCarriesTheIngressGuard(t *testing.T) {
	info, ok := config.KeyByName("gateway.enabled")
	if !ok || info.Guard != config.GuardFederationIngress {
		t.Fatalf("gateway.enabled guard = %q (found %v), want %q", info.Guard, ok, config.GuardFederationIngress)
	}
}
