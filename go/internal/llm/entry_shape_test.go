package llm

// T04-9: the slim chain row's field set and the one thing the constructor
// deliberately does NOT own — metadata.chain. The embed row writes the key
// only for a walk that produced attempts, the two chat rows write it
// unconditionally, so an empty walk persists metadata {} on one path and
// {"chain": null} on the other (llmlog.insert turns a nil map into {} but
// passes a present key through). Folding that into one constructor would
// rewrite rows, so the callers keep the assignment and these tests keep the
// difference visible.

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestNewChainEntryFieldSet pins exactly which columns the shared constructor
// fills — every other field of the row stays zero and belongs to a caller.
func TestNewChainEntryFieldSet(t *testing.T) {
	attempts := []ChainAttempt{
		{Backend: "gpu", Class: "server_error", Ms: 120, WaitMs: 40},
		{Backend: "gpu-a", Class: "ok", Ms: 200, WaitMs: 7},
	}
	got := newChainEntry("query-synthesize", nil, []string{"b1", "b2"},
		backends.SensInternal, attempts, "33333333-3333-3333-3333-333333333333")
	want := llmlog.Entry{
		Pipeline:            "query-synthesize",
		BlockIDs:            []string{"b1", "b2"},
		RequiredSensitivity: "internal",
		Attempt:             2,
		APIKeyID:            "33333333-3333-3333-3333-333333333333",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("row = %#v\nwant %#v", got, want)
	}
	if got.Metadata != nil {
		t.Error("metadata belongs to the caller — the constructor must leave it nil")
	}
}

// TestNewChainEntryKeepsNilBlockIDs: a caller without block ids gets a NULL
// block_ids column, never an empty array (pgx sees the slice unchanged).
func TestNewChainEntryKeepsNilBlockIDs(t *testing.T) {
	entry := newChainEntry("cluster-label", nil, nil, backends.SensPublic, nil, "")
	if entry.BlockIDs != nil {
		t.Errorf("block_ids = %#v, want nil", entry.BlockIDs)
	}
	if entry.Attempt != 0 || entry.APIKeyID != "" {
		t.Errorf("attempt/api_key_id = %d/%q, want 0/empty (both fall to NULL)", entry.Attempt, entry.APIKeyID)
	}
}

// TestEmbedWireEntryWritesNoChainOnAnEmptyWalk: the embed row's metadata stays
// absent when nothing was attempted — the reason metadata.chain is not part of
// newChainEntry.
func TestEmbedWireEntryWritesNoChainOnAnEmptyWalk(t *testing.T) {
	entry := embedWireEntry("dream-keyword-embed", backends.RoleEmbed, backends.SensInternal,
		nil, nil, nil, nil, "", dispatch.ClassBackground)
	if entry.Metadata != nil {
		t.Errorf("metadata = %#v, want nil (an empty walk writes no chain key)", entry.Metadata)
	}
}
