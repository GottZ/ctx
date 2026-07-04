package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const testProjectUUID = "11111111-1111-1111-1111-111111111111"
const testBlockUUID = "22222222-2222-2222-2222-222222222222"

// ── terminal-escape allowlist (§5.4 / §7-W8 gate) ─────────────────────────────.

// TestSanitizeTerminal is the W8 escape gate. It is RED under a bare \x1b
// blocklist (the 0x9B C1 CSI single byte, the \r line-overwrite, the BEL that
// terminates an OSC title hijack and a U+009B code point all slip through) and
// GREEN only under the allowlist: every C0 control except \n/\t, DEL 0x7f, and
// every C1 control (raw byte OR code point) is dropped.
func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// OSC title hijack: ESC ] 0 ; pwned BEL — ESC (0x1b) AND BEL (0x07) must go.
		{"osc_title_hijack", "safe\x1b]0;pwned\x07end", "safe]0;pwnedend"},
		// C1 CSI as a single raw byte 0x9b — the case a \x1b filter is too thin for.
		{"c1_csi_single_byte", "a\x9bb", "ab"},
		// C1 CSI as a proper code point U+009B (UTF-8 0xc2 0x9b).
		{"c1_csi_codepoint", "a\u009bb", "ab"},
		// DCS 0x90 and OSC 0x9d single bytes.
		{"c1_dcs_osc_bytes", "x\x90y\x9dz", "xyz"},
		// \r line overwrite.
		{"cr_overwrite", "line1\rline2", "line1line2"},
		// Allowlisted controls survive.
		{"keeps_newline_tab", "a\nb\tc", "a\nb\tc"},
		// Valid multibyte UTF-8 is preserved.
		{"keeps_utf8", "café — Größe ☃", "café — Größe ☃"},
		// DEL is stripped.
		{"strips_del", "a\x7fb", "ab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeTerminal(c.in); got != c.want {
				t.Errorf("sanitizeTerminal(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ── cobra-tree harness ────────────────────────────────────────────────────────.

// newIssuesRoot builds a minimal `ctx project issues …` cobra tree whose client
// points at srv, so a full command line exercises the real tree (Args, flags,
// stdin) — not a bypass to the run functions.
func newIssuesRoot(srv *httptest.Server) *cobra.Command {
	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	pc := &cobra.Command{Use: "project"}
	pc.AddCommand(issuesCmd(getClient))
	root.AddCommand(pc)
	return root
}

// withStdin swaps os.Stdin for a pipe carrying s for the duration of fn (a pipe
// is not a char device, so ReadStdin treats it as piped input).
func withStdin(t *testing.T, s string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	go func() {
		_, _ = io.WriteString(w, s)
		_ = w.Close()
	}()
	defer func() { os.Stdin = orig; _ = r.Close() }()
	fn()
}

// ── stdin body path (§7-W8 gate) ──────────────────────────────────────────────.

// TestIssuesCreateStdinBody drives `… issues create --title t` through the cobra
// tree with a piped body and asserts the server received it as content.
func TestIssuesCreateStdinBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte(`{"success":true,"render":"untrusted","issue":{"id":"` + testBlockUUID + `","title":"t","workflow_status":"open"}}`))
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	root := newIssuesRoot(srv)
	root.SetArgs([]string{"project", "issues", "create", "--project", testProjectUUID, "--title", "t"})

	var execErr error
	withStdin(t, "body from stdin\n", func() {
		execErr = root.Execute()
	})
	if execErr != nil {
		t.Fatalf("create with stdin body: %v", execErr)
	}
	if !strings.Contains(gotBody, `"content":"body from stdin"`) {
		t.Errorf("server body = %q, want it to carry content:\"body from stdin\"", gotBody)
	}
	if !strings.Contains(gotBody, `"title":"t"`) {
		t.Errorf("server body = %q, want title:\"t\"", gotBody)
	}
}

// ── exit 1 on success:false (§7-W8 gate) ──────────────────────────────────────.

func TestIssuesCreateExitOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"scope not writable"}`))
	}))
	defer srv.Close()

	root := newIssuesRoot(srv)
	root.SetArgs([]string{"project", "issues", "create", "--project", testProjectUUID, "--title", "t", "--body", "x"})
	err := root.Execute()
	if err == nil {
		t.Fatal("success:false ⇒ want a non-nil error (exit 1), got nil")
	}
	if !strings.Contains(err.Error(), "scope not writable") {
		t.Errorf("error = %q, want the server reason \"scope not writable\"", err)
	}
}

// ── status verb: invalid transition ⇒ 422 mapped cleanly (§7-W8 gate) ──────────.

func TestIssuesStatusInvalidTransition(t *testing.T) {
	var sawPatch bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			sawPatch = true
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"status":"bogus"`) {
				t.Errorf("PATCH body = %q, want status:\"bogus\"", string(b))
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"success":false,"error":"transition open→bogus is not allowed"}`))
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	root := newIssuesRoot(srv)
	root.SetArgs([]string{"project", "issues", "status", testBlockUUID, "bogus", "--project", testProjectUUID})
	err := root.Execute()
	if !sawPatch {
		t.Fatal("status verb did not issue a PATCH")
	}
	if err == nil {
		t.Fatal("invalid transition (422) ⇒ want a non-nil error (exit 1), got nil")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %q, want the server reason", err)
	}
}

// ── pipe JSON golden (§7-W8 gate) ─────────────────────────────────────────────.

// TestIssuesListPipeGolden freezes the piped list output byte-for-byte. stdout
// under `go test` is not a TTY, so the list takes the raw-JSON branch (PrintJSON
// re-indents the server body verbatim — a stable machine contract).
func TestIssuesListPipeGolden(t *testing.T) {
	const serverBody = `{"success":true,"render":"untrusted","issues":[{"id":"11111111-1111-1111-1111-111111111111","scope":"acme:widget","type_name":"issue","title":"Login broken","workflow_status":"open","updated_at":"2026-07-03T10:00:00Z"}],"cursor":null}`
	const want = `{
  "success": true,
  "render": "untrusted",
  "issues": [
    {
      "id": "11111111-1111-1111-1111-111111111111",
      "scope": "acme:widget",
      "type_name": "issue",
      "title": "Login broken",
      "workflow_status": "open",
      "updated_at": "2026-07-03T10:00:00Z"
    }
  ],
  "cursor": null
}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(serverBody))
	}))
	defer srv.Close()

	root := newIssuesRoot(srv)
	root.SetArgs([]string{"project", "issues", "list", "--project", testProjectUUID})

	var execErr error
	got := captureStdout(t, func() { execErr = root.Execute() })
	if execErr != nil {
		t.Fatalf("list: %v", execErr)
	}
	if got != want {
		t.Errorf("pipe golden mismatch:\n got: %q\nwant: %q", got, want)
	}
}
