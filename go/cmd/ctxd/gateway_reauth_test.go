package main

// The production binding of the model-key re-check of running requests
// (W02-08; design/02 §4.11.3 D6, masterplan K22): gatewayDeps hands the
// chat route the door's own handler.ReauthConsumer — never a wrapper, never
// the knowledge credential path — and a route without a re-check refuses to
// serve (fail-closed, oai.Deps.inferenceMissing).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestGatewayDepsBindsReauthConsumer pins Deps.Reauth to
// handler.ReauthConsumer by function pointer. The pin can fail: a wrapper
// around the same function has a pointer of its own.
func TestGatewayDepsBindsReauthConsumer(t *testing.T) {
	d := gatewayDeps(context.Background(), nil, nil, nil, nil, nil, nil, nil)
	if d.Reauth == nil {
		t.Fatal("gatewayDeps leaves Reauth unset")
	}
	want := reflect.ValueOf(handler.ReauthConsumer).Pointer()
	if got := reflect.ValueOf(d.Reauth).Pointer(); got != want {
		t.Fatalf("Deps.Reauth is %#x, want handler.ReauthConsumer %#x", got, want)
	}
	wrapper := func(ctx context.Context, ca *auth.ConsumerAuth) (*auth.ConsumerAuth, error) { return handler.ReauthConsumer(ctx, ca) }
	if reflect.ValueOf(wrapper).Pointer() == want {
		t.Fatal("a wrapper compares equal to handler.ReauthConsumer — the pin could not see a wrong binding")
	}
}

// TestChatRouteWithoutReauthIsInternal: the production dependencies with
// Reauth removed make the chat route answer 503 internal_error,
// x-should-retry false, and log Reauth as the one missing dependency. The
// control with the production Reauth passes that check (the body's own 400).
func TestChatRouteWithoutReauthIsInternal(t *testing.T) {
	disp := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	d := gatewayDeps(context.Background(), nil, config.NewStore(config.Defaults()), modelalias.NewRegistry(nil),
		backends.NewPool(nil, nil), backends.NewQuotaAccountant(nil, time.Hour), disp, nil)
	// No door in a unit test: the consumer the door would have put into the
	// request context.
	ca := &auth.ConsumerAuth{IsValid: true, ConsumerID: "c-w02-08", TenantID: "t-w02-08", ModelKeyID: "k-w02-08", AllModels: true}
	d.Consumer = func(context.Context) *auth.ConsumerAuth { return ca }

	serve := func(d oai.Deps) (*httptest.ResponseRecorder, string) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		defer slog.SetDefault(prev)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		oai.ChatCompletions(d).ServeHTTP(rec, req)
		return rec, logs.String()
	}
	code := func(rec *httptest.ResponseRecorder) string {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e.Error.Code
	}

	ok, okLogs := serve(d)
	t.Logf("production Reauth: %d %s", ok.Code, strings.TrimSpace(ok.Body.String()))
	if strings.Contains(okLogs, "inference dependency missing") || (ok.Code == http.StatusServiceUnavailable && code(ok) == oai.CodeInternalError) {
		t.Fatalf("control: the complete production deps were refused as incomplete: %d %s %s", ok.Code, ok.Body.String(), okLogs)
	}

	d.Reauth = nil
	rec, logs := serve(d)
	t.Logf("without Reauth: %d x-should-retry=%q %s; log %s", rec.Code, rec.Header().Get("x-should-retry"), strings.TrimSpace(rec.Body.String()), strings.TrimSpace(logs))
	if rec.Code != http.StatusServiceUnavailable || code(rec) != oai.CodeInternalError || rec.Header().Get("x-should-retry") != "false" {
		t.Errorf("without Reauth: %d %q x-should-retry=%q, want 503 internal_error, x-should-retry false", rec.Code, code(rec), rec.Header().Get("x-should-retry"))
	}
	if !strings.Contains(logs, `"missing":"Reauth"`) {
		t.Errorf("the refusal does not name Reauth as the one missing dependency: %s", logs)
	}
}
