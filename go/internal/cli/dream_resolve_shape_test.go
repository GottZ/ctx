// Issue #197 documents taking a declared supersedes back with
// `ctx dream resolve <new-id> <old-id> supersedes delete`. This pins the request
// that command sends: POST /api/manage with the resolve fields under `data`,
// the shape the docs give for the HTTP form.
//
//	go test -short ./internal/cli/ -run TestDreamResolveSendsTheDocumentedManageShape -count=1 -v
package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDreamResolveSendsTheDocumentedManageShape(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"resolution":"delete"}}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", "fixture-key")
	t.Setenv("NO_COLOR", "1")

	if _, err := runCommandCaptured(t, []string{"dream", "resolve", "new-id", "old-id", "supersedes", "delete"}); err != nil {
		t.Fatalf("ctx dream resolve: %v", err)
	}
	if path != "/api/manage" {
		t.Errorf("path = %q, want /api/manage", path)
	}
	data, _ := body["data"].(map[string]any)
	if body["action"] != "dream-link-resolve" || data == nil ||
		data["source_id"] != "new-id" || data["target_id"] != "old-id" ||
		data["relationship"] != "supersedes" || data["resolution"] != "delete" {
		t.Errorf("request body = %v, want action dream-link-resolve with source_id/target_id/relationship/resolution under data", body)
	}
}
