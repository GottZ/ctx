package handler

import (
	"slices"
	"strings"
)

// ingestNULChunks names, per chunk, the field whose value carries a NUL
// character ("" for a chunk without one): such a chunk fails on its own
// (issue #519), as a chunk whose write fails does, and writes nothing.
func ingestNULChunks(chunks []IngestChunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = ingestChunkNULField(c)
	}
	return out
}

func ingestChunkNULField(c IngestChunk) string {
	nul := func(s string) bool { return strings.IndexByte(s, 0) >= 0 }
	switch {
	case nul(c.Category):
		return "category"
	case nul(c.Title):
		return "title"
	case nul(c.Content):
		return "content"
	case slices.ContainsFunc(c.Tags, nul):
		return "tags"
	case carriesNUL(map[string]any(c.Metadata)):
		return "metadata"
	case nul(c.SourceID):
		return "source_id"
	}
	return ""
}
