package recblocks_test

import (
	"bytes"
	"go/build"
	"reflect"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/recblocks"
)

func TestSideStates(t *testing.T) {
	cases := []struct {
		name        string
		in          recblocks.Input
		req, resp   string
		reqB, respB int64
	}{
		{"cleared request, cleared answer",
			recblocks.Input{State: "done", Status: 200, RequestNull: true, ResponseNull: true, ResponseBytes: 512},
			"cleared", "cleared", 0, 512},
		{"empty request, no answer",
			recblocks.Input{State: "done", Request: []byte{}, ResponseNull: true},
			"empty", "none", 0, 0},
		{"nil request is empty",
			recblocks.Input{State: "done", Response: []byte{}},
			"empty", "none", 0, 0},
		{"open row",
			recblocks.Input{State: "open", Request: []byte(`{}`), ResponseNull: true},
			"present", "pending", 2, 0},
		{"open row with an answer anyway",
			recblocks.Input{State: "open", Status: 200, Request: []byte(`{}`), Response: []byte(`{}`), ResponseBytes: 2},
			"present", "present", 2, 2},
		{"open row past the retention",
			recblocks.Input{State: "open", RequestNull: true, ResponseNull: true},
			"cleared", "pending", 0, 0},
		{"done, answer present",
			recblocks.Input{State: "done", Status: 200, Request: []byte(`{}`), Response: []byte(`{"a":1}`), ResponseBytes: 7},
			"present", "present", 2, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(c.in)
			checkInvariants(t, r)
			if r.Request.Address != "request" || r.Request.State != c.req || r.Request.Bytes != c.reqB {
				t.Errorf("request side %+v, want %s %d", r.Request, c.req, c.reqB)
			}
			if r.Response.Address != "response" || r.Response.State != c.resp || r.Response.Bytes != c.respB {
				t.Errorf("response side %+v, want %s %d", r.Response, c.resp, c.respB)
			}
			if c.req != "present" && (r.Request.Format != "" || len(sideBlocks(t, r, "request")) != 0) {
				t.Errorf("request side %q has a format or blocks: %+v", c.req, r.Request)
			}
			if c.resp != "present" && (r.Response.Format != "" || len(sideBlocks(t, r, "response")) != 0) {
				t.Errorf("response side %q has a format or blocks: %+v", c.resp, r.Response)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	in := answer("chat", "/v1/chat/completions", false, 200, chatBody, chatAnswer)
	r := recblocks.Decompose(in)
	for _, b := range r.Blocks {
		got, ok := recblocks.Lookup(r, in, b.Address)
		if !ok || !reflect.DeepEqual(got, b) {
			t.Errorf("Lookup(%q) = %+v %v, want the block", b.Address, view(got), ok)
		}
	}
	for side, body := range map[string]string{"request": chatBody, "response": chatAnswer} {
		got, ok := recblocks.Lookup(r, in, side)
		if !ok {
			t.Fatalf("Lookup(%q): not found", side)
		}
		if view(got) != rawOf(side, []byte(body)) || got.Bytes != len(body) {
			t.Errorf("Lookup(%q) = %+v", side, view(got))
		}
	}
	for _, addr := range []string{"", "request/messages/99", "response/choices/0/content/0", "requests", "request/"} {
		if got, ok := recblocks.Lookup(r, in, addr); ok {
			t.Errorf("Lookup(%q) = %+v, want none", addr, view(got))
		}
	}
}

func TestLookupSideStates(t *testing.T) {
	cleared := recblocks.Input{State: "done", Status: 200, RequestNull: true, ResponseNull: true, ResponseBytes: 5}
	open := recblocks.Input{State: "open", Request: []byte{}, ResponseNull: true}
	for _, in := range []recblocks.Input{cleared, open} {
		r := recblocks.Decompose(in)
		for _, side := range []string{"request", "response"} {
			if got, ok := recblocks.Lookup(r, in, side); ok {
				t.Errorf("Lookup(%q) of %+v = %+v, want none", side, r, view(got))
			}
		}
	}
	// The single block of a side is the side address itself.
	bad := []byte("x\xffy")
	in := recblocks.Input{State: "done", Status: 200, Request: bad, Response: []byte(`{"a":1}`)}
	r := recblocks.Decompose(in)
	got, ok := recblocks.Lookup(r, in, "request")
	if !ok || view(got) != rawOf("request", bad) {
		t.Errorf("Lookup(request) = %+v %v", view(got), ok)
	}
	got, ok = recblocks.Lookup(r, in, "response")
	if !ok || view(got) != js("response", `{"a":1}`) {
		t.Errorf("Lookup(response) = %+v %v", view(got), ok)
	}
}

// Decompose neither modifies its input nor hands out memory that aliases it.
func TestNoAliasNoMutation(t *testing.T) {
	for _, in := range corpus() {
		req, resp := bytes.Clone(in.Request), bytes.Clone(in.Response)
		r := recblocks.Decompose(in)
		if !bytes.Equal(in.Request, req) || !bytes.Equal(in.Response, resp) {
			t.Fatalf("Decompose modified its input")
		}
		var looked []recblocks.Block
		for _, side := range []string{"request", "response"} {
			if b, ok := recblocks.Lookup(r, in, side); ok {
				looked = append(looked, b)
			}
		}
		before := blocksOf(t, r)
		var lookedBefore []wb
		for _, b := range looked {
			lookedBefore = append(lookedBefore, view(b))
		}
		for i := range in.Request {
			in.Request[i] = 'X'
		}
		for i := range in.Response {
			in.Response[i] = 'X'
		}
		equalBlocks(t, blocksOf(t, r), before)
		for i, b := range looked {
			if view(b) != lookedBefore[i] {
				t.Errorf("side block %q changed with the caller's input", b.Address)
			}
		}
	}
}

// The same input gives a byte-identical Result, also from a copy of it.
func TestDeterminism(t *testing.T) {
	for i, in := range corpus() {
		a := recblocks.Decompose(in)
		b := recblocks.Decompose(in)
		cp := in
		cp.Request, cp.Response = bytes.Clone(in.Request), bytes.Clone(in.Response)
		c := recblocks.Decompose(cp)
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, c) {
			t.Errorf("corpus %d: results differ", i)
		}
		checkInvariants(t, a)
	}
}

// A 100k-deep body neither panics nor blows the stack: it is one block.
func TestDeepNesting(t *testing.T) {
	deep := strings.Repeat("[", 100000) + strings.Repeat("]", 100000)
	for _, op := range []string{"chat", "embed", "rerank", ""} {
		in := answer(op, "/v1/chat/completions", false, 200, deep, deep)
		r := recblocks.Decompose(in)
		bl := blocksOf(t, r)
		if len(bl) != 2 || bl[0].Address != "request" || bl[1].Address != "response" ||
			bl[0].Content != deep || bl[1].Content != deep {
			t.Errorf("op %q: %d blocks", op, len(bl))
		}
	}
	in := answer("chat", "/v1/chat/completions", true, 200, `{}`, sse("data: "+deep))
	r := recblocks.Decompose(in)
	if r.Response.StreamLines != 1 || r.Response.StreamLinesUnparsed != 1 || len(sideBlocks(t, r, "response")) != 1 {
		t.Errorf("deep stream line: %+v", r.Response)
	}
	// Nesting json.Valid accepts is walked without recursion as well.
	mid := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	body := `{"messages":[` + mid + `,{"role":"user","content":[` + mid + `]}]}`
	r = recblocks.Decompose(request("chat", "/v1/chat/completions", body))
	equalBlocks(t, blocksOf(t, r), []wb{
		js("request/messages/0", mid),
		func() wb { w := js("request/messages/1/content/0", mid); w.Role = "user"; return w }(),
	})
}

// The package is stdlib-only: no database, no other ctx package.
func TestStdlibOnly(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") {
			t.Errorf("import %q is not the standard library", imp)
		}
	}
}
