// Package recblocks splits one context_llm_recordings row — a request a
// recording model consumer sent through the gateway and the answer its
// client got (internal/llmlog recordings.go) — into addressable blocks for
// an admin view: the messages of a chat request with their content parts,
// tool calls and reasoning, the inputs of an embeddings request, the query
// and documents of a rerank request, and the answer per choice, a streamed
// one assembled from its relayed lines.
//
// The decomposition is pure and deterministic: no database, no clock, the
// standard library only; the same Input gives a byte-identical Result. It
// never repairs a body: one that is not valid UTF-8, or not one JSON value,
// is one raw block holding its bytes unchanged. It never modifies or keeps
// its input: each body is copied once, and every block's content is that
// copy, a sub-slice of it, or a string decoded from it.
//
// A decoded string is what encoding/json yields for it — the bytes between
// the quotes when the string has no escape. That differs from a byte-exact
// reading of the body in one case: a lone UTF-16 surrogate escape (a high
// or low surrogate without its pair) decodes to U+FFFD. The side addresses
// "request" and "response" (Lookup) read the body byte for byte. An object
// that names a key twice is read as encoding/json reads it: the last
// occurrence wins; the earlier ones stay in the body.
package recblocks

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The sides of a row and their addresses.
const (
	sideRequest  = "request"
	sideResponse = "response"
)

// Side states.
const (
	statePresent = "present"
	stateCleared = "cleared"
	stateEmpty   = "empty"
	stateNone    = "none"
	statePending = "pending"
)

// Side formats.
const (
	formatJSON    = "json"
	formatNotJSON = "not_json"
	formatNotUTF8 = "not_utf8"
)

// recordingOpen is the state of a row whose outcome is not recorded yet
// (llmlog.RecordingOpen).
const recordingOpen = "open"

// Ops as context_llm_recordings.op stores them (llmlog.GatewayOp*).
const (
	opChat   = "chat"
	opEmbed  = "embed"
	opRerank = "rerank"
)

// Block kinds.
const (
	kindText       = "text"
	kindImage      = "image"
	kindAudio      = "audio"
	kindFile       = "file"
	kindToolCall   = "tool_call"
	kindToolResult = "tool_result"
	kindJSON       = "json"
	kindTokens     = "tokens"
	kindError      = "error"
	kindRaw        = "raw"
)

// Input is one context_llm_recordings row as the decomposition reads it.
// Op values as stored in context_llm_recordings.op (llmlog.GatewayOp*):
// "chat", "embed", "rerank", or "" (no route parsed it).
type Input struct {
	Op            string
	Path          string // request path, e.g. /v1/chat/completions, /v1/embeddings, /v1/rerank, /v2/rerank, /v1/models
	Stream        bool   // the row's stream flag (NULL -> false)
	State         string // "open" | "done"
	Status        int    // HTTP status the client got; 0 = NULL (gateway wrote nothing)
	Request       []byte // request_body; nil together with RequestNull
	RequestNull   bool   // request_body IS NULL (retention cleared it)
	Response      []byte // response_body
	ResponseNull  bool   // response_body IS NULL
	ResponseBytes int64  // response_bytes column (size kept after the retention)
}

// Side is one side of a row: its address, its state and, when present, the
// format and size of its body.
type Side struct {
	Address string `json:"address"`          // "request" | "response"
	State   string `json:"state"`            // request: present|cleared|empty ; response: present|cleared|none|pending
	Format  string `json:"format,omitempty"` // json | not_json | not_utf8 (only when present)
	Bytes   int64  `json:"bytes"`
	// stream answers only (response side, 2xx, stream=true):
	StreamLines         int `json:"stream_lines,omitempty"`
	StreamLinesUnparsed int `json:"stream_lines_unparsed,omitempty"`
}

