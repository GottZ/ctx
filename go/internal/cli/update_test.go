package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #408: `ctx update <id>` sends the fields of `manage update` from flags,
// so an update needs no JSON body built in the shell.

const updateOKAnswer = `{"action":"update","success":true,"block":{"id":"0197aaaa-0000-7000-8000-000000000001",` +
	`"category":"reference","tags":["a"],"title":"Release notes","content":"x","metadata":{},"scope":"private",` +
	`"created_at":"2026-11-20T10:00:00Z","updated_at":"2026-11-21T01:02:03.456789Z"}}`

const updateID = "0197aaaa-0000-7000-8000-000000000001"

// The incident this command exists to end: `ctx manage update <id>
// "$(jq -n --arg c "$(cat file)" '{content:$c}')"` with a file that was not
// there sent content "" and the update succeeded. A content file that is
// missing, unreadable or empty is an error before any request.
func TestUpdateContentFileRefusedBeforeAnyRequest(t *testing.T) {
	srv := newRecordedServer(t, func(map[string]any) string { return updateOKAnswer })
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newlineOnly := filepath.Join(dir, "newline.md")
	if err := os.WriteFile(newlineOnly, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blank := filepath.Join(dir, "blank.md")
	if err := os.WriteFile(blank, []byte("  \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		stdin *string
		args  []string
		want  string
	}{
		{"missing file", nil, []string{"update", updateID, "--content-file", filepath.Join(dir, "nope.md")}, "nope.md"},
		{"a directory", nil, []string{"update", updateID, "--content-file", dir}, dir},
		{"empty file", nil, []string{"update", updateID, "--content-file", empty}, "empty"},
		{"only a newline", nil, []string{"update", updateID, "--content-file", newlineOnly}, "empty"},
		{"only whitespace", nil, []string{"update", updateID, "--content-file", blank}, "empty"},
		{"empty stdin", strPtr(""), []string{"update", updateID, "--content-file", "-"}, "empty"},
		{"empty edits file", nil, []string{"update", updateID, "--edits-file", empty}, "empty"},
		{"missing edits file", nil, []string{"update", updateID, "--edits-file", filepath.Join(dir, "nope.json")}, "nope.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(srv.requests())
			stdout, _, err := runCLI(t, c.stdin, c.args...)
			if err == nil {
				t.Fatalf("`ctx %s` succeeded, want an error before any request", strings.Join(c.args, " "))
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to name %q", err, c.want)
			}
			if n := len(srv.requests()) - before; n != 0 {
				t.Fatalf("%d request(s) reached the server, want none", n)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing", stdout)
			}
		})
	}
}

// `--content-file -` reads piped text only: stdin on a terminal or a device
// (here /dev/null, a character device like a terminal) is refused, never read
// as an empty content.
func TestUpdateContentFromADeviceIsRefused(t *testing.T) {
	srv := newRecordedServer(t, func(map[string]any) string { return updateOKAnswer })
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	old := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = old }()

	stdout, _, err := runCLI(t, nil, "update", updateID, "--content-file", "-")
	if err == nil || !strings.Contains(err.Error(), "stdin is a terminal or a device") {
		t.Fatalf("err = %v, want the stdin refusal", err)
	}
	if n := len(srv.requests()); n != 0 || stdout != "" {
		t.Fatalf("%d request(s), stdout %q — want none and nothing", n, stdout)
	}
}

