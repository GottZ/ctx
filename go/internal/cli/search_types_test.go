package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestSearchSendsTypes: `ctx search` takes types:<a,b> like tags:<a,b> and sends
// them as the request's `types` (issue #342). With the server's
// search.leave_out_never_ranked on, a full-text search names the type of a
// never-ranked block this way (`ctx search index query:topic-map
// types:system-meta`, docs/cli.md). Without the argument the request body is
// the one the command sent before it existed: no `types` key.
func TestSearchSendsTypes(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("search body %s: %v", raw, err)
			}
			bodies = append(bodies, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"count":0,"results":[]}`))
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", "fixture-key")
	t.Setenv("NO_COLOR", "1")

	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"search", "index", "query:topic-map", "types:system-meta"},
			map[string]any{"category": "index", "query": "topic-map", "types": []any{"system-meta"}, "limit": float64(10), "compact": true}},
		{[]string{"search", "query:Compaction checkpoint head", "types:checkpoint,insight"},
			map[string]any{"query": "Compaction checkpoint head", "types": []any{"checkpoint", "insight"}, "limit": float64(10), "compact": true}},
		{[]string{"search", "index", "query:topic-map"},
			map[string]any{"category": "index", "query": "topic-map", "limit": float64(10), "compact": true}},
	} {
		bodies = nil
		if _, err := runCommandCaptured(t, tc.args); err != nil {
			t.Fatalf("ctx %v: %v", tc.args, err)
		}
		if len(bodies) != 1 || !reflect.DeepEqual(bodies[0], tc.want) {
			t.Errorf("ctx %v sent %v, want %v", tc.args, bodies, tc.want)
		}
	}
}
