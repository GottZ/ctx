//go:build integration

// V-11 store gate (design/02 §5.1 BA7, Schicht 3): SearchBlocks, RecentBlocks
// and GetBlock resolve the untrusted framing of a row's TYPE from the registry
// snapshot and serialize it on the response types.
//
// RecentBlocks needs its OWN probe here: the MCP recent tool runs a separate
// statement (handler/mcp.go), so the only production consumer of this function
// is the chat ctx_recent tool — a path with no handler-level test surface.
//
// The probes assert over the MARSHALLED bytes, not the Go field, because the
// wire is what BA7 is about: a `checkpoint` block is captured session
// transcript, and every reader has to be able to tell it apart from a
// first-party `knowledge` block.
//
//	go test -tags=integration ./internal/store/ -run TestUntrustedV11 -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const v11Category = "v11-store-untrusted"

var v11Scopes = []string{"private"}

func v11Seed(t *testing.T, pool *pgxpool.Pool) (untrustedID, trustedID string) {
	t.Helper()
	for _, fx := range []struct {
		title    string
		typeName string
		out      *string
	}{
		{"V11S Checkpoint", "checkpoint", &untrustedID},
		{"V11S Knowledge", "knowledge", &trustedID},
	} {
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ($1, $2, 'v11 store fixture', 'private', $3) RETURNING id::text`,
			v11Category, fx.title, fx.typeName).Scan(fx.out); err != nil {
			t.Fatalf("seed %s: %v", fx.title, err)
		}
	}
	return untrustedID, trustedID
}

func v11Set(t *testing.T, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()
	if !set.IsUntrusted("checkpoint") {
		t.Fatalf("fixture premise broken: checkpoint is not retrieval.untrusted in this registry")
	}
	if set.IsUntrusted("knowledge") {
		t.Fatalf("fixture premise broken: knowledge IS retrieval.untrusted in this registry")
	}
	return set
}

// v11Flag marshals one response value and reports the `untrusted` key state.
func v11Flag(t *testing.T, v any) (present, value bool) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	got, ok := m["untrusted"]
	b, _ := got.(bool)
	return ok, b
}

func v11AssertPreviews(t *testing.T, where string, rows []store.BlockPreview) {
	t.Helper()
	seen := 0
	for _, bp := range rows {
		if bp.Category != v11Category {
			continue
		}
		seen++
		present, value := v11Flag(t, bp)
		switch bp.TypeName {
		case "checkpoint":
			if !present || !value {
				t.Errorf("%s: checkpoint preview untrusted=(present=%v,value=%v), want (true,true)", where, present, value)
			}
		case "knowledge":
			if present {
				t.Errorf("%s: knowledge preview carries an untrusted key, want it omitted (omitempty)", where)
			}
		default:
			t.Errorf("%s: unexpected fixture type %q", where, bp.TypeName)
		}
	}
	if seen != 2 {
		t.Fatalf("%s: saw %d fixture rows, want 2", where, seen)
	}
}

func TestUntrustedV11StoreSearchBlocks_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	v11Seed(t, pool)
	set := v11Set(t, pool)
	ctx := context.Background()

	rows, err := store.SearchBlocks(ctx, pool, set, "", v11Scopes, v11Category, nil, 50, true, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("SearchBlocks: %v", err)
	}
	v11AssertPreviews(t, "SearchBlocks(compact)", rows)

	full, err := store.SearchBlocks(ctx, pool, set, "", v11Scopes, v11Category, nil, 50, false, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("SearchBlocks(full): %v", err)
	}
	v11AssertPreviews(t, "SearchBlocks(full)", full)
}

func TestUntrustedV11StoreRecentBlocks_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	v11Seed(t, pool)
	set := v11Set(t, pool)

	rows, err := store.RecentBlocks(context.Background(), pool, set, v11Scopes, v11Category, 50, nil, nil)
	if err != nil {
		t.Fatalf("RecentBlocks: %v", err)
	}
	v11AssertPreviews(t, "RecentBlocks", rows)
}

func TestUntrustedV11StoreGetBlock_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	untrustedID, trustedID := v11Seed(t, pool)
	set := v11Set(t, pool)
	ctx := context.Background()

	b, err := store.GetBlock(ctx, pool, set, untrustedID, v11Scopes, nil)
	if err != nil || b == nil {
		t.Fatalf("GetBlock(checkpoint) = (%v, %v)", b, err)
	}
	if present, value := v11Flag(t, b); !present || !value {
		t.Errorf("GetBlock(checkpoint) untrusted=(present=%v,value=%v), want (true,true)", present, value)
	}

	k, err := store.GetBlock(ctx, pool, set, trustedID, v11Scopes, nil)
	if err != nil || k == nil {
		t.Fatalf("GetBlock(knowledge) = (%v, %v)", k, err)
	}
	if present, _ := v11Flag(t, k); present {
		t.Errorf("GetBlock(knowledge) carries an untrusted key, want it omitted (omitempty)")
	}

	// A caller without a registry snapshot must not panic and must not INVENT
	// a framing — the nil seam degrades to "no statement", never to "trusted
	// by assertion". (The framing's fail-open direction lives in
	// blocktype.Set.IsUntrusted and is a known, separately-tracked limit.)
	n, err := store.GetBlock(ctx, pool, nil, untrustedID, v11Scopes, nil)
	if err != nil || n == nil {
		t.Fatalf("GetBlock(nil set) = (%v, %v)", n, err)
	}
	if present, _ := v11Flag(t, n); present {
		t.Errorf("GetBlock(nil set) carries an untrusted key, want it omitted")
	}
}
