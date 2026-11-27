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
		for _, b := range r.Blocks {
			got, ok := recblocks.Lookup(r, in, b.Address)
			if !ok || !reflect.DeepEqual(got, b) {
				t.Fatalf("Lookup(%q) does not return its block", b.Address)
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
