package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The forge conflict commands of issue #509 — conflicts, resolve,
// stamp-reset — against a fake server: the manage request each one sends,
// the raw JSON in a pipe, the rendering on a terminal (with the foreign text
// through the escape allowlist), and exit 1 on a success:false envelope.

// manageCapture is a fake /api/manage that records each request and answers
// with body under status.
type manageCapture struct {
	mu       sync.Mutex
	requests []map[string]any
}

func (m *manageCapture) server(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/manage" {
			t.Errorf("request %s %s, want POST /api/manage", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("request body %q: %v", raw, err)
		}
		m.mu.Lock()
		m.requests = append(m.requests, req)
		m.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// only returns the one request the command sent, as action and data.
func (m *manageCapture) only(t *testing.T) (string, map[string]any) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) != 1 {
		t.Fatalf("%d requests, want 1: %v", len(m.requests), m.requests)
	}
	action, _ := m.requests[0]["action"].(string)
	data, _ := m.requests[0]["data"].(map[string]any)
	return action, data
}

const conflictListBody = `{"success":true,"project_id":"11111111-1111-1111-1111-111111111111","conflicts":[` +
	`{"block_id":"33333333-3333-3333-3333-333333333333","entity_kind":"issue","forge_id":5,"title":"#5 Crane\u001b]0;pwned\u0007 jib",` +
	`"flag":"0123456789abcdef","conflict_at":"2026-11-01T11:00:00.123456Z","cell":"diverged",` +
	`"block":{"hash":"b","fields":{"body":"body edited locally"}},"block_updated_at":"2026-11-01T10:30:00Z",` +
	`"base":{"hash":"a","fields":{"body":"forge body"}},` +
	`"forge":{"updated_at":"2026-11-01T12:00:00+01:00","seen":{"hash":"c","fields":{"body":"body edited on the forge"}},"seen_at":"2026-11-01T11:00:00Z","seen_current":true}}` +
	`],"next_cursor":"33333333-3333-3333-3333-333333333333"}`

func TestIssuesConflictsRequestAndPipe(t *testing.T) {
	var m manageCapture
	srv := m.server(t, http.StatusOK, conflictListBody)
	root := newIssuesRoot(srv)
	root.SetArgs([]string{"project", "issues", "conflicts", "--project", testProjectUUID, "--limit", "20", "--after", testBlockUUID})
	var execErr error
	got := captureStdout(t, func() { execErr = root.Execute() })
	if execErr != nil {
		t.Fatalf("conflicts: %v", execErr)
	}
	action, data := m.only(t)
	if action != "forge-conflict-list" || data["project_id"] != testProjectUUID || data["after"] != testBlockUUID || data["limit"] != float64(20) {
		t.Errorf("request %s %v, want forge-conflict-list with project_id, after and limit", action, data)
	}
	// The pipe gets the server's JSON verbatim, re-indented (the escapes of
	// the title stay escapes: the machine contract is not rewritten).
	var want bytes.Buffer
	if err := json.Indent(&want, []byte(conflictListBody), "", "  "); err != nil {
		t.Fatal(err)
	}
	if got != want.String()+"\n" {
		t.Errorf("pipe output:\n got %q\nwant %q", got, want.String()+"\n")
	}

	// Without --after and --limit the data carries neither.
	var m2 manageCapture
	srv2 := m2.server(t, http.StatusOK, conflictListBody)
	root2 := newIssuesRoot(srv2)
	root2.SetArgs([]string{"project", "issues", "conflicts", "--project", testProjectUUID})
	_ = captureStdout(t, func() { execErr = root2.Execute() })
	if execErr != nil {
		t.Fatalf("conflicts without flags: %v", execErr)
	}
	if _, data := m2.only(t); len(data) != 1 || data["project_id"] != testProjectUUID {
		t.Errorf("data %v, want project_id alone", data)
	}
}

