package store

// guardMatchedIDSQL is the block a guard verdict names, read from the
// guard_matched_id key of the metadata expression metadata (e.g.
// "b.metadata"): the uuid the key holds, NULL for a value the uuid cast does
// not read (a malformed value degrades to "no block" instead of a 22P02).
// Every reader of the key reads it through this expression — the redaction
// of the raw read paths (guardRedactedMetadataSQL) and the guard list
// (guardListQuery) — so the key names the same block on every surface, and
// the reader finds that block with one probe of context_blocks_pkey (issue
// #536: the guard list compared mb.id::text with the key, which no index
// serves, and read every row of context_blocks for each list).
//
// The cast reads every spelling of a uuid PostgreSQL accepts — upper case,
// braces, the hyphens left out — where the text comparison matched the
// canonical text only. The guard writes the canonical text (id::text), and
// since issue #238 no client write stores the key; a row written before #238
// with another spelling names its block on the guard list as it already did
// on the raw read paths.
func guardMatchedIDSQL(metadata string) string {
	return `CASE WHEN pg_input_is_valid(` + metadata + `->>'guard_matched_id', 'uuid') THEN (` + metadata + `->>'guard_matched_id')::uuid END`
}
