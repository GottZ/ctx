//go:build integration

// Issue #407: GetBlockOmitting and GetArchivedBlock leave the content and the
// metadata column unread when asked to — the SQL side of the `fields` option
// of MCP get and manage get — and answer every other column as the full read
// does.
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestGetBlockOmitting -count=1 -v
package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGetBlockOmitting(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{"private"}
	sens := store.SensitivityWrite{Value: backends.SensInternal, Manual: true}

	live, err := store.UpsertBlock(ctx, pool, "i407", "i407 live", "content of the live block", []string{"a"},
		map[string]any{"source": "i407"}, "private", true, sens, "")
	if err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	arch, err := store.UpsertBlock(ctx, pool, "i407", "i407 archived", "content of the archived block", nil,
		map[string]any{"source": "i407"}, "private", true, sens, "")
	if err != nil {
		t.Fatalf("upsert archived: %v", err)
	}
	if _, err := store.DeleteBlock(ctx, pool, arch.ID, scopes); err != nil {
		t.Fatalf("archive: %v", err)
	}

	reads := map[string]func(store.BlockReadOmit) (*store.Block, error){
		"GetBlockOmitting": func(o store.BlockReadOmit) (*store.Block, error) {
			return store.GetBlockOmitting(ctx, pool, nil, live.ID, scopes, nil, o)
		},
		"GetArchivedBlock": func(o store.BlockReadOmit) (*store.Block, error) {
			return store.GetArchivedBlock(ctx, pool, nil, arch.ID, scopes, o)
		},
	}
	for name, read := range reads {
		full, err := read(store.BlockReadOmit{})
		if err != nil || full == nil || full.Content == "" || full.Metadata["source"] != "i407" {
			t.Fatalf("%s full read = %+v, %v", name, full, err)
		}
		if name == "GetBlockOmitting" {
			viaGetBlock, err := store.GetBlock(ctx, pool, nil, live.ID, scopes, nil)
			if err != nil || !reflect.DeepEqual(viaGetBlock, full) {
				t.Errorf("GetBlock = %+v (%v), want the zero-omit read %+v", viaGetBlock, err, full)
			}
		}
		for _, omit := range []store.BlockReadOmit{{Content: true}, {Metadata: true}, {Content: true, Metadata: true}} {
			got, err := read(omit)
			if err != nil || got == nil {
				t.Fatalf("%s %+v = %+v, %v", name, omit, got, err)
			}
			want := *full
			if omit.Content {
				want.Content = ""
			}
			if omit.Metadata {
				want.Metadata = nil
			}
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("%s %+v =\n %+v\nwant\n %+v", name, omit, *got, want)
			}
		}
	}

	// The omission never changes which block is found.
	if b, err := store.GetBlockOmitting(ctx, pool, nil, arch.ID, scopes, nil, store.BlockReadOmit{Content: true, Metadata: true}); err != nil || b != nil {
		t.Errorf("an archived id through the live read = %+v, %v, want a miss", b, err)
	}
	if b, err := store.GetBlockOmitting(ctx, pool, nil, live.ID, []string{"elsewhere"}, nil, store.BlockReadOmit{Content: true}); err != nil || b != nil {
		t.Errorf("a foreign scope = %+v, %v, want a miss", b, err)
	}
}