func TestIssuesConflictsTTY(t *testing.T) {
	var err error
	got := captureStdout(t, func() { err = printConflicts([]byte(conflictListBody)) })
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if want := []string{"BLOCK", "KIND", "NR", "CELL", "FLAGGED", "FORGE_UPDATED", "TITLE", "FLAG"}; strings.Join(strings.Fields(lines[0]), " ") != strings.Join(want, " ") {
		t.Fatalf("header %q, want %v", lines[0], want)
	}
	row := lines[1]
	// The full block id: resolve and stamp-reset take it as their argument.
	for _, part := range []string{"33333333-3333-3333-3333-333333333333", "issue", "5", "diverged", "2026-11-01 11:00:00", "2026-11-01 11:00:00", "#5 Crane]0;pwned jib", "0123456789abcdef"} {
		if !strings.Contains(row, part) {
			t.Errorf("row %q lacks %q", row, part)
		}
	}
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("an escape sequence of the title reached the terminal: %q", got)
	}
	if !strings.Contains(got, "next page: --after 33333333-3333-3333-3333-333333333333") {
		t.Errorf("no next-page hint in %q", got)
	}

	got = captureStdout(t, func() { err = printConflicts([]byte(`{"success":true,"conflicts":[],"next_cursor":null}`)) })
	if err != nil || got != "No conflicts.\n" {
		t.Errorf("empty list: %q %v, want \"No conflicts.\" and no hint", got, err)
	}
}

func TestIssuesResolveRequestAndPipe(t *testing.T) {
	const body = `{"success":true,"project_id":"p","result":{"block_id":"` + testBlockUUID + `","direction":"remote","changed":true,"leg":"issues","held_since":"2026-11-01T11:00:00Z"}}`
	var m manageCapture
	srv := m.server(t, http.StatusOK, body)
	root := newIssuesRoot(srv)
	root.SetArgs([]string{"project", "issues", "resolve", testBlockUUID, "--keep", "remote", "--expect", "0123456789abcdef", "--project", testProjectUUID})
	var execErr error
	got := captureStdout(t, func() { execErr = root.Execute() })
	if execErr != nil {
		t.Fatalf("resolve: %v", execErr)
	}
	action, data := m.only(t)
	if action != "forge-conflict-resolve" || data["project_id"] != testProjectUUID || data["block_id"] != testBlockUUID ||
		data["direction"] != "remote" || data["expect"] != "0123456789abcdef" || len(data) != 4 {
		t.Errorf("request %s %v, want forge-conflict-resolve with project_id, block_id, direction remote and expect", action, data)
	}
	if !strings.Contains(got, `"direction": "remote"`) || !strings.Contains(got, `"changed": true`) {
		t.Errorf("pipe output %q, want the server JSON", got)
	}
}

func TestIssuesResolveFlagsCheckedBeforeAnyRequest(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"without --keep", []string{"project", "issues", "resolve", testBlockUUID, "--project", testProjectUUID}, "keep"},
		{"--keep both", []string{"project", "issues", "resolve", testBlockUUID, "--keep", "both", "--project", testProjectUUID}, "local or remote"},
		{"without block id", []string{"project", "issues", "resolve", "--keep", "local", "--project", testProjectUUID}, "arg"},
		{"stamp-reset without block id or --all", []string{"project", "issues", "stamp-reset", "--project", testProjectUUID}, "exactly one"},
		{"stamp-reset with block id and --all", []string{"project", "issues", "stamp-reset", testBlockUUID, "--all", "--project", testProjectUUID}, "exactly one"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var m manageCapture
			srv := m.server(t, http.StatusOK, `{"success":true}`)
			root := newIssuesRoot(srv)
			root.SetArgs(c.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want one naming %q", err, c.want)
			}
			if len(m.requests) != 0 {
				t.Errorf("%d request(s) sent, want none", len(m.requests))
			}
		})
	}
}

