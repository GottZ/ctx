package oai

// The stream relay (wave A03-W10, design/03 §4.10 no. 1, I-03-7): the bounded
// buffer between a /v1 stream's upstream reader and its client writer. The
// lease of a stream ends at the upstream's end, never at the client's drain;
// the client writer drains the buffer on its own.

// RelayRecordHeader is the size of the length header the relay buffer stores
// in front of every frame (a little-endian uint32) — the bytes one frame
// takes in the buffer beyond its own. config's V39 reads it: the buffer must
// hold one frame at gateway.max_stream_frame_bytes plus this header.
const RelayRecordHeader = 4
