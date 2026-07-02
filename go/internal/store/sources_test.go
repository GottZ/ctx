package store

import "testing"

// --- Integration test skeletons for DB-dependent functions ---.
// These require a live database and are skipped in -short / CI.

func TestUpsertSource_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: insert new source returns populated Source with is_new=true.
	// Test: upsert existing (same file_path+scope) resets status to pending.
	// Test: file_hash and file_size are updated on conflict.
}

func TestGetSourceByPath_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: returns nil for non-existent path.
	// Test: returns correct source for existing path+scope.
	// Test: different scope returns nil.
}

func TestUpdateSourceStatus_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: status changes from pending to chunking.
	// Test: error_message is set when status=error.
}

func TestUpdateSourceChunkCount_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: chunk_count is updated.
	// Test: updated_at changes.
}

func TestListOrphanSources_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: source not in activePaths is returned.
	// Test: source in activePaths is not returned.
	// Test: scope isolation — other scope sources are not returned.
}

func TestDeleteSourceAndChunks_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: chunks are archived (is_archived=true).
	// Test: source row is deleted.
	// Test: transaction rolls back on failure.
}

func TestInsertChunk_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: inserts new chunk with lifecycle_state='chunk'.
	// Test: idempotent — same source_id+chunk_index updates content.
	// Test: nil tags/metadata default to empty.
	// Test: returns populated Block.
}
