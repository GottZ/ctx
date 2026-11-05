//go:build integration

// W01-4 gate, SSE half (design/01 §4.3): both in-stream re-auth seams refuse a
// model key without a single knowledge-auth statement, and still resolve a
// 64-hex knowledge key through exactly one ctx_auth query.
//
//	go test -tags=integration -p 1 -run TestModelKeyShapeSSEReauth -count=1 -v ./internal/handler/
package handler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// sseProbeKey tags a probe's context; the tracer counts only statements
// issued under a tag, so a concurrent background query cannot leak into a
// probe's count.
type sseProbeKey struct{}

// sseAuthTracer records, per probe tag, every knowledge-auth statement:
// ctx_auth (raw key), ctx_auth_by_id (token, session) and any statement that
// names context_api_keys.
type sseAuthTracer struct {
	mu   sync.Mutex
	hits map[string][]string
}

func (s *sseAuthTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tag, ok := ctx.Value(sseProbeKey{}).(string)
	if !ok {
		return ctx
	}
	var family string
	switch {
	case strings.Contains(data.SQL, "ctx_auth_by_id("):
		family = "ctx_auth_by_id"
	case strings.Contains(data.SQL, "ctx_auth("):
		family = "ctx_auth"
	case strings.Contains(data.SQL, "context_api_keys"):
		family = "context_api_keys"
	default:
		return ctx
	}
	s.mu.Lock()
	s.hits[tag] = append(s.hits[tag], family)
	s.mu.Unlock()
	return ctx
}

func (s *sseAuthTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *sseAuthTracer) of(tag string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits[tag]...)
}

func TestModelKeyShapeSSEReauth_Integration(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	const hexKey = "a4112026110300000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	if created, _, err := store.BootstrapAdminKey(ctx, seed, hexKey, "w01-4-sse-probe"); err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	tracer := &sseAuthTracer{hits: map[string][]string{}}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)

	seams := []struct {
		name string
		auth func(ctx context.Context, key string, isSession bool) (*auth.AuthResult, error)
	}{
		{"sse-admin-reauth (events.go)", NewEventsHandler(ctx, pool, nil, nil).hub.authenticate},
		{"sse-project-reauth (project_events.go)", NewProjectEventsHandler(nil, pool, nil).authenticate},
	}
	for _, seam := range seams {
		for _, s := range modelKeyShapes(t) {
			tag := seam.name + "/" + s.name
			ar, err := seam.auth(context.WithValue(ctx, sseProbeKey{}, tag), s.raw, false)
			hits := tracer.of(tag)
			t.Logf("seam=%s shape=%s valid=%v err=%v knowledge_auth=%d %v", seam.name, s.name, ar != nil && ar.IsValid, err, len(hits), hits)
			if err != nil || ar == nil || ar.IsValid {
				t.Errorf("%s: result %+v err %v, want a plain invalid result", tag, ar, err)
			}
			if len(hits) != 0 {
				t.Errorf("%s: %d knowledge-auth statement(s) %v, want 0", tag, len(hits), hits)
			}
		}

		// Positive control: the 64-hex key still takes its one ctx_auth query.
		tag := seam.name + "/hex-key"
		ar, err := seam.auth(context.WithValue(ctx, sseProbeKey{}, tag), hexKey, false)
		hits := tracer.of(tag)
		t.Logf("seam=%s shape=hex-key valid=%v err=%v knowledge_auth=%d %v", seam.name, ar != nil && ar.IsValid, err, len(hits), hits)
		if err != nil || ar == nil || !ar.IsValid {
			t.Errorf("%s: result %+v err %v, want the valid admin key", tag, ar, err)
		}
		if fmt.Sprint(hits) != "[ctx_auth]" {
			t.Errorf("%s: knowledge-auth statements %v, want exactly [ctx_auth]", tag, hits)
		}
	}
}
