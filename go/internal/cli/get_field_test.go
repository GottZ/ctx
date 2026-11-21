package cli

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// Issue #408: `ctx get <id> --field NAME` prints one field's value without jq;
// --offset/--limit ask for a window of the content. The names ride to the
// server's fields/content_offset/content_limit (issue #407), so the server
// reads only what is printed.

const getID = "0197aaaa-0000-7000-8000-000000000001"

func TestGetFieldRequest(t *testing.T) {
	srv := newRecordedServer(t, func(map[string]any) string {
		return `{"action":"get","success":true,"block":{"id":"` + getID + `","title":"t","content":"c","updated_at":"2026-11-21T00:00:00Z"}}`
	})
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"one field", []string{"--field", "content"},
			`{"action":"get","id":"` + getID + `","fields":["content"]}`},
		{"repeated", []string{"--field", "title", "--field", "updated_at"},
			`{"action":"get","id":"` + getID + `","fields":["title","updated_at"]}`},
		{"comma list", []string{"--field", "title,updated_at"},
			`{"action":"get","id":"` + getID + `","fields":["title","updated_at"]}`},
		{"names trimmed", []string{"--field", " title , updated_at ,"},
			`{"action":"get","id":"` + getID + `","fields":["title","updated_at"]}`},
		{"window with the field", []string{"--field", "content", "--offset", "0", "--limit", "100"},
			`{"action":"get","id":"` + getID + `","fields":["content"],"content_offset":0,"content_limit":100}`},
		{"window alone", []string{"--offset", "10"},
			`{"action":"get","id":"` + getID + `","content_offset":10}`},
		{"no flag keeps the old body", nil,
			`{"action":"get","id":"` + getID + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(srv.requests())
			args := append([]string{"get", getID}, c.args...)
			if _, stderr, err := runCLI(t, nil, args...); err != nil {
				t.Fatalf("`ctx %s`: %v (stderr %q)", strings.Join(args, " "), err, stderr)
			}
			reqs := srv.requests()[before:]
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			var want any
			if err := jsonUnmarshalString(c.want, &want); err != nil {
				t.Fatalf("bad want: %v", err)
			}
			if g, w := canonicalJSON(t, reqs[0]), canonicalJSON(t, want); g != w {
				t.Fatalf("body = %s, want %s", g, w)
			}
		})
	}
}

// One field prints its value as `jq -r .block.<name>` does: a string raw (no
// JSON quoting) and one newline, any other value as JSON, a key the answer
// does not carry as null. Several fields print the block object.
func TestGetFieldPrints(t *testing.T) {
	var answer string
	newRecordedServer(t, func(map[string]any) string { return answer })
	cases := []struct {
		name   string
		block  string
		args   []string
		stdout string
	}{
		{"string raw", `{"id":"` + getID + `","content":"He said \"hi\" \\ — ä\n\tindented"}`,
			[]string{"--field", "content"}, "He said \"hi\" \\ — ä\n\tindented\n"},
		{"empty string", `{"id":"` + getID + `","content":""}`,
			[]string{"--field", "content"}, "\n"},
		{"list as JSON", `{"id":"` + getID + `","tags":["a","b"]}`,
			[]string{"--field", "tags"}, "[\n  \"a\",\n  \"b\"\n]\n"},
		{"absent key", `{"id":"` + getID + `"}`,
			[]string{"--field", "sensitivity"}, "null\n"},
		{"one name twice is one field", `{"id":"` + getID + `","content":"once"}`,
			[]string{"--field", "content", "--field", "content"}, "once\n"},
		{"several keep the server's order", `{"id":"` + getID + `","title":"T","updated_at":"2026-11-21T00:00:00Z"}`,
			[]string{"--field", "updated_at", "--field", "title"},
			"{\n  \"id\": \"" + getID + "\",\n  \"title\": \"T\",\n  \"updated_at\": \"2026-11-21T00:00:00Z\"\n}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			answer = `{"action":"get","success":true,"block":` + c.block + `,"graph_visible":true}`
			stdout, stderr, err := runCLI(t, nil, append([]string{"get", getID}, c.args...)...)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if stdout != c.stdout {
				t.Fatalf("stdout = %q, want %q", stdout, c.stdout)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want nothing", stderr)
			}
		})
	}
}