// Block is one addressable piece of a row.
type Block struct {
	Address      string `json:"address"`
	Side         string `json:"side"` // request | response
	Kind         string `json:"kind"` // text | image | audio | file | tool_call | tool_result | json | tokens | error | raw
	Role         string `json:"role,omitempty"`
	Name         string `json:"name,omitempty"` // function/tool name of a tool_call; message "name"
	ToolCallID   string `json:"tool_call_id,omitempty"`
	MediaType    string `json:"media_type,omitempty"` // image/audio/file: from the data URL or format field
	Bytes        int    `json:"bytes"`                // len(Content())
	UTF8         bool   `json:"utf8"`                 // utf8.Valid(Content())
	Translatable bool   `json:"translatable"`
	// Inline is false for content a viewer must not get inline by default:
	// an image/audio/file payload (data URL or base64). It is still readable
	// through the window read of its address.
	Inline  bool `json:"inline"`
	content []byte
}

// Content returns the block's bytes. They never share memory with the
// Input — they are the decomposition's own copy of a body, a sub-slice of
// it, or a decoded string — but the blocks of one Result may share them.
func (b Block) Content() []byte { return b.content }

// WithoutContent is the block without its bytes: what a caller keeps of it
// once it has cut what it needs, so the copy of the body the bytes point
// into is not held with it.
func (b Block) WithoutContent() Block {
	b.content = nil
	return b
}

// Result is a row's decomposition.
type Result struct {
	Request  Side
	Response Side
	Blocks   []Block // request blocks first, then response blocks, each in document order
	// Total is the number of blocks of the row — len(Blocks) for
	// Decompose, every block the walk met for DecomposeWindow and Find.
	Total int
}

// Decompose splits the row in into its sides and all its blocks.
func Decompose(in Input) Result {
	return decompose(in, keepAll{})
}

// DecomposeWindow is Decompose keeping only the blocks [from, from+limit)
// of the row's order: the walk meets every block (Total counts them), but
// only those of the window are kept — a body of millions of tiny elements
// costs the walk, never a block list of its size.
func DecomposeWindow(in Input, from, limit int) Result {
	return decompose(in, keepWindow{from: from, to: from + limit})
}

// Find is the block at address, decomposing in and keeping only that
// block; for the side addresses "request" and "response" Lookup's
// synthetic raw block. ok=false: no such address, or the side is
// cleared/absent (the Result's Side.State tells which).
func Find(in Input, address string) (Result, Block, bool) {
	r := decompose(in, keepAddress(address))
	b, ok := Lookup(r, in, address)
	return r, b, ok
}

// keeper decides which blocks a decomposition keeps (n: the block's
// position in the row's order).
type keeper interface {
	keep(n int, address string) bool
}

type keepAll struct{}

func (keepAll) keep(int, string) bool { return true }

type keepWindow struct{ from, to int }

func (k keepWindow) keep(n int, _ string) bool { return n >= k.from && n < k.to }

type keepAddress string

func (k keepAddress) keep(_ int, address string) bool { return address == string(k) }

func decompose(in Input, k keeper) Result {
	res := Result{Request: requestSide(in), Response: responseSide(in)}
	b := &builder{side: sideRequest, keeper: k}
	if res.Request.State == statePresent {
		res.Request.Format = b.request(in, bytes.Clone(in.Request))
	}
	b.side = sideResponse
	if res.Response.State == statePresent {
		b.response(in, bytes.Clone(in.Response), &res.Response)
	}
	res.Blocks, res.Total = b.blocks, b.n
	if res.Blocks == nil {
		res.Blocks = []Block{}
	}
	return res
}

// Lookup returns the block with this exact address, or for the side
// addresses "request"/"response" a synthetic raw block over the whole body
// (Kind "raw", content = the whole body, Translatable = utf8 valid &&
// present). ok=false: no such address, or the side is cleared/absent
// (caller tells which by Side.State).
func Lookup(r Result, in Input, address string) (Block, bool) {
	for _, b := range r.Blocks {
		if b.Address == address {
			return b, true
		}
	}
	switch {
	case address == sideRequest && r.Request.State == statePresent && !in.RequestNull && len(in.Request) > 0:
		return rawBlock(sideRequest, bytes.Clone(in.Request)), true
	case address == sideResponse && r.Response.State == statePresent && !in.ResponseNull && len(in.Response) > 0:
		return rawBlock(sideResponse, bytes.Clone(in.Response)), true
	}
	return Block{}, false
}

