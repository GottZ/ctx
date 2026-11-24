package store

import (
	"context"
	"testing"
)

// TestResolveBlockIDFullShapeNamesABlockOnlyAsAUUID (second review of PR
// #554): a 36-character id with the hyphens of a UUID skipped the prefix path
// unread, so "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz" reached the database's
// uuid cast and came back as a server error (22P02) on manage get, update and
// delete and the MCP get. Such an id names no block; a UUID in either case
// still resolves to itself (the cast reads both).
func TestResolveBlockIDFullShapeNamesABlockOnlyAsAUUID(t *testing.T) {
	scopes := []string{"private"}
	for _, id := range []string{
		"zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz",
		"%%%%%%%%-%%%%-%%%%-%%%%-%%%%%%%%%%%%",
		"01a20eb4-b17d-7000-8000-00000000000g",
		"01a20eb4-b17d-7000-8000-0000-0000000",
		"01a20eb4-b17d-7000-8000-0000000000 0",
	} {
		got, matches, err := ResolveBlockID(context.Background(), nil, id, scopes, nil)
		if got != "" || matches != nil || err != nil {
			t.Errorf("%q: resolved to %q, %v, %v — want no block", id, got, matches, err)
		}
	}
	for _, id := range []string{
		"01a20eb4-b17d-7000-8000-0123456789ab",
		"01A20EB4-B17D-7000-8000-0123456789AB",
		"01a20EB4-b17d-7000-8000-0123456789aB",
	} {
		if got, _, err := ResolveBlockID(context.Background(), nil, id, scopes, nil); got != id || err != nil {
			t.Errorf("%q: resolved to %q, %v — want the id itself", id, got, err)
		}
	}
}
