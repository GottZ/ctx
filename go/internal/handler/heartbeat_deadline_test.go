package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression: the heartbeat must outlive http.Server's ABSOLUTE WriteTimeout.
// The CPU synthesis fallback runs minutes; with WriteTimeout 120s the server
// canceled the request context at ~120s and the client received zero payload
// bytes (live: Welle-G failover E2E, 126s, 4 bytes = ticks only). Each tick
// now rolls the write deadline forward via http.ResponseController.
func TestHeartbeatOutlivesServerWriteTimeout(t *testing.T) {
	origInterval := heartbeatInterval
	origWindow := heartbeatWriteWindow
	heartbeatInterval = 50 * time.Millisecond
	heartbeatWriteWindow = 500 * time.Millisecond
	defer func() {
		heartbeatInterval = origInterval
		heartbeatWriteWindow = origWindow
	}()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hb := startHeartbeat(w, true)
		// Simulated slow synthesis: 3x the server WriteTimeout. The request
		// context must stay alive the whole time (it is canceled when the
		// write deadline kills the connection).
		select {
		case <-time.After(900 * time.Millisecond):
		case <-r.Context().Done():
			t.Error("request context canceled — write deadline was not extended")
		}
		hb.finish(http.StatusOK, map[string]any{"answer": "made-it"})
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body (connection killed by WriteTimeout?): %v", err)
	}
	if !strings.Contains(string(body), "made-it") {
		t.Errorf("payload missing, got %d bytes: %q", len(body), string(body))
	}
}