func TestIssuesResolveTTY(t *testing.T) {
	id := "33333333-3333-3333-3333-333333333333"
	for _, c := range []struct {
		name, result, want string
	}{
		{"local on the forge version seen", `{"block_id":"` + id + `","direction":"local","changed":true,"basis":"forge_seen"}`,
			"33333333 resolved, keep local: the next push sends the block over the forge version the flag recorded\n"},
		{"local on the row's base", `{"block_id":"` + id + `","direction":"local","changed":true,"basis":"base"}`,
			"33333333 resolved, keep local: the next push sends the block's changes against the last synced version\n"},
		{"local on an unknown base", `{"block_id":"` + id + `","direction":"local","changed":true,"basis":"unknown"}`,
			"33333333 resolved, keep local: the forge side is not known, the next push sends every pushed field\n"},
		{"remote", `{"block_id":"` + id + `","direction":"remote","changed":true,"leg":"issues","held_since":"2026-11-01T11:00:00Z"}`,
			"33333333 resolved, keep remote: the next pull writes the forge version over the block (issues listed again from 2026-11-01 11:00:00)\n"},
		{"not flagged", `{"block_id":"` + id + `","direction":"remote","changed":false}`,
			"33333333: not flagged — nothing changed\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var err error
			got := captureStdout(t, func() { err = printResolved([]byte(`{"success":true,"result":` + c.result + `}`)) })
			if err != nil || got != c.want {
				t.Errorf("got %q %v, want %q", got, err, c.want)
			}
		})
	}
}

func TestIssuesStampResetRequestAndPipe(t *testing.T) {
	const body = `{"success":true,"project_id":"p","reset":1,"legs":["issues"]}`
	for _, c := range []struct {
		name     string
		args     []string
		wantData map[string]any
	}{
		{"one mapping", []string{"project", "issues", "stamp-reset", testBlockUUID, "--project", testProjectUUID},
			map[string]any{"project_id": testProjectUUID, "block_id": testBlockUUID}},
		{"all of the project", []string{"project", "issues", "stamp-reset", "--all", "--project", testProjectUUID},
			map[string]any{"project_id": testProjectUUID, "all": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var m manageCapture
			srv := m.server(t, http.StatusOK, body)
			root := newIssuesRoot(srv)
			root.SetArgs(c.args)
			var execErr error
			got := captureStdout(t, func() { execErr = root.Execute() })
			if execErr != nil {
				t.Fatalf("stamp-reset: %v", execErr)
			}
			action, data := m.only(t)
			if action != "forge-stamp-reset" || len(data) != len(c.wantData) {
				t.Fatalf("request %s %v, want forge-stamp-reset %v", action, data, c.wantData)
			}
			for k, v := range c.wantData {
				if data[k] != v {
					t.Errorf("data[%s] = %v, want %v", k, data[k], v)
				}
			}
			if !strings.Contains(got, `"reset": 1`) {
				t.Errorf("pipe output %q, want the server JSON", got)
			}
		})
	}
}

func TestIssuesStampResetTTY(t *testing.T) {
	var err error
	got := captureStdout(t, func() { err = printStampReset([]byte(`{"success":true,"reset":2,"legs":["comments","issues"]}`)) })
	if err != nil || got != "forge stamp cleared on 2 mapping(s); the next run lists in full: comments, issues\n" {
		t.Errorf("got %q %v", got, err)
	}
	got = captureStdout(t, func() { err = printStampReset([]byte(`{"success":true,"reset":0,"legs":[]}`)) })
	if err != nil || got != "forge stamp cleared on 0 mapping(s)\n" {
		t.Errorf("no mapping: got %q %v", got, err)
	}
}

// TestIssuesForgeCommandsExitOnFailure: a success:false envelope is exit 1
// with the server's reason, and nothing on stdout.
func TestIssuesForgeCommandsExitOnFailure(t *testing.T) {
	const refusal = `{"success":false,"error":"the flag changed since it was listed — list the conflicts again and decide on the current flag","flag":"fedcba9876543210"}`
	for _, args := range [][]string{
		{"project", "issues", "conflicts", "--project", testProjectUUID},
		{"project", "issues", "resolve", testBlockUUID, "--keep", "local", "--expect", "0123456789abcdef", "--project", testProjectUUID},
		{"project", "issues", "stamp-reset", testBlockUUID, "--project", testProjectUUID},
	} {
		t.Run(args[2], func(t *testing.T) {
			var m manageCapture
			srv := m.server(t, http.StatusConflict, refusal)
			root := newIssuesRoot(srv)
			root.SetArgs(args)
			var err error
			got := captureStdout(t, func() { err = root.Execute() })
			if err == nil || !strings.Contains(err.Error(), "the flag changed since it was listed") {
				t.Errorf("error %v, want the server's reason (exit 1)", err)
			}
			if got != "" {
				t.Errorf("stdout %q, want nothing on a refused call", got)
			}
		})
	}
}
