//go:build integration

// W01-6 (K23): the door gate over mountModelGroup itself, for /v1 and /v2
// with one shared door — the form W02-05's mount PR runs with the real
// handlers.
//
//	go test -tags=integration -p 1 -count=1 -run TestModelGroup -v ./cmd/ctxd/
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1door"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type groupUsage struct{}

func (groupUsage) Touch(string) {}

func TestModelGroupDoor_Integration(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, knowledge, err := store.CreateApiKey(ctx, seed, "group-knowledge", "private", nil, "")
	if err != nil {
		t.Fatalf("knowledge key: %v", err)
	}
	_, _, model, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "group"}, nil, store.APIStamp(admin.ID, "group"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	tracer := &v1door.KnowledgeAuthTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var enabled atomic.Bool
	base := config.Defaults().GatewaySettings()
	get := func() oai.Settings { s := base; s.Enabled = enabled.Load(); return s }
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: pool, Settings: get, Usage: groupUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	gate := oai.EnabledGate(get)

	mount := func(r chi.Router, probe http.Handler) {
		mountModelGroup(r, "/v1", modelGroupDeps{gate: gate, door: door, routes: []modelRoute{
			{http.MethodGet, "/models", probe},
			{http.MethodGet, "/models/{model}", probe},
			{http.MethodPost, "/chat/completions", probe},
		}})
		mountModelGroup(r, "/v2", modelGroupDeps{gate: gate, door: door, routes: []modelRoute{
			{http.MethodPost, "/rerank", probe},
		}})
	}
	v1door.Assert(t, mount, v1door.Options{
		SetEnabled: enabled.Store, KnowledgeKey: knowledge, KnowledgeAuth: tracer, ModelKey: model.Plaintext(),
	})
}

// TestModelGroupRefusesMissingDeps: a group without gate or door is a
// wiring bug and panics at mount time instead of serving an open group.
func TestModelGroupRefusesMissingDeps(t *testing.T) {
	for name, deps := range map[string]modelGroupDeps{
		"no gate": {door: &handler.ModelDoor{}},
		"no door": {gate: func(h http.Handler) http.Handler { return h }},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: mountModelGroup did not panic", name)
				}
			}()
			mountModelGroup(chi.NewRouter(), "/v1", deps)
		}()
	}
}