// The framing keys say how to read a block (the server keeps them on every
// projection). One field printed raw cannot carry them on stdout, so they go
// to stderr; stdout stays the value alone.
func TestGetFieldKeepsTheFramingOnStderr(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string {
		return `{"action":"get","success":true,"block":{"id":"` + getID + `","content":"tool output",` +
			`"untrusted":true,"untrusted_origin":"checkpoint","archived":true}}`
	})
	stdout, stderr, err := runCLI(t, nil, "get", getID, "--field", "content")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stdout != "tool output\n" {
		t.Fatalf("stdout = %q, want the value alone", stdout)
	}
	for _, want := range []string{"untrusted", store.UntrustedOriginNote("checkpoint"), "archived"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
}

// The framing key that IS the printed field is on stdout already; only the
// others go to stderr.
func TestGetFieldFramingNotRepeated(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string {
		return `{"action":"get","success":true,"block":{"id":"` + getID + `","untrusted":true,"untrusted_origin":"checkpoint"}}`
	})
	stdout, stderr, err := runCLI(t, nil, "get", getID, "--field", "untrusted")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stdout != "true\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "true\n")
	}
	if strings.Contains(stderr, "untrusted:true") || !strings.Contains(stderr, store.UntrustedOriginNote("checkpoint")) {
		t.Fatalf("stderr = %q, want the origin note and not the printed key again", stderr)
	}
}

// An empty --field is what --field "$name" hands over when $name is empty; it
// is refused instead of falling back to the whole block.
func TestGetFieldEmptyIsRefused(t *testing.T) {
	srv := newRecordedServer(t, func(map[string]any) string { return `{"action":"get","success":true,"block":{"id":"x"}}` })
	for _, value := range []string{"", " , "} {
		stdout, _, err := runCLI(t, nil, "get", getID, "--field", value)
		if err == nil || !strings.Contains(err.Error(), "--field") {
			t.Fatalf("--field %q: err = %v, want a refusal naming --field", value, err)
		}
		if stdout != "" {
			t.Fatalf("--field %q: stdout = %q, want nothing", value, stdout)
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("%d request(s), want none", n)
	}
}

// A window of --limit 0 answers truncated with no content; "the next window
// starts at the same offset" would send a loop in circles, so no note.
func TestGetFieldLimitZeroNamesNoNextWindow(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string {
		return `{"action":"get","success":true,"block":{"id":"` + getID + `","content":"",` +
			`"content_length":10,"content_truncated":true}}`
	})
	stdout, stderr, err := runCLI(t, nil, "get", getID, "--field", "content", "--offset", "5", "--limit", "0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stdout != "\n" || strings.Contains(stderr, "--offset") {
		t.Fatalf("stdout = %q, stderr = %q, want an empty value and no next-window note", stdout, stderr)
	}
}

// A window that cut the content says so on stderr, with the offset the next
// window starts at; the value on stdout is the cut alone.
func TestGetFieldWindowSaysWhereTheNextStarts(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string {
		return `{"action":"get","success":true,"block":{"id":"` + getID + `","content":"defg",` +
			`"content_length":10,"content_truncated":true}}`
	})
	stdout, stderr, err := runCLI(t, nil, "get", getID, "--field", "content", "--offset", "3", "--limit", "4")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stdout != "defg\n" {
		t.Fatalf("stdout = %q, want the window alone", stdout)
	}
	for _, want := range []string{"--offset 7", "10"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
}

// A failed get with --field prints nothing on stdout: a script that captures
// the value must not capture an error body as the value.
func TestGetFieldFailurePrintsNothing(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string { return `{"success":false,"error":"Block not found"}` })
	stdout, _, err := runCLI(t, nil, "get", getID, "--field", "content")
	if err == nil || err.Error() != "Block not found" {
		t.Fatalf("err = %v, want the server's reason", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}
