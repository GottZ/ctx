// W02-8: the MCP blob tools — blob_store over the shared blob write core
// (blob_core.go), blob_fetch as a RANGED read.
//
// Why they exist: a provider that writes evidence over MCP had no way to put a
// payload anywhere but into a block. The REST route /api/blob/store was the
// only blob write path, which meant a second credential and a second
// authorisation path for the same principal — the shape BP-6 warns about. The
// tools close that by running the REST handler's own gates, not by restating
// them (blob_core.go), and by staging for confirm_writes keys exactly as the
// store tool does (N-28): a blob must not be the hole in a key's write policy.
//
// Why the read is ranged: /api/blob/fetch answers the WHOLE blob, base64, in
// one piece (blob.go), and GetBlobByID selects the whole data column. Handing
// a 50 MB payload to a model context is not a large answer, it is the end of
// the context. offset/length address the stored (uncompressed) bytes, so a
// caller that knows its own layout can pull one record instead of the file.
package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Range bounds of ONE blob_fetch call.
//
// Compiled-in rather than a settings key, and the reason is what the numbers
// bound: not an operator policy over storage (that is pool.blob_*), but how
// much of a payload may enter one model context in one answer. That ceiling is
// a property of the protocol surface — no tenant has a reason to hold a
// different one, and a per-tenant value would only ever be a way to lift a
// guard that costs nothing to work within (the caller reads twice).
//
// The default is the working size of a drill-down; the maximum is the point at
// which the answer stops being a drill-down. Over the maximum the call is
// REFUSED rather than clamped: a caller that believes it read a whole range
// and silently got a prefix builds a wrong index out of it, and nothing later
// can tell that apart from a correct one.
const (
	blobFetchDefaultLength = 256 * 1024  // 262144
	blobFetchMaxLength     = 1024 * 1024 // 1048576
)

type blobStoreInput struct {
	Category string `json:"category" jsonschema:"blob category (upserts on category+title+scope)"`
	Title    string `json:"title" jsonschema:"blob title (upserts on category+title+scope)"`
	Filename string `json:"filename" jsonschema:"file name of the payload"`
	MimeType string `json:"mime_type" jsonschema:"media type of the payload (e.g. application/x-ndjson)"`
	// Exactly one of File / Text carries the payload. Two fields rather than
	// one because both callers are real: a binary payload has to be base64
	// (the REST route's encoding, mirrored verbatim down to the
	// invalid_encoding verdict), while a text payload — the NDJSON case this
	// surface exists for — would otherwise pay a third of its size in
	// transport for nothing.
	File string `json:"file,omitempty" jsonschema:"payload as base64 (use this for binary data)"`
	Text string `json:"text,omitempty" jsonschema:"payload as UTF-8 text (alternative to file; exactly one of the two)"`
	// NO scope field — decision D4, the same line the block `store` tool
	// holds: an MCP writer writes its key's home scope, and a foreign-scope
	// MCP write is a decision about the write surface, not a property of this
	// wave. The shared core keeps its Scope parameter (REST fills it); this
	// arm passes the empty string, which resolves to ar.HomeScope.
	Tags     []string       `json:"tags,omitempty" jsonschema:"optional tags for filtering"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object"`
}

type blobFetchInput struct {
	ID       string `json:"id,omitempty" jsonschema:"blob UUID (or give category+title)"`
	Category string `json:"category,omitempty" jsonschema:"blob category (with title, as an alternative to id)"`
	Title    string `json:"title,omitempty" jsonschema:"blob title (with category, as an alternative to id)"`
	MetaOnly bool   `json:"meta_only,omitempty" jsonschema:"return metadata only, no payload"`
	Offset   int    `json:"offset,omitempty" jsonschema:"byte offset into the stored payload (default 0)"`
	Length   int    `json:"length,omitempty" jsonschema:"bytes to return (default 262144, max 1048576; a larger value is refused, not clamped)"`
}

// registerBlobTools registers the two blob tools. Registration is
// unconditional, like every other tool file's: the tool list must not depend
// on the calling key, or a client's tool cache would differ per principal.
func registerBlobTools(server *mcp.Server, cfg MCPConfig) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "blob_store",
		Description: "Store a binary or text payload as a blob. Upserts on (category, title, scope). Runs the same gates as POST /api/blob/store; a key with the confirm_writes capability gets the call STAGED instead of executed.",
	}, mcpBlobStoreHandler(cfg))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "blob_fetch",
		Description: "Read a blob by id (or category+title). Reads a BYTE RANGE by default (offset/length over the stored payload) so a large blob can be drilled into instead of pulled whole; meta_only returns metadata alone.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpBlobFetchHandler(cfg))
}