// requestSide is the request side's state: cleared by the retention, empty
// (nothing was read), or present.
func requestSide(in Input) Side {
	s := Side{Address: sideRequest}
	switch {
	case in.RequestNull:
		s.State = stateCleared
	case len(in.Request) == 0:
		s.State = stateEmpty
	default:
		s.State, s.Bytes = statePresent, int64(len(in.Request))
	}
	return s
}

// responseSide is the response side's state: present when the row carries
// answer bytes (an open row too), pending for an open row without them,
// cleared by the retention (the size kept), or none.
func responseSide(in Input) Side {
	s := Side{Address: sideResponse}
	switch {
	case !in.ResponseNull && len(in.Response) > 0:
		s.State, s.Bytes = statePresent, int64(len(in.Response))
	case in.State == recordingOpen:
		s.State = statePending
	case in.ResponseNull && in.ResponseBytes > 0:
		s.State, s.Bytes = stateCleared, in.ResponseBytes
	default:
		s.State = stateNone
	}
	return s
}

// rawBlock is the block of a whole side body, its bytes as they are.
func rawBlock(side string, body []byte) Block {
	ok := utf8.Valid(body)
	return Block{Address: side, Side: side, Kind: kindRaw, Bytes: len(body), UTF8: ok,
		Translatable: ok, Inline: true, content: body}
}

// builder collects the blocks of a row, side by side; keeper decides which
// it keeps, n counts every block met.
type builder struct {
	side   string
	blocks []Block
	keeper keeper
	n      int
}

// met counts a block and reports whether the builder keeps it.
func (b *builder) met(address string) bool {
	n := b.n
	b.n++
	return b.keeper == nil || b.keeper.keep(n, address)
}

// add appends blk with content: its side, size and UTF-8 flag come from
// here, and nothing that is not valid UTF-8 is translatable.
func (b *builder) add(blk Block, content []byte) {
	if !b.met(blk.Address) {
		return
	}
	blk.Side = b.side
	blk.content = content
	blk.Bytes = len(content)
	blk.UTF8 = utf8.Valid(content)
	blk.Translatable = blk.Translatable && blk.UTF8
	b.blocks = append(b.blocks, blk)
}

// text appends a text-like block (text, tool_result, tool_call, error):
// translatable and inline.
func (b *builder) text(blk Block, content []byte) {
	blk.Translatable, blk.Inline = true, true
	b.add(blk, content)
}

// jsonBlock appends a block of raw JSON bytes.
func (b *builder) jsonBlock(address string, content []byte, translatable bool) {
	b.add(Block{Address: address, Kind: kindJSON, Translatable: translatable, Inline: true}, content)
}

// raw appends the one block of a body that is not valid UTF-8 or not valid
// JSON: the side address, the bytes unchanged.
func (b *builder) raw(body []byte) {
	if b.met(b.side) {
		b.blocks = append(b.blocks, rawBlock(b.side, body))
	}
}

// bodyFormat is the format of a valid UTF-8 body.
func bodyFormat(body []byte) string {
	if json.Valid(body) {
		return formatJSON
	}
	return formatNotJSON
}

// route is the route rule a row falls under: its op, or — when no route
// parsed it — the one its path names; "" for none.
func route(in Input) string {
	switch in.Op {
	case opChat, opEmbed, opRerank:
		return in.Op
	case "":
		return routeOfPath(in.Path)
	}
	return ""
}

func routeOfPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return opChat
	case strings.HasSuffix(path, "/embeddings"):
		return opEmbed
	case strings.HasSuffix(path, "/rerank"):
		return opRerank
	}
	return ""
}
