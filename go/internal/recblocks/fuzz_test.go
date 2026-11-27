package recblocks_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/recblocks"
)

// FuzzDecompose: no input panics, the invariants hold, the input stays
// untouched, the result is deterministic, and every address — the side
// addresses included — reads back through Lookup.
func FuzzDecompose(f *testing.F) {
	for _, in := range corpus() {
		f.Add(in.Op, in.Path, in.Stream, in.State, in.Status, in.Request, in.RequestNull,
			in.Response, in.ResponseNull, in.ResponseBytes)
	}
	f.Fuzz(func(t *testing.T, op, path string, stream bool, state string, status int,
		req []byte, reqNull bool, resp []byte, respNull bool, respBytes int64) {
		in := recblocks.Input{Op: op, Path: path, Stream: stream, State: state, Status: status,
			Request: req, RequestNull: reqNull, Response: resp, ResponseNull: respNull, ResponseBytes: respBytes}
		reqCopy, respCopy := bytes.Clone(req), bytes.Clone(resp)
		r := recblocks.Decompose(in)
		if !bytes.Equal(req, reqCopy) || !bytes.Equal(resp, respCopy) {
			t.Fatal("Decompose modified its input")
		}
		if again := recblocks.Decompose(in); !reflect.DeepEqual(r, again) {
			t.Fatal("Decompose is not deterministic")
		}
		checkInvariants(t, r)
		if r.Total != len(r.Blocks) {
			t.Fatalf("Total %d, %d blocks", r.Total, len(r.Blocks))
		}
		for _, b := range r.Blocks {
			got, ok := recblocks.Lookup(r, in, b.Address)
			if !ok || !reflect.DeepEqual(got, b) {
				t.Fatalf("Lookup(%q) does not return its block", b.Address)
			}
			_, found, ok := recblocks.Find(in, b.Address)
			if !ok || !reflect.DeepEqual(found, b) {
				t.Fatalf("Find(%q) does not return its block", b.Address)
			}
		}
		for _, w := range [][2]int{{0, 1}, {1, 2}, {len(r.Blocks) / 2, 3}, {len(r.Blocks), 5}} {
			win := recblocks.DecomposeWindow(in, w[0], w[1])
			lo, hi := min(w[0], len(r.Blocks)), min(w[0]+w[1], len(r.Blocks))
			if win.Total != len(r.Blocks) || !reflect.DeepEqual(win.Blocks, r.Blocks[lo:hi]) ||
				win.Request != r.Request || win.Response != r.Response {
				t.Fatalf("DecomposeWindow(%d, %d) is not the slice [%d:%d] of Decompose", w[0], w[1], lo, hi)
			}
		}
		for side, s := range map[string]recblocks.Side{"request": r.Request, "response": r.Response} {
			body := req
			if side == "response" {
				body = resp
			}
			got, ok := recblocks.Lookup(r, in, side)
			if ok != (s.State == "present") {
				t.Fatalf("Lookup(%q) ok=%v in state %q", side, ok, s.State)
			}
			if ok && !bytes.Equal(got.Content(), body) {
				t.Fatalf("Lookup(%q) is not the whole body", side)
			}
		}
	})
}