// mcpBlobStoreHandler is the MCP arm of the blob write path. It owns exactly
// one thing the REST arm does not: which of the two payload fields carries the
// bytes. Everything after that is blob_core.go, in the same order.
func mcpBlobStoreHandler(cfg MCPConfig) mcp.ToolHandlerFor[blobStoreInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input blobStoreInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return classUnauthorized.errResult("unauthorized: no resolved tenant identity"), nil, nil
		}

		data, rej := blobPayloadOf(input)
		if rej != nil {
			return errResultReject(rej), nil, nil
		}

		// Scope stays EMPTY (D4): the core resolves it to ar.HomeScope, which
		// is byte-identical to what the block store tool writes. The field is
		// absent from the tool, so there is nothing here to pass through.
		in := blobWriteInput{
			Category: input.Category,
			Title:    input.Title,
			Filename: input.Filename,
			MimeType: input.MimeType,
			Data:     data,
			Tags:     input.Tags,
			Metadata: input.Metadata,
		}

		// Staging branch (N-28), mirroring mcpStoreHandler: a confirm_writes
		// key never writes directly, on any surface it can reach. Handler-level
		// like its block twin — internal writers go through store.UpsertBlob
		// and must never self-stage.
		if ar.ConfirmWrites {
			return mcpStageBlobStore(ctx, cfg, ar, in)
		}

		blob, blobRej := executeBlobWrite(ctx, cfg.Pool, cfg.Cfg, ar, in, RequestIDFromContext(ctx))
		if blobRej != nil {
			return errResultReject(blobRej.writeReject), nil, nil
		}

		// JSON, not prose: the caller of this tool is a provider that has to
		// keep the id (and, for a self-indexing payload, the size) to address
		// the blob again.
		out, _ := json.Marshal(map[string]any{
			"id": blob.ID, "category": blob.Category, "title": blob.Title,
			"filename": blob.Filename, "mime_type": blob.MimeType,
			"file_size": blob.FileSize, "checksum": blob.Checksum, "scope": blob.Scope,
		})
		return &mcp.CallToolResult{Content: []mcp.Content{textContent(string(out))}}, nil, nil
	}
}

// blobPayloadOf resolves the payload field and runs the two transport-side
// steps of the core (fields + name caps, then decode + payload caps) in the
// REST handler's order.
//
// Exactly one of file/text: neither is a missing field, both is a request that
// names two different payloads for one blob — answering it would mean picking
// one silently, which is the class of behaviour this surface exists to avoid.
func blobPayloadOf(input blobStoreInput) ([]byte, *writeReject) {
	hasFile, hasText := input.File != "", input.Text != ""
	if hasFile && hasText {
		return nil, classMissingFields.reject("Provide exactly one of 'file' (base64) or 'text' — not both")
	}
	if rej := blobFieldGates(hasFile || hasText, input.Filename, input.Category, input.Title, input.MimeType); rej != nil {
		return nil, rej
	}
	if hasText {
		return blobTextPayload(input.Text)
	}
	return decodeBlobFile(input.File)
}

