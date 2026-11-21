//go:build integration

package handler

// Issue #453 at real routes over a real database, on a real httptest server
// whose client gives up:
//
//   - model-alias-preview (point 3): the chain dry run waits on the request
//     context; its line went out through slog.ErrorContext, which the census
//     of #417 did not see, and stayed ERROR on a client abort.
//
//	go test -tags=integration ./internal/handler/ -run 'TestClientAbortFollowups' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/testdb"
)

// abortManage serves h's /api/manage as ar behind the production RequestID
// and Logger stages and gives up on the call once parked reports that the
// route waits.
func abortManage(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action string, data any, parked <-chan struct{}) <-chan struct{} {
	t.Helper()
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleManage(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}), nil)
	body, err := json.Marshal(map[string]any{"action": action, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/manage", strings.NewReader(string(body)))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-parked
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
	return done
}

// TestClientAbortFollowupsAliasPreview — the client gives up while the chain
// dry run waits: the preview's line is INFO with client_gone=true, the
// request log says 499.
func TestClientAbortFollowupsAliasPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "pv-abort")
	c := mcCreate(t, h, admin, map[string]any{"label": "pv-abort-app", "tenant_id": a.id, "tenant_scope": a.scope})

	parked := make(chan struct{})
	h.SetChainPreview(func(pctx context.Context, _ *auth.ConsumerAuth, _ string) (oai.ChainPreview, error) {
		close(parked)
		<-pctx.Done()
		return oai.ChainPreview{}, fmt.Errorf("chain preview: %w", pctx.Err())
	}, func(string) string { return "" })

	logs := installRejectLogCapture(t)
	waitDone(t, abortManage(t, h, admin, "model-alias-preview", map[string]any{"alias": "chat", "consumer_id": c.id}, parked))

	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up on the preview produced ERROR lines: %+v", errs)
	}
	recs := logs.withMsg("model-alias-preview: chain dry run failed")
	if len(recs) != 1 || recs[0].level >= slog.LevelError || recs[0].attrs["client_gone"] != true {
		t.Errorf("preview line = %+v, want one INFO line with client_gone=true", recs)
	}
}
