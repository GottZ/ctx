package oai

// frame writes chunk as one data frame — the stream writer's frame before
// FOLGE-S, when the writer framed the chunk itself; the route's writer
// takes the line the rewrite framed (frameLine). The writer tests keep
// their chunks and wire bytes through it.
func (s *streamWriter) frame(chunk []byte) error {
	line := make([]byte, 0, len(chunk)+8)
	return s.frameLine(append(append(append(line, "data: "...), chunk...), "\n\n"...))
}