// mcpStageBlobStore is the blob arm of the stage-then-confirm dance: run every
// gate the direct path would run (a staged card is a promise that the confirm
// will succeed, D1-M2), canonicalize the POST-GATE intent, stage it.
//
// The answer is IsError=true BY DESIGN (D3-C3) and deliberately UNCODED: the
// gates PASSED, so this is not a rejection — the absence of a code is how a
// client tells a staged write from a refused one.
func mcpStageBlobStore(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, in blobWriteInput) (*mcp.CallToolResult, any, error) {
	reqID := RequestIDFromContext(ctx)

	stageMax := 0
	var ttl time.Duration
	if cfg.Cfg != nil {
		snap := cfg.Cfg.SnapshotForRequest(ctx)
		stageMax = snap.Pool.BlobStageMaxBytes
		ttl = snap.Writes.ConfirmTTL
	}

	// The staging cap, BEFORE the scope gate and the budget: it is a property
	// of the payload, like the size caps it stands beside, and a payload that
	// can never be staged should cost neither a scope lookup nor budget.
	//
	// Over the cap the call is REFUSED. Falling back to a direct write would
	// hand this key exactly the bypass its confirm_writes flag exists to
	// close — the provider degrades to writing less, not to writing unchecked.
	if stageMax <= 0 {
		return classSizeCap.errResult(
			"blob staging is disabled (pool.blob_stage_max_bytes = 0) and this key requires write confirmation " +
				"(confirm_writes) — it cannot store blobs until an operator sets a staging cap"), nil, nil
	}
	if len(in.Data) > stageMax {
		return classSizeCap.errResult(fmt.Sprintf(
			"payload of %d bytes exceeds pool.blob_stage_max_bytes (%d): this key requires write confirmation "+
				"(confirm_writes), and a staged payload is held server-side until it is confirmed or expires — "+
				"store a smaller payload",
			len(in.Data), stageMax)), nil, nil
	}

	writeScope, _, rej := blobWriteGate(ctx, cfg.Pool, cfg.Cfg, ar, in, reqID)
	if rej != nil {
		return errResultReject(rej), nil, nil
	}
	// blobWriteGate booked the write INTENT (the logID above is deliberately
	// dropped): at stage time no blob exists, and an unconfirmed stage may
	// never produce one, so there is nothing to attribute the row to. Charging
	// at intent — not at confirm — is the same semantics the block stage path
	// carries, and it is why executeConfirm books nothing: a confirmed write is
	// already paid for.

	cw := store.CanonicalWrite{
		Op:       store.OpBlobStore,
		Scope:    writeScope,
		Category: in.Category,
		Title:    in.Title,
		Tags:     in.Tags,
		Metadata: in.Metadata,
		Filename: in.Filename,
		MimeType: in.MimeType,
		Data:     in.Data,
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged blob write failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: cannot canonicalize payload"), nil, nil
	}

	pw, err := store.StagePendingWrite(ctx, cfg.Pool, ar.ApiKeyID, writeScope, store.OpBlobStore, "mcp", canonical, hash, ttl)
	if err != nil {
		slog.Error("mcp: stage pending blob write failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: could not persist the staged write"), nil, nil
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	return errResult(fmt.Sprintf(
		"STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"+
			"payload_hash: %s\n"+
			"bytes: %d\n"+
			"expires: %s\n"+
			"To execute this exact write, call the 'confirm' tool with this payload_hash. "+
			"The server holds the authoritative payload; confirming cannot alter it.",
		hash, len(in.Data), expiry)), nil, nil
}

// mcpBlobFetchHandler reads a blob, by default as a RANGE.
//
// Rejections here stay uncoded: errcode.go's vocabulary is the WRITE surfaces'
// (its scope note says so), and a read handler that started emitting write
// codes would widen that boundary as a side effect of this wave.
func mcpBlobFetchHandler(cfg MCPConfig) mcp.ToolHandlerFor[blobFetchInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input blobFetchInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		if input.ID == "" && (input.Category == "" || input.Title == "") {
			return errResult("Provide 'id' or both 'category' and 'title'"), nil, nil
		}
		if input.Offset < 0 || input.Length < 0 {
			return errResult(fmt.Sprintf("offset (%d) and length (%d) must not be negative", input.Offset, input.Length)), nil, nil
		}
		if input.Length > blobFetchMaxLength {
			return errResult(fmt.Sprintf(
				"length %d exceeds the maximum of %d bytes for one blob_fetch range — read the blob in several ranges "+
					"(the range is refused rather than shortened, so a short answer always means the blob ended)",
				input.Length, blobFetchMaxLength)), nil, nil
		}
		length := input.Length
		if length == 0 {
			length = blobFetchDefaultLength
		}

		// A category+title read resolves to the id first and then takes the
		// same ranged path — one range implementation, not one per selector.
		blob, err := blobFetchTarget(ctx, cfg, ar, input, length)
		if err != nil {
			slog.Error("mcp blob_fetch: query error", "error", err, "request_id", RequestIDFromContext(ctx))
			return errResult(fmt.Sprintf("blob_fetch failed: %v", err)), nil, nil
		}
		if blob == nil {
			return errResult("blob not found"), nil, nil
		}

		out := map[string]any{
			"id": blob.ID, "category": blob.Category, "title": blob.Title,
			"filename": blob.Filename, "mime_type": blob.MimeType,
			"file_size": blob.FileSize, "checksum": blob.Checksum,
			"tags": blob.Tags, "metadata": blob.Metadata, "scope": blob.Scope,
			"created_at": blob.CreatedAt, "updated_at": blob.UpdatedAt,
		}
		if !input.MetaOnly {
			out["offset"] = input.Offset
			out["length"] = len(blob.Data)
			// Text when the window IS text, base64 otherwise. The encoding is
			// named in the answer rather than inferred from the mime type: a
			// range can cut a multi-byte rune in half, and a caller must be
			// able to tell "this is the text" from "this is bytes".
			if utf8.Valid(blob.Data) {
				out["encoding"] = "text"
				out["content"] = string(blob.Data)
			} else {
				out["encoding"] = "base64"
				out["file"] = base64.StdEncoding.EncodeToString(blob.Data)
			}
		}

		data, _ := json.MarshalIndent(out, "", "  ")
		return &mcp.CallToolResult{Content: []mcp.Content{textContent(string(data))}}, nil, nil
	}
}

// blobFetchTarget resolves the addressed blob and applies the read form:
// metadata only, or a ranged read of the stored payload.
func blobFetchTarget(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, input blobFetchInput, length int) (*store.Blob, error) {
	id := input.ID
	if id == "" {
		meta, err := store.GetBlobByCategoryTitle(ctx, cfg.Pool, input.Category, input.Title, ar.ReadScopes, true)
		if err != nil || meta == nil {
			return meta, err
		}
		if input.MetaOnly {
			return meta, nil
		}
		id = meta.ID
	}
	if input.MetaOnly {
		return store.GetBlobByID(ctx, cfg.Pool, id, ar.ReadScopes, true)
	}
	return store.GetBlobRangeByID(ctx, cfg.Pool, id, ar.ReadScopes, input.Offset, length)
}
