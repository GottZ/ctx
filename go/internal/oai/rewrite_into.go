package oai

import (
	"strconv"
)

// The rewrite of FOLGE-S (RC-D, cluster C8; W02-08 M-1, external review 5
// point 5): one upstream stream chunk into its client SSE line. rewriteChunkInto
// writes the whole line — "data: ", the chunk's client JSON, "\n\n" — into
// dst[:0] (one buffer per attempt, sized for the chunk once and reused),
// as a walk over the upstream object (rewrite_walk.go), byte for byte what
// the map rewrite and the stream writer's frame wrote before
// (TestRewriteChunkTraceIsTheBase). sc.out aliases dst, so the relay copies
// what it keeps (streamRelay.put, on every path), and the writer writes the
// line as it is (streamWriter.frameLine).

// rewriteChunkInto renders the client SSE line of one upstream stream chunk
// into dst; roles is the role carry of the attempt the chunk belongs to.
// A chunk with nothing for the client (skip) leaves sc.out nil.
func rewriteChunkInto(dst, upstream []byte, id string, created int64, alias, lane string, reasoningOff, includeUsage bool, roles *roleCarry) (streamChunk, error) {
	obj, null, err := upstreamObject(upstream)
	if err != nil {
		return streamChunk{}, err
	}
	var sc streamChunk
	if null {
		// The map rewrite read no member of a null chunk: no choices, no
		// usage — nothing for the client.
		sc.skip = true
		return sc, nil
	}
	choices, usage, timings := topMembers(obj)
	switch {
	case choices == nil || isNull(choices):
		choices = emptyArray
	case !hasKind(choices, kindArray):
		return streamChunk{}, errNoChoices
	}
	if err := checkChoiceKinds(choices); err != nil {
		return streamChunk{}, err
	}
	out := growBy(dst[:0], rewriteBufferLen(len(upstream), id, alias))
	out = append(out, `data: {"choices":`...)
	out, n := appendChunkChoices(out, choices, reasoningOff, &sc, roles)
	readStreamUsage(usage, timings, &sc)
	usageObj := usage != nil && hasKind(usage, kindObject)
	// Nothing for the client: the usage chunk without include_usage, an
	// empty chunk that is no usage chunk (client code reads choices[0] of
	// every chunk), and a chunk the reasoning strip emptied.
	if n == 0 && (!usageObj || !includeUsage) {
		sc.skip = true
		return sc, nil
	}
	out = append(growBy(out, 64), `,"created":`...)
	out = strconv.AppendInt(out, created, 10)
	out = append(out, `,"id":`...)
	out = appendString(out, id)
	out = append(out, `,"model":`...)
	out = appendString(out, alias)
	out = append(growBy(out, 64), `,"object":"chat.completion.chunk","service_tier":`...)
	out = appendString(out, serviceTierOf(lane))
	if usage != nil && includeUsage {
		out = append(growBy(out, 16), `,"usage":`...)
		if usageObj {
			out = appendUsage(out, usage)
		} else {
			out = append(out, nullValue...)
		}
	}
	sc.out = append(growBy(out, 3), "}\n\n"...)
	return sc, nil
}

// emptyArray is the choices of a chunk without any.
var emptyArray = []byte("[]")