// Each flag lands in the `data` field of manage update under the server's
// name; flags left out are not sent.
func TestUpdateSendsTheFields(t *testing.T) {
	srv := newRecordedServer(t, func(map[string]any) string { return updateOKAnswer })
	dir := t.TempDir()
	content := filepath.Join(dir, "content.md")
	if err := os.WriteFile(content, []byte("# Notes\n\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	twoNewlines := filepath.Join(dir, "two.md")
	if err := os.WriteFile(twoNewlines, []byte("body\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edits := filepath.Join(dir, "edits.json")
	if err := os.WriteFile(edits, []byte(`[{"old":"x","new":"y","replace_all":true},{"old":"p","new":""}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		stdin *string
		args  []string
		data  string
	}{
		{"content file, one trailing newline dropped", nil,
			[]string{"--content-file", content}, `{"content":"# Notes\n\nline two"}`},
		{"only the last newline is dropped", nil,
			[]string{"--content-file", twoNewlines}, `{"content":"body\n"}`},
		{"content from stdin", strPtr("piped text\n"),
			[]string{"--content-file", "-"}, `{"content":"piped text"}`},
		{"edit pairs in order, an empty new deletes", nil,
			[]string{"--edit-old", "a", "--edit-old", "c", "--edit-new", "b", "--edit-new", ""},
			`{"edits":[{"old":"a","new":"b"},{"old":"c","new":""}]}`},
		{"replace-all marks every pair", nil,
			[]string{"--edit-old", "a", "--edit-new", "b", "--replace-all"},
			`{"edits":[{"old":"a","new":"b","replace_all":true}]}`},
		{"edits file", nil,
			[]string{"--edits-file", edits},
			`{"edits":[{"old":"x","new":"y","replace_all":true},{"old":"p","new":""}]}`},
		{"title, category, type", nil,
			[]string{"--title", "New title", "--category", "decisions", "--type", "note"},
			`{"title":"New title","category":"decisions","type":"note"}`},
		{"tags replace", nil, []string{"--tags", "a,b"}, `{"tags":["a","b"]}`},
		{"tags trimmed", nil, []string{"--tags", " a , b ,"}, `{"tags":["a","b"]}`},
		{"meta merge and unset", nil,
			[]string{"--meta", "source=claude-code-2026-11-21", "--meta", "expr=a=b", "--meta", "blank=", "--meta-unset", "old"},
			`{"metadata_merge":{"source":"claude-code-2026-11-21","expr":"a=b","blank":"","old":null}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(srv.requests())
			args := append([]string{"update", updateID}, c.args...)
			if _, stderr, err := runCLI(t, c.stdin, args...); err != nil {
				t.Fatalf("`ctx %s`: %v (stderr %q)", strings.Join(args, " "), err, stderr)
			}
			reqs := srv.requests()[before:]
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			got := reqs[0]
			if got["action"] != "update" || got["id"] != updateID {
				t.Fatalf("action/id = %v/%v, want update/%s", got["action"], got["id"], updateID)
			}
			var want any
			if err := jsonUnmarshalString(c.data, &want); err != nil {
				t.Fatalf("bad want %s: %v", c.data, err)
			}
			if g, w := canonicalJSON(t, got["data"]), canonicalJSON(t, want); g != w {
				t.Fatalf("data = %s, want %s", g, w)
			}
		})
	}
}

// Contradictory or incomplete flags are refused before any request, with the
// reason named.
func TestUpdateRefusesBadFlagsBeforeAnyRequest(t *testing.T) {
	srv := newRecordedServer(t, func(map[string]any) string { return updateOKAnswer })
	dir := t.TempDir()
	content := filepath.Join(dir, "c.md")
	if err := os.WriteFile(content, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no change", nil, "nothing to update"},
		{"unpaired edit", []string{"--edit-old", "a"}, "--edit-old"},
		{"unpaired new", []string{"--edit-new", "a"}, "--edit-old"},
		{"replace-all alone", []string{"--replace-all"}, "--replace-all"},
		{"content beside edits", []string{"--content-file", content, "--edit-old", "a", "--edit-new", "b"}, "--content-file"},
		{"edits file beside pairs", []string{"--edits-file", write("e.json", `[{"old":"a","new":"b"}]`), "--edit-old", "a", "--edit-new", "b"}, "--edits-file"},
		{"replace-all beside edits file", []string{"--edits-file", write("e2.json", `[{"old":"a","new":"b"}]`), "--replace-all"}, "--replace-all"},
		{"edits file not JSON", []string{"--edits-file", write("bad.json", `old=a`)}, "--edits-file"},
		{"edits file empty list", []string{"--edits-file", write("none.json", `[]`)}, "--edits-file"},
		{"edits file unknown key", []string{"--edits-file", write("typo.json", `[{"old":"a","new":"b","replaceAll":true}]`)}, "replaceAll"},
		{"meta without =", []string{"--meta", "source"}, "--meta"},
		{"meta empty key", []string{"--meta", "=v"}, "--meta"},
		{"meta key set and unset", []string{"--meta", "k=v", "--meta-unset", "k"}, `"k"`},
		{"meta key twice", []string{"--meta", "k=v", "--meta", "k=w"}, `"k"`},
		{"empty title", []string{"--title", ""}, "--title"},
		{"empty category", []string{"--category", ""}, "--category"},
		{"empty type", []string{"--type", ""}, "--type"},
		// An empty --tags is what --tags "$(cat missing)" hands over; sent, it
		// would clear the tag set.
		{"empty tags", []string{"--tags", ""}, "--tags"},
		{"tags of blanks only", []string{"--tags", " , "}, "--tags"},
		{"empty meta-unset key", []string{"--meta-unset", ""}, "--meta-unset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(srv.requests())
			args := append([]string{"update", updateID}, c.args...)
			stdout, _, err := runCLI(t, nil, args...)
			if err == nil {
				t.Fatalf("`ctx %s` succeeded, want a refusal", strings.Join(args, " "))
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to name %q", err, c.want)
			}
			if n := len(srv.requests()) - before; n != 0 {
				t.Fatalf("%d request(s) reached the server, want none", n)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing", stdout)
			}
		})
	}
}

// The answer of a successful update is one line — id, title, updated_at,
// tab-separated — instead of the whole block; --json prints the server's
// answer. A title's tab or line break becomes a space so the answer stays one
// line.
func TestUpdateAnswersOneLine(t *testing.T) {
	answer := updateOKAnswer
	newRecordedServer(t, func(map[string]any) string { return answer })

	stdout, _, err := runCLI(t, nil, "update", updateID, "--title", "Release notes")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if want := updateID + "\tRelease notes\t2026-11-21T01:02:03.456789Z\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}

	answer = strings.Replace(updateOKAnswer, `"title":"Release notes"`, `"title":"two\tparts\nand a line"`, 1)
	stdout, _, err = runCLI(t, nil, "update", updateID, "--title", "x")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if want := updateID + "\ttwo parts and a line\t2026-11-21T01:02:03.456789Z\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}

	answer = updateOKAnswer
	stdout, _, err = runCLI(t, nil, "update", updateID, "--title", "x", "--json")
	if err != nil {
		t.Fatalf("update --json: %v", err)
	}
	if !strings.Contains(stdout, `"success": true`) || !strings.Contains(stdout, `"content": "x"`) {
		t.Fatalf("--json stdout = %q, want the server's whole answer", stdout)
	}
}

// A refused update exits non-zero with the server's reason; the line form
// prints nothing on stdout, --json keeps the body (the envelope contract).
func TestUpdateFailureKeepsTheReason(t *testing.T) {
	const refusal = `{"success":false,"code":"edit_mismatch","error":"edits[0]: old matches 0 times in the content (after the edits before it) — an edit needs exactly one match; nothing was written"}`
	newRecordedServer(t, func(map[string]any) string { return refusal })

	stdout, _, err := runCLI(t, nil, "update", updateID, "--edit-old", "gone", "--edit-new", "x")
	if err == nil || !strings.Contains(err.Error(), "edits[0]: old matches 0 times") {
		t.Fatalf("err = %v, want the server's edit_mismatch reason", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing on a refused update", stdout)
	}

	stdout, _, err = runCLI(t, nil, "update", updateID, "--edit-old", "gone", "--edit-new", "x", "--json")
	if err == nil {
		t.Fatal("--json: a refused update must not exit 0")
	}
	if !strings.Contains(stdout, `"code": "edit_mismatch"`) {
		t.Fatalf("--json stdout = %q, want the refusal body", stdout)
	}
}
