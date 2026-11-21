package cli

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// Issue #408: `ctx search … --lines` prints one hit per line — id, category,
// title, tab-separated — after the precedent of `ctx guard list --ids-only`.

func TestSearchLinesPrintsOneHitPerLine(t *testing.T) {
	var answer string
	srv := newRecordedServer(t, func(map[string]any) string { return answer })

	answer = `{"success":true,"count":3,"compact":true,"results":[` +
		`{"id":"0197aaaa-0000-7000-8000-000000000001","category":"learnings","title":"Prompt shape","scope":"private"},` +
		`{"id":"0197aaaa-0000-7000-8000-000000000002","category":"learnings","title":"Tab\there and\na line","scope":"private"},` +
		`{"id":"0197aaaa-0000-7000-8000-000000000003","category":"odd\tcat","title":"Third","scope":"private"}],"next_after":null}`
	stdout, stderr, err := runCLI(t, nil, "search", "learnings", "query:prompt", "--lines")
	if err != nil {
		t.Fatalf("search --lines: %v (stderr %q)", err, stderr)
	}
	want := "0197aaaa-0000-7000-8000-000000000001\tlearnings\tPrompt shape\n" +
		"0197aaaa-0000-7000-8000-000000000002\tlearnings\tTab here and a line\n" +
		"0197aaaa-0000-7000-8000-000000000003\todd cat\tThird\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}

	// --lines changes the printing, not the request.
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	var wantBody any
	_ = jsonUnmarshalString(`{"limit":10,"compact":true,"category":"learnings","query":"prompt"}`, &wantBody)
	if g, w := canonicalJSON(t, reqs[0]), canonicalJSON(t, wantBody); g != w {
		t.Fatalf("body = %s, want %s", g, w)
	}

	// No hit, no line.
	answer = `{"success":true,"count":0,"compact":true,"results":[],"next_after":null}`
	stdout, _, err = runCLI(t, nil, "search", "query:nothing", "--lines")
	if err != nil {
		t.Fatalf("search --lines (empty): %v", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing for no hits", stdout)
	}
}

// A hit whose type marks foreign text keeps that framing: stdout keeps its
// three cells, stderr names the hit by id with the same notes `get --field`
// writes.
func TestSearchLinesKeepsTheFramingOnStderr(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string {
		return `{"success":true,"count":2,"compact":true,"results":[` +
			`{"id":"0197aaaa-0000-7000-8000-000000000001","category":"c","title":"agent text","untrusted":true},` +
			`{"id":"0197aaaa-0000-7000-8000-000000000002","category":"c","title":"retyped","untrusted_origin":"checkpoint"}],"next_after":null}`
	})
	stdout, stderr, err := runCLI(t, nil, "search", "c", "--lines")
	if err != nil {
		t.Fatalf("search --lines: %v", err)
	}
	if want := "0197aaaa-0000-7000-8000-000000000001\tc\tagent text\n0197aaaa-0000-7000-8000-000000000002\tc\tretyped\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	for _, want := range []string{
		"0197aaaa-0000-7000-8000-000000000001 untrusted:true",
		"0197aaaa-0000-7000-8000-000000000002 " + store.UntrustedOriginNote("checkpoint"),
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
}

// A failed search with --lines prints nothing on stdout and exits with the
// server's reason; without --lines the JSON body stays (the envelope contract).
func TestSearchLinesFailurePrintsNothing(t *testing.T) {
	newRecordedServer(t, func(map[string]any) string { return `{"success":false,"error":"malformed cluster handle"}` })
	stdout, _, err := runCLI(t, nil, "search", "cluster:x", "--lines")
	if err == nil || err.Error() != "malformed cluster handle" {
		t.Fatalf("err = %v, want the server's reason", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
	stdout, _, err = runCLI(t, nil, "search", "cluster:x")
	if err == nil || !strings.Contains(stdout, `"success": false`) {
		t.Fatalf("without --lines: err = %v, stdout = %q, want exit 1 and the body", err, stdout)
	}
}
