package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQueryRendersUntrustedOrigin (issue #280): the human output of `ctx query`
// puts the server's untrusted_origin record on a line of its own under the
// source it belongs to, in the wording the MCP surfaces use; a source without
// a record keeps its line and gets none.
func TestQueryRendersUntrustedOrigin(t *testing.T) {
	const body = `{"success":true,"answer":"an answer","confidence":"confident","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions","score":0.5,"age_days":3,"untrusted_origin":"tool-evidence"},` +
		`{"id":"id2","title":"T2","category":"projects","score":0.4,"age_days":1}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", "fixture-key")
	t.Chdir(t.TempDir())

	stdout, err := runCommandCaptured(t, []string{"query", "hello"})
	if err != nil {
		t.Fatalf("ctx query: %v (stdout %q)", err, stdout)
	}
	want := "  [1] T1 (decisions, 3d ago) id:id1\n" +
		"      untrusted_origin:tool-evidence (agent-produced text: re-typed out of a type marked untrusted)\n" +
		"  [2] T2 (projects, 1d ago) id:id2\n"
	if !strings.HasSuffix(stdout, want) {
		t.Errorf("stdout:\n%s\nwant it to end with:\n%s", stdout, want)
	}
}
