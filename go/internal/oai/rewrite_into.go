package oai

// The rewrite of FOLGE-S (RC-D, cluster C8; W02-08 M-1, external review 5
// point 5): one upstream stream chunk into its client SSE line. Contract
// (bau report RCD-c8-hardening-S1 §Vertrag): rewriteChunkInto writes the
// whole line — "data: ", the chunk's client JSON, "\n\n" — into dst[:0]
// (one buffer per attempt, grown only to the largest line), as a token walk
// over the upstream object (FOLGE-P's newMemberIter/hullKey pattern), byte
// for byte what rewriteChunk and the stream writer's frame write today
// (TestRewriteChunkTraceIsTheBase); sc.out aliases dst, so the relay copies
// what it keeps (its ring does; its big path must as well). The writer
// then writes the line as it is (streamWriter.frameLine, S2) instead of
// framing it again (sse.go frameLocked: make(len+8) per frame).

// rewriteChunkInto is rewriteChunk with the SSE frame, written into dst.
// RC-D S1 stub: rewriteChunk, then the frame appended — the base's work
// (7.3–8.0 × the chunk) plus the line.
func rewriteChunkInto(dst, upstream []byte, id string, created int64, alias, lane string, reasoningOff, includeUsage bool, roles *roleCarry) (streamChunk, error) {
	sc, err := rewriteChunk(upstream, id, created, alias, lane, reasoningOff, includeUsage, roles)
	if err != nil || sc.skip {
		return sc, err
	}
	out := append(dst[:0], "data: "...)
	out = append(out, sc.out...)
	sc.out = append(out, "\n\n"...)
	return sc, nil
}
